package signal

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/qf-studio/studio-sdk/sdk/core"
)

// pollQuestionMax is Signal's poll-question length cap; an over-long question
// is rejected outright, so the text is trimmed rather than risking a failed send.
const pollQuestionMax = 140

// refusedVoteText answers a vote from outside the approver allowlist.
const refusedVoteText = "Vote ignored — not an approver."

// bridge implements core.ChatBridge for Signal over signal-cli-rest-api:
// a websocket receive stream inbound, HTTP endpoints outbound.
//
// Signal has no inline buttons and cannot edit sent messages. Buttons render as
// a native poll (the vote binds to the message and the voter is identifiable),
// and Edit posts a fresh message instead of mutating one.
type bridge struct {
	sender    *Sender
	receiver  *Receiver
	deps      core.ChatDeps
	allow     *GroupAllowlist
	approvers []string
	selfUUID  string
	logger    *slog.Logger

	initErr error

	mu    sync.Mutex
	polls map[int64]pollMeta
	// seenVoteRevision tracks each voter's latest vote revision per poll. Signal
	// lets an open poll's answer be changed and redelivers frames; without the
	// revision guard a stale redelivery could overturn a later decision.
	seenVoteRevision map[string]int
	// refusedVotes remembers which (voter, poll) pairs were already told they may
	// not approve, so repeated revisions cannot be turned into a message flood.
	refusedVotes map[string]struct{}
}

// pollMeta binds a poll pilot posted to the button payloads its options carry.
type pollMeta struct {
	recipient string
	data      []string
	actionIDs []string
}

// Start blocks streaming received frames until ctx is cancelled.
func (b *bridge) Start(ctx context.Context) error {
	if b.initErr != nil {
		return b.initErr
	}
	return b.receiver.Run(ctx, func(f Frame) {
		b.processFrame(ctx, f.Envelope)
	})
}

// Send delivers an outbound message. With Buttons it creates a poll whose
// options are the button labels; the returned ref's MessageID is the poll or
// message timestamp.
func (b *bridge) Send(ctx context.Context, m core.OutboundMessage) (core.MessageRef, error) {
	recipient := GroupRecipient(m.ChannelID)

	if len(m.Buttons) > 0 {
		question := truncateRunes(oneLine(plainText(m.Text)), pollQuestionMax)
		options := make([]string, 0, len(m.Buttons))
		data := make([]string, 0, len(m.Buttons))
		actionIDs := make([]string, 0, len(m.Buttons))
		for _, btn := range m.Buttons {
			options = append(options, btn.Label)
			data = append(data, btn.Data)
			actionIDs = append(actionIDs, btn.ActionID)
		}

		ts, err := b.sender.CreatePoll(ctx, recipient, question, options, false)
		if err != nil {
			return core.MessageRef{}, err
		}
		b.mu.Lock()
		b.polls[ts] = pollMeta{recipient: recipient, data: data, actionIDs: actionIDs}
		b.mu.Unlock()
		return core.MessageRef{ChannelID: m.ChannelID, MessageID: strconv.FormatInt(ts, 10)}, nil
	}

	stamps, err := b.sender.SendChunked(ctx, recipient, m.Text)
	if err != nil {
		return core.MessageRef{}, err
	}
	msgID := ""
	if len(stamps) > 0 {
		msgID = strconv.FormatInt(stamps[0], 10)
	}
	return core.MessageRef{ChannelID: m.ChannelID, MessageID: msgID}, nil
}

// Edit posts a fresh message: signal-cli cannot edit a sent message, so the
// update arrives as a new message rather than an in-place mutation.
func (b *bridge) Edit(ctx context.Context, ref core.MessageRef, text string) error {
	_, err := b.sender.SendChunked(ctx, GroupRecipient(ref.ChannelID), text)
	return err
}

// Ack closes the poll behind a callback, which is what makes the answer final —
// Signal allows an open poll's vote to be changed.
func (b *bridge) Ack(ctx context.Context, callbackID string) error {
	recipient, ts, err := parseCallbackID(callbackID)
	if err != nil {
		return err
	}
	if err := b.sender.ClosePoll(ctx, recipient, ts); err != nil {
		return err
	}
	b.forgetPoll(ts)
	return nil
}

// forgetPoll drops the poll and every per-voter entry keyed to it; without the
// per-voter sweep those maps grow for the lifetime of the process.
func (b *bridge) forgetPoll(ts int64) {
	suffix := "|" + strconv.FormatInt(ts, 10)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.polls, ts)
	for k := range b.seenVoteRevision {
		if strings.HasSuffix(k, suffix) {
			delete(b.seenVoteRevision, k)
		}
	}
	for k := range b.refusedVotes {
		if strings.HasSuffix(k, suffix) {
			delete(b.refusedVotes, k)
		}
	}
}

