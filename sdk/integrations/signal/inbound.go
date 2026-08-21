package signal

import (
	"encoding/base64"
	"strings"
)

// groupRecipientPrefix marks the send-side form of a group identifier.
const groupRecipientPrefix = "group."

// GroupRecipient converts a received group ID into the form the send endpoints
// expect. The two are different encodings of the same group — an inbound
// envelope carries the raw ID, while /v2/send and /v1/polls want
// "group." + base64 of it — so passing a received ID straight back as a
// recipient silently addresses nothing.
func GroupRecipient(groupID string) string {
	if groupID == "" || strings.HasPrefix(groupID, groupRecipientPrefix) {
		return groupID
	}
	return groupRecipientPrefix + base64.StdEncoding.EncodeToString([]byte(groupID))
}

// GroupID converts a send-side recipient back to the received form. Group
// identifiers appear in two encodings, and the allowlist must compare in one:
// GET /v1/groups hands an operator "group.<base64>" while envelopes carry the
// raw ID, so a config copied from the API would otherwise match nothing and
// silently refuse every command.
func GroupID(recipient string) string {
	if !strings.HasPrefix(recipient, groupRecipientPrefix) {
		return recipient
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(recipient, groupRecipientPrefix))
	if err != nil {
		return recipient
	}
	return string(raw)
}

// GroupAllowlist decides which frames pilot may act on. Group membership is the
// authorization boundary — anyone in a group can vote in its polls, so admitting a
// group grants its members approval authority. It is therefore default-deny:
// unknown groups and direct messages are refused.
type GroupAllowlist struct {
	ids map[string]struct{}
}

// NewGroupAllowlist admits exactly the given group IDs. An empty allowlist admits
// nothing, which is the safe failure mode for a missing or malformed config.
func NewGroupAllowlist(groupIDs ...string) *GroupAllowlist {
	ids := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		// Accept either encoding; store the received form, which is what
		// envelopes carry.
		if canonical := GroupID(id); canonical != "" {
			ids[canonical] = struct{}{}
		}
	}
	return &GroupAllowlist{ids: ids}
}

// Allows reports whether pilot may act on this envelope. Direct messages are always
// refused: they have no group, so nothing constrains who sent them.
func (a *GroupAllowlist) Allows(e Envelope) bool {
	if a == nil || !e.IsGroup() {
		return false
	}
	_, ok := a.ids[e.DataMessage.GroupInfo.GroupID]
	return ok
}

// Command is an actionable message: text a human sent to a group pilot serves.
type Command struct {
	Text string
	// GroupID is the received form. Use [GroupRecipient] before replying —
	// the send endpoints expect a different encoding.
	GroupID   string
	SenderID  string
	Timestamp int64
}

// CommandFrom returns the command an envelope carries, or false when it carries
// none. Most frames carry none — typing indicators, delivery receipts and poll
// activity all arrive on the same stream, and poll payloads sit inside dataMessage
// alongside a null message body. Only KindData qualifies.
func CommandFrom(a *GroupAllowlist, e Envelope) (Command, bool) {
	if !a.Allows(e) || e.Kind() != KindData || e.VoterUUID() == "" {
		return Command{}, false
	}
	text := e.Text()
	if text == "" {
		return Command{}, false
	}
	return Command{
		Text:      text,
		GroupID:   e.DataMessage.GroupInfo.GroupID,
		SenderID:  e.VoterUUID(),
		Timestamp: e.Timestamp,
	}, true
}

// Vote is a poll answer bound to the approval it belongs to.
//
// The identity key is (GroupID, PollAuthorID, PollTimestamp): a timestamp alone
// does not identify a poll, since any group member can create one. Revision is
// the voter's monotonic vote counter — an answer may be changed while the poll
// is open, so a consumer must ignore any revision it has already seen or a stale
// redelivery can overturn a later decision.
type Vote struct {
	PollTimestamp int64
	PollAuthorID  string
	OptionIndexes []int
	// GroupID is the received form. Use [GroupRecipient] before replying.
	GroupID    string
	VoterID    string
	Revision   int
	ReceivedAt int64
}

// VoteFrom returns the vote an envelope carries, or false when it carries none.
//
// VoterID comes from the envelope: the author fields inside a poll vote name the
// poll's author, so trusting them would attribute an approval to pilot itself. A
// vote with no identifiable voter is refused rather than attributed to the empty
// principal, which would let two unattributable votes collapse into one.
func VoteFrom(a *GroupAllowlist, e Envelope) (Vote, bool) {
	if !a.Allows(e) || e.Kind() != KindPollVote || e.VoterUUID() == "" {
		return Vote{}, false
	}
	pv := e.DataMessage.PollVote
	// PollAuthorID is half the poll's identity key, so an absent one collapses
	// two members' polls sharing a timestamp into one — the same argument that
	// refuses an empty voter.
	if len(pv.OptionIndexes) == 0 || pv.PollAuthorUUID == "" {
		return Vote{}, false
	}
	return Vote{
		PollTimestamp: pv.TargetSentTimestamp,
		PollAuthorID:  pv.PollAuthorUUID,
		OptionIndexes: pv.OptionIndexes,
		GroupID:       e.DataMessage.GroupInfo.GroupID,
		VoterID:       e.VoterUUID(),
		Revision:      pv.VoteCount,
		ReceivedAt:    e.Timestamp,
	}, true
}
