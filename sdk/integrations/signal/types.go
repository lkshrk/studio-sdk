package signal

import (
	"encoding/json"
	"fmt"
)

// FrameKind identifies which payload an [Envelope] carries. The receive stream is
// mostly not user messages, so callers must select KindData explicitly rather
// than treating every frame as a command.
type FrameKind string

// Frame kinds observed on the receive stream.
const (
	KindData          FrameKind = "dataMessage"
	KindSync          FrameKind = "syncMessage"
	KindTyping        FrameKind = "typingMessage"
	KindReceipt       FrameKind = "receiptMessage"
	KindEdit          FrameKind = "editMessage"
	KindPollCreate    FrameKind = "pollCreate"
	KindPollVote      FrameKind = "pollVote"
	KindPollTerminate FrameKind = "pollTerminate"
	KindUnknown       FrameKind = "unknown"
)

// Frame is one message off the /v1/receive/{number} websocket.
type Frame struct {
	Envelope Envelope `json:"envelope"`
	Account  string   `json:"account"`
}

// Envelope is the sender identity and payload common to every received frame.
type Envelope struct {
	Source                   string          `json:"source"`
	SourceNumber             string          `json:"sourceNumber"`
	SourceUUID               string          `json:"sourceUuid"`
	SourceName               string          `json:"sourceName"`
	SourceDevice             int             `json:"sourceDevice"`
	Timestamp                int64           `json:"timestamp"`
	ServerReceivedTimestamp  int64           `json:"serverReceivedTimestamp"`
	ServerDeliveredTimestamp int64           `json:"serverDeliveredTimestamp"`
	DataMessage              *DataMessage    `json:"dataMessage,omitempty"`
	SyncMessage              json.RawMessage `json:"syncMessage,omitempty"`
	TypingMessage            json.RawMessage `json:"typingMessage,omitempty"`
	ReceiptMessage           json.RawMessage `json:"receiptMessage,omitempty"`
	EditMessage              json.RawMessage `json:"editMessage,omitempty"`
}

// DataMessage carries user-authored content. Poll payloads are nested here rather
// than on the envelope, and arrive with a null Message.
type DataMessage struct {
	Timestamp          int64          `json:"timestamp"`
	Message            *string        `json:"message"`
	ExpiresInSeconds   int            `json:"expiresInSeconds"`
	IsExpirationUpdate bool           `json:"isExpirationUpdate"`
	ViewOnce           bool           `json:"viewOnce"`
	GroupInfo          *GroupInfo     `json:"groupInfo,omitempty"`
	PollCreate         *PollCreate    `json:"pollCreate,omitempty"`
	PollVote           *PollVote      `json:"pollVote,omitempty"`
	PollTerminate      *PollTerminate `json:"pollTerminate,omitempty"`
}

// PollCreate announces a poll another member started.
type PollCreate struct {
	Question      string   `json:"question"`
	AllowMultiple bool     `json:"allowMultiple"`
	Options       []string `json:"options"`
}

// PollVote is a vote cast on a poll.
//
// PollAuthorUUID is the author of the poll being voted on — for an approval that
// is pilot itself, never the voter. It is named to make that unmistakable and
// exists so a vote can be bound to a poll pilot actually created; the voter comes
// from [Envelope.VoterUUID].
//
// VoteCount is the voter's revision number: Signal lets an open poll's answer be
// changed, and each change increments it. Without it a stale redelivery and a
// deliberate change are indistinguishable.
type PollVote struct {
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	OptionIndexes       []int  `json:"optionIndexes"`
	VoteCount           int    `json:"voteCount"`
	PollAuthorUUID      string `json:"authorUuid"`
}

// PollTerminate reports a poll being closed by its author.
type PollTerminate struct {
	TargetSentTimestamp int64 `json:"targetSentTimestamp"`
}

// GroupInfo is present when a message was sent to a group rather than direct.
type GroupInfo struct {
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
	Revision  int    `json:"revision"`
	Type      string `json:"type"`
}

// Kind reports which payload the envelope carries. Poll payloads are nested inside
// dataMessage and must be tested before it, or every vote reads as a user message.
func (e Envelope) Kind() FrameKind {
	if dm := e.DataMessage; dm != nil {
		switch {
		case dm.PollVote != nil:
			return KindPollVote
		case dm.PollCreate != nil:
			return KindPollCreate
		case dm.PollTerminate != nil:
			return KindPollTerminate
		}
	}
	switch {
	case e.DataMessage != nil:
		return KindData
	case len(e.SyncMessage) > 0:
		return KindSync
	case len(e.TypingMessage) > 0:
		return KindTyping
	case len(e.ReceiptMessage) > 0:
		return KindReceipt
	case len(e.EditMessage) > 0:
		return KindEdit
	default:
		return KindUnknown
	}
}

// IsGroup reports whether the frame is a group message.
func (e Envelope) IsGroup() bool {
	return e.DataMessage != nil && e.DataMessage.GroupInfo != nil
}

// VoterUUID is the person who acted, and the only identity safe to attribute an
// approval to. The author fields inside a poll vote name the poll's author — the
// bot — so trusting them would record pilot approving its own work.
func (e Envelope) VoterUUID() string { return e.SourceUUID }

// Text is the user-authored message body, empty for the many frames that carry none.
func (e Envelope) Text() string {
	if e.DataMessage == nil || e.DataMessage.Message == nil {
		return ""
	}
	return *e.DataMessage.Message
}

// ParseFrame decodes one raw websocket frame.
func ParseFrame(b []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return Frame{}, fmt.Errorf("signalcli: parse frame: %w", err)
	}
	return f, nil
}
