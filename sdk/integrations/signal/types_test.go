package signal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qf-studio/studio-sdk/sdk/integrations/signal"
)

func load(t *testing.T, name string) signal.Frame {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f, err := signal.ParseFrame(b)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return f
}

func TestEnvelopeKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture string
		want    signal.FrameKind
		group   bool
	}{
		{"group_data_message.json", signal.KindData, true},
		{"typing_message.json", signal.KindTyping, false},
		{"receipt_message.json", signal.KindReceipt, false},
		{"poll_vote.json", signal.KindPollVote, true},
		{"poll_create.json", signal.KindPollCreate, true},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			f := load(t, tc.fixture)
			if got := f.Envelope.Kind(); got != tc.want {
				t.Errorf("Kind() = %q, want %q", got, tc.want)
			}
			if got := f.Envelope.IsGroup(); got != tc.group {
				t.Errorf("IsGroup() = %v, want %v", got, tc.group)
			}
		})
	}
}

func TestGroupDataMessageFields(t *testing.T) {
	t.Parallel()

	f := load(t, "group_data_message.json")
	dm := f.Envelope.DataMessage
	if dm == nil {
		t.Fatal("DataMessage is nil")
	}
	if dm.Message == nil || *dm.Message != "hello claude" {
		t.Errorf("Message = %v", dm.Message)
	}
	if dm.GroupInfo == nil {
		t.Fatal("GroupInfo is nil")
	}
	if dm.GroupInfo.GroupName != "h-cloud admin" {
		t.Errorf("GroupName = %q", dm.GroupInfo.GroupName)
	}
	if f.Envelope.SourceUUID == "" {
		t.Error("SourceUUID is empty; identity must survive parsing")
	}
	if f.Account == "" {
		t.Error("Account is empty")
	}
}

// Poll payloads are nested inside dataMessage, so a Kind() that tests DataMessage
// first classifies every vote as a user message with an empty body.
func TestPollVoteIsNotMistakenForAMessage(t *testing.T) {
	t.Parallel()

	f := load(t, "poll_vote.json")
	if got := f.Envelope.Kind(); got == signal.KindData {
		t.Fatal("poll vote classified as a user message")
	}
	if got := f.Envelope.Text(); got != "" {
		t.Errorf("Text() = %q, want empty for a poll frame", got)
	}
	if f.Envelope.DataMessage.Message != nil {
		t.Error("poll frames carry a null message; Message must stay nil-able")
	}
}

func TestPollVoteBindsToItsPoll(t *testing.T) {
	t.Parallel()

	pv := load(t, "poll_vote.json").Envelope.DataMessage.PollVote
	if pv == nil {
		t.Fatal("PollVote is nil")
	}
	if pv.TargetSentTimestamp != 1786854621623 {
		t.Errorf("TargetSentTimestamp = %d", pv.TargetSentTimestamp)
	}
	if len(pv.OptionIndexes) != 1 || pv.OptionIndexes[0] != 1 {
		t.Errorf("OptionIndexes = %v", pv.OptionIndexes)
	}
}

// The author fields inside a vote name the poll's author (the bot), not the voter.
// Attributing an approval to them would record pilot approving its own PR, so they
// are deliberately absent from PollVote and the voter comes from the envelope.
func TestVoterAttributionComesFromEnvelope(t *testing.T) {
	t.Parallel()

	env := load(t, "poll_vote.json").Envelope
	if env.VoterUUID() != env.SourceUUID {
		t.Error("VoterUUID must be the envelope source")
	}
	if env.VoterUUID() == "" {
		t.Fatal("VoterUUID is empty; an approval could not be attributed")
	}
	// The fixture's account is the bot; the voter must not be it.
	if env.VoterUUID() == "00000000-0000-4000-8000-0000000000bb" {
		t.Error("voter resolved to the poll author (the bot), not the human")
	}
}

func TestPollCreateOptions(t *testing.T) {
	t.Parallel()

	pc := load(t, "poll_create.json").Envelope.DataMessage.PollCreate
	if pc == nil {
		t.Fatal("PollCreate is nil")
	}
	if pc.AllowMultiple {
		t.Error("AllowMultiple = true, want false")
	}
	if len(pc.Options) != 2 {
		t.Errorf("Options = %v, want 2", pc.Options)
	}
}

func TestParseFrameRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := signal.ParseFrame([]byte("not json")); err == nil {
		t.Fatal("expected error for malformed frame")
	}
}

func TestUnknownKindForEmptyEnvelope(t *testing.T) {
	t.Parallel()

	f, err := signal.ParseFrame([]byte(`{"envelope":{},"account":"+490000000001"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.Envelope.Kind(); got != signal.KindUnknown {
		t.Errorf("Kind() = %q, want %q", got, signal.KindUnknown)
	}
}
