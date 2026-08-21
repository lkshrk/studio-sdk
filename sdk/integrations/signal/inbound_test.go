package signal_test

import (
	"strings"
	"testing"

	"github.com/qf-studio/studio-sdk/sdk/integrations/signal"
)

const fixtureGroup = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func TestAllowlistIsDefaultDeny(t *testing.T) {
	t.Parallel()

	env := load(t, "group_data_message.json").Envelope

	tests := []struct {
		name  string
		allow *signal.GroupAllowlist
		want  bool
	}{
		{"allowlisted group", signal.NewGroupAllowlist(fixtureGroup), true},
		{"other group only", signal.NewGroupAllowlist("some-other-group"), false},
		{"empty allowlist", signal.NewGroupAllowlist(), false},
		{"empty string is not a group", signal.NewGroupAllowlist(""), false},
		{"nil allowlist", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.allow.Allows(env); got != tc.want {
				t.Errorf("Allows() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A direct message has no group, so nothing constrains who sent it. Admitting one
// would bypass the boundary that decides who can approve a merge.
func TestDirectMessagesAreRefused(t *testing.T) {
	t.Parallel()

	f, err := signal.ParseFrame([]byte(
		`{"envelope":{"sourceUuid":"u1","dataMessage":{"message":"deploy"}},"account":"+490000000001"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	allow := signal.NewGroupAllowlist(fixtureGroup)
	if allow.Allows(f.Envelope) {
		t.Fatal("direct message admitted")
	}
	if _, ok := signal.CommandFrom(allow, f.Envelope); ok {
		t.Error("direct message surfaced as a command")
	}
}

func TestOnlyDataFramesBecomeCommands(t *testing.T) {
	t.Parallel()

	allow := signal.NewGroupAllowlist(fixtureGroup)

	tests := []struct {
		fixture string
		want    bool
	}{
		{"group_data_message.json", true},
		{"poll_vote.json", false},
		{"poll_create.json", false},
		{"typing_message.json", false},
		{"receipt_message.json", false},
	}

	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			cmd, ok := signal.CommandFrom(allow, load(t, tc.fixture).Envelope)
			if ok != tc.want {
				t.Fatalf("CommandFrom() ok = %v, want %v", ok, tc.want)
			}
			if ok && cmd.Text != "hello claude" {
				t.Errorf("Text = %q", cmd.Text)
			}
		})
	}
}

func TestVoteFromBindsAndAttributes(t *testing.T) {
	t.Parallel()

	allow := signal.NewGroupAllowlist(fixtureGroup)
	env := load(t, "poll_vote.json").Envelope

	vote, ok := signal.VoteFrom(allow, env)
	if !ok {
		t.Fatal("poll vote not recognised")
	}
	if vote.PollTimestamp != 1786854621623 {
		t.Errorf("PollTimestamp = %d", vote.PollTimestamp)
	}
	if vote.VoterID != env.SourceUUID {
		t.Errorf("VoterID = %q, want the envelope source", vote.VoterID)
	}
	// The fixture's poll author is the bot. Attributing to it would record pilot
	// approving its own work.
	if vote.VoterID == "00000000-0000-4000-8000-0000000000bb" {
		t.Error("vote attributed to the poll author instead of the voter")
	}
}

func TestVoteFromRejectsNonVotes(t *testing.T) {
	t.Parallel()

	allow := signal.NewGroupAllowlist(fixtureGroup)
	for _, fixture := range []string{"group_data_message.json", "poll_create.json"} {
		if _, ok := signal.VoteFrom(allow, load(t, fixture).Envelope); ok {
			t.Errorf("%s surfaced as a vote", fixture)
		}
	}
}

// A vote in a group pilot does not serve must not resolve an approval.
func TestVoteFromDeniesForeignGroup(t *testing.T) {
	t.Parallel()

	allow := signal.NewGroupAllowlist("some-other-group")
	if _, ok := signal.VoteFrom(allow, load(t, "poll_vote.json").Envelope); ok {
		t.Fatal("vote from a non-allowlisted group accepted")
	}
}

// Received and send-side group identifiers are different encodings of the same
// group. These values are a real captured pair: passing the received form back
// as a recipient addresses nothing.
func TestGroupRecipientConvertsReceivedForm(t *testing.T) {
	t.Parallel()

	const received = "Km7Q/DSD6h3XlGjsZ5BSZ+nKbdukhEe/2rYzaJYwmrw="
	const wantPrefix = "group.S203US9EU0Q2aDNYbEdqc1"

	got := signal.GroupRecipient(received)
	if got == received {
		t.Fatal("received form returned unchanged; replies would go nowhere")
	}
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("GroupRecipient(%q) = %q, want prefix %q", received, got, wantPrefix)
	}
}

func TestGroupRecipientIsIdempotent(t *testing.T) {
	t.Parallel()

	once := signal.GroupRecipient("Km7Q/DSD6h3XlGjsZ5BSZ+nKbdukhEe/2rYzaJYwmrw=")
	if twice := signal.GroupRecipient(once); twice != once {
		t.Errorf("double conversion changed the recipient: %q -> %q", once, twice)
	}
	if got := signal.GroupRecipient(""); got != "" {
		t.Errorf("GroupRecipient(\"\") = %q, want empty", got)
	}
}

// PollAuthorID is half the poll's identity key, so an absent one must be
// refused for the same reason an absent voter is.
func TestVoteFromRequiresPollAuthor(t *testing.T) {
	t.Parallel()

	raw := `{"envelope":{"sourceUuid":"u-human","dataMessage":{"message":null,
	  "pollVote":{"targetSentTimestamp":1,"optionIndexes":[0],"voteCount":1},
	  "groupInfo":{"groupId":"` + fixtureGroup + `","groupName":"g"}}},"account":"+49"}`
	f, err := signal.ParseFrame([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := signal.VoteFrom(signal.NewGroupAllowlist(fixtureGroup), f.Envelope); ok {
		t.Error("vote with no poll author accepted")
	}
}

// An operator naturally copies group IDs from GET /v1/groups, which returns the
// send-side form, while envelopes carry the raw form. Comparing them unconverted
// matches nothing and silently refuses every command.
func TestAllowlistAcceptsEitherGroupEncoding(t *testing.T) {
	t.Parallel()

	received := load(t, "group_data_message.json").Envelope
	sendForm := signal.GroupRecipient(fixtureGroup)

	if sendForm == fixtureGroup {
		t.Fatal("test precondition: the two encodings should differ")
	}
	for _, configured := range []string{fixtureGroup, sendForm} {
		if !signal.NewGroupAllowlist(configured).Allows(received) {
			t.Errorf("allowlist configured with %q rejected the matching group", configured)
		}
	}
}

func TestGroupIDRoundTripsWithGroupRecipient(t *testing.T) {
	t.Parallel()

	const raw = "Km7Q/DSD6h3XlGjsZ5BSZ+nKbdukhEe/2rYzaJYwmrw="
	if got := signal.GroupID(signal.GroupRecipient(raw)); got != raw {
		t.Errorf("round trip = %q, want %q", got, raw)
	}
	// A malformed send-form value must degrade to itself, not to empty.
	if got := signal.GroupID("group.!!!not-base64!!!"); got == "" {
		t.Error("malformed recipient collapsed to empty; would match nothing silently")
	}
}