func (b *bridge) processFrame(ctx context.Context, e Envelope) {
	if vote, ok := VoteFrom(b.allow, e); ok {
		b.handleVote(ctx, vote)
		return
	}

	if e.Kind() != KindData || !b.allow.Allows(e) {
		return
	}
	if b.selfUUID != "" && e.SourceUUID == b.selfUUID {
		return
	}
	text := e.Text()
	if text == "" {
		return
	}

	channelID := e.DataMessage.GroupInfo.GroupID
	messageID := strconv.FormatInt(e.Timestamp, 10)
	sender := core.Identity{UserID: e.SourceUUID, DisplayName: e.SourceName}

	var ev core.MessageEvent
	if strings.HasPrefix(text, "/") {
		parts := strings.Fields(text)
		ev = core.MessageEvent{
			Action:    "command",
			MessageID: messageID,
			ChannelID: channelID,
			Command:   parts[0],
			Args:      parts[1:],
			Sender:    sender,
		}
	} else {
		ev = core.MessageEvent{
			Action:    "message",
			MessageID: messageID,
			ChannelID: channelID,
			Text:      sanitizeMessageText(text, b.logger),
			Sender:    sender,
		}
	}

	if err := b.deps.Handler.HandleMessage(ctx, ev); err != nil {
		b.logger.Warn("signal: handler error", slog.Any("error", err))
	}
}

func (b *bridge) handleVote(ctx context.Context, vote Vote) {
	if b.selfUUID != "" {
		if vote.PollAuthorID != b.selfUUID {
			b.logger.Debug("signal: vote on a poll pilot did not author ignored",
				slog.Int64("poll", vote.PollTimestamp),
				slog.String("author", vote.PollAuthorID))
			return
		}
		if vote.VoterID == b.selfUUID {
			b.logger.Debug("signal: own vote ignored", slog.Int64("poll", vote.PollTimestamp))
			return
		}
	}

	revKey := voteKey(vote.VoterID, vote.PollTimestamp)
	b.mu.Lock()
	meta, known := b.polls[vote.PollTimestamp]
	// Presence, not the zero value: a first vote carries revision 0.
	seen, replayed := b.seenVoteRevision[revKey]
	stale := known && replayed && seen >= vote.Revision
	if known && !stale {
		b.seenVoteRevision[revKey] = vote.Revision
	}
	b.mu.Unlock()

	if !known || stale || len(vote.OptionIndexes) == 0 {
		return
	}
	idx := vote.OptionIndexes[0]
	if idx < 0 || idx >= len(meta.data) {
		return
	}

	if !b.mayApprove(vote.VoterID) {
		b.logger.Warn("signal: vote from non-approver ignored",
			slog.Int64("poll", vote.PollTimestamp),
			slog.String("voter", vote.VoterID))
		b.mu.Lock()
		_, told := b.refusedVotes[revKey]
		if !told {
			if b.refusedVotes == nil {
				b.refusedVotes = make(map[string]struct{})
			}
			b.refusedVotes[revKey] = struct{}{}
		}
		b.mu.Unlock()
		if told {
			return
		}
		// The poll stays open so an approver can still decide it.
		if _, err := b.sender.SendText(ctx, meta.recipient, refusedVoteText); err != nil {
			b.logger.Warn("signal: reporting refused vote failed", slog.Any("error", err))
		}
		return
	}

	ev := core.MessageEvent{
		Action:     "callback",
		CallbackID: formatCallbackID(meta.recipient, vote.PollTimestamp),
		Data:       meta.data[idx],
		ChannelID:  vote.GroupID,
		MessageID:  strconv.FormatInt(vote.PollTimestamp, 10),
		Sender:     core.Identity{UserID: vote.VoterID},
	}

	if err := b.deps.Handler.HandleMessage(ctx, ev); err != nil {
		b.logger.Warn("signal: callback handler error", slog.Any("error", err))
	}
}

// mayApprove reports whether a voter is allowed to decide a poll. An empty
// approver list leaves group membership as the boundary.
func (b *bridge) mayApprove(voter string) bool {
	if len(b.approvers) == 0 {
		return true
	}
	for _, id := range b.approvers {
		if id == voter {
			return true
		}
	}
	return false
}

func voteKey(voterID string, pollTS int64) string {
	return voterID + "|" + strconv.FormatInt(pollTS, 10)
}

// formatCallbackID encodes everything Ack needs to close the poll, so
// acknowledgement works without bridge state surviving in between.
func formatCallbackID(recipient string, ts int64) string {
	return recipient + "|" + strconv.FormatInt(ts, 10)
}

func parseCallbackID(callbackID string) (string, int64, error) {
	i := strings.LastIndexByte(callbackID, '|')
	if i < 0 {
		return "", 0, fmt.Errorf("signal: malformed callback ID %q", callbackID)
	}
	ts, err := strconv.ParseInt(callbackID[i+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("signal: malformed callback ID %q: %w", callbackID, err)
	}
	return callbackID[:i], ts, nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
