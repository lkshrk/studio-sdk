package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qf-studio/studio-sdk/sdk/core"
)

const (
	fixtureGroupID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	fixtureVoterID = "00000000-0000-4000-8000-000000000001"
)

type recordingHandler struct {
	mu     sync.Mutex
	events []core.MessageEvent
}

func (h *recordingHandler) HandleMessage(_ context.Context, ev core.MessageEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
	return nil
}

func (h *recordingHandler) all() []core.MessageEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]core.MessageEvent(nil), h.events...)
}

func loadEnvelope(t *testing.T, name string) Envelope {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return f.Envelope
}

type apiCall struct {
	path string
	body map[string]any
}

func newTestBridge(t *testing.T, srvURL string) (*bridge, *recordingHandler) {
	t.Helper()
	return newTestBridgeWithApprovers(t, srvURL)
}

func newTestBridgeWithApprovers(t *testing.T, srvURL string, approvers ...string) (*bridge, *recordingHandler) {
	t.Helper()
	h := &recordingHandler{}
	a := New(Config{
		BaseURL:   srvURL,
		Account:   "+490000000001",
		Groups:    []string{fixtureGroupID},
		Approvers: approvers,
	}, nil)
	b := a.NewChatBridge(core.ChatDeps{Handler: h}).(*bridge)
	if b.initErr != nil {
		t.Fatalf("bridge init: %v", b.initErr)
	}
	return b, h
}

func apiServer(t *testing.T, response string) (*httptest.Server, *[]apiCall) {
	t.Helper()
	var mu sync.Mutex
	calls := []apiCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, apiCall{path: r.URL.Path, body: body})
		mu.Unlock()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestProcessFrameEmitsSanitizedMessage(t *testing.T) {
	b, h := newTestBridge(t, "http://signal.invalid")

	e := loadEnvelope(t, "group_data_message.json")
	smuggled := "review​ this⁠ change"
	e.DataMessage.Message = &smuggled

	b.processFrame(context.Background(), e)

	events := h.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Action != "message" {
		t.Errorf("action = %q, want message", ev.Action)
	}
	if ev.Text != "review this change" {
		t.Errorf("text = %q, want invisible runes stripped", ev.Text)
	}
	if ev.ChannelID != fixtureGroupID {
		t.Errorf("channel = %q, want group ID", ev.ChannelID)
	}
	if ev.Sender.UserID != fixtureVoterID || ev.Sender.DisplayName != "Tester" {
		t.Errorf("sender = %+v", ev.Sender)
	}
}

func TestProcessFrameNormalizesCommands(t *testing.T) {
	b, h := newTestBridge(t, "http://signal.invalid")

	e := loadEnvelope(t, "group_data_message.json")
	cmd := "/run TASK-7 now"
	e.DataMessage.Message = &cmd

	b.processFrame(context.Background(), e)

	events := h.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Action != "command" || ev.Command != "/run" {
		t.Errorf("action/command = %q/%q", ev.Action, ev.Command)
	}
	if len(ev.Args) != 2 || ev.Args[0] != "TASK-7" {
		t.Errorf("args = %v", ev.Args)
	}
}

func TestProcessFrameRefusesUnlistedGroupAndSelf(t *testing.T) {
	b, h := newTestBridge(t, "http://signal.invalid")
	b.selfUUID = fixtureVoterID

	self := loadEnvelope(t, "group_data_message.json")
	b.processFrame(context.Background(), self)

	other := loadEnvelope(t, "group_data_message.json")
	other.SourceUUID = "someone-else"
	other.DataMessage.GroupInfo.GroupID = "not-allowlisted"
	b.processFrame(context.Background(), other)

	if got := len(h.all()); got != 0 {
		t.Errorf("got %d events, want 0 (self and unlisted group refused)", got)
	}
}

func TestSendWithButtonsCreatesPollAndVoteBecomesCallback(t *testing.T) {
	srv, calls := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, h := newTestBridge(t, srv.URL)

	ref, err := b.Send(context.Background(), core.OutboundMessage{
		ChannelID: fixtureGroupID,
		Text:      "Approve TASK-7? deletes the unused PoiCard",
		Buttons: []core.Button{
			{Label: "Approve", ActionID: "approve", Data: "approve:TASK-7"},
			{Label: "Reject", ActionID: "reject", Data: "reject:TASK-7"},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref.MessageID != "1786854621623" {
		t.Errorf("ref.MessageID = %q", ref.MessageID)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0].path, "/polls") {
		t.Fatalf("calls = %+v, want one poll create", *calls)
	}

	vote := loadEnvelope(t, "poll_vote.json")
	b.processFrame(context.Background(), vote)

	events := h.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 callback", len(events))
	}
	ev := events[0]
	if ev.Action != "callback" {
		t.Fatalf("action = %q, want callback", ev.Action)
	}
	if ev.Data != "reject:TASK-7" {
		t.Errorf("data = %q, want the voted option's payload (index 1)", ev.Data)
	}
	if ev.Sender.UserID != fixtureVoterID {
		t.Errorf("voter = %q", ev.Sender.UserID)
	}

	if err := b.Ack(context.Background(), ev.CallbackID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	last := (*calls)[len(*calls)-1]
	if !strings.Contains(last.path, "/polls") {
		t.Errorf("Ack call path = %q, want poll close", last.path)
	}
}

func TestStaleVoteRevisionIsIgnored(t *testing.T) {
	srv, _ := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, h := newTestBridge(t, srv.URL)

	if _, err := b.Send(context.Background(), core.OutboundMessage{
		ChannelID: fixtureGroupID,
		Text:      "Approve?",
		Buttons:   []core.Button{{Label: "Approve", Data: "a"}, {Label: "Reject", Data: "r"}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	vote := loadEnvelope(t, "poll_vote.json")
	b.processFrame(context.Background(), vote)
	b.processFrame(context.Background(), vote)

	if got := len(h.all()); got != 1 {
		t.Errorf("got %d events, want 1 (same-revision redelivery ignored)", got)
	}
}

// openApprovalPoll posts a two-option poll so a fixture vote has a poll to hit.
func openApprovalPoll(t *testing.T, b *bridge) {
	t.Helper()
	if _, err := b.Send(context.Background(), core.OutboundMessage{
		ChannelID: fixtureGroupID,
		Text:      "Approve?",
		Buttons:   []core.Button{{Label: "Approve", Data: "a"}, {Label: "Reject", Data: "r"}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestNonApproverVoteIsRefused(t *testing.T) {
	srv, calls := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, h := newTestBridgeWithApprovers(t, srv.URL, "00000000-0000-4000-8000-00000000ffff")
	openApprovalPoll(t, b)

	b.processFrame(context.Background(), loadEnvelope(t, "poll_vote.json"))

	if got := len(h.all()); got != 0 {
		t.Errorf("got %d events, want 0 (voter is not an approver)", got)
	}
	last := (*calls)[len(*calls)-1]
	if last.path != "/v2/send" {
		t.Fatalf("last call = %q, want a refusal message", last.path)
	}
	if last.body["message"] != refusedVoteText {
		t.Errorf("message = %v, want %q", last.body["message"], refusedVoteText)
	}
	if _, closed := b.polls[1786854621623]; !closed {
		t.Error("poll was forgotten; it must stay open for an approver")
	}
}

func TestApproverVoteEmitsCallback(t *testing.T) {
	srv, _ := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, h := newTestBridgeWithApprovers(t, srv.URL, fixtureVoterID)
	openApprovalPoll(t, b)

	b.processFrame(context.Background(), loadEnvelope(t, "poll_vote.json"))

	events := h.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 callback", len(events))
	}
	if events[0].Action != "callback" || events[0].Sender.UserID != fixtureVoterID {
		t.Errorf("event = %+v", events[0])
	}
}

func TestEmptyApproversAdmitsAnyGroupMember(t *testing.T) {
	srv, calls := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, h := newTestBridgeWithApprovers(t, srv.URL)
	openApprovalPoll(t, b)

	b.processFrame(context.Background(), loadEnvelope(t, "poll_vote.json"))

	if got := len(h.all()); got != 1 {
		t.Fatalf("got %d events, want 1 (group membership is the boundary)", got)
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %+v, want only the poll create (no refusal)", *calls)
	}
}

func TestVoteOnUnknownPollIsIgnored(t *testing.T) {
	b, h := newTestBridge(t, "http://signal.invalid")

	vote := loadEnvelope(t, "poll_vote.json")
	b.processFrame(context.Background(), vote)

	if got := len(h.all()); got != 0 {
		t.Errorf("got %d events, want 0 (no poll registered for this timestamp)", got)
	}
}

func TestSendWithoutButtonsChunks(t *testing.T) {
	srv, calls := apiServer(t, `{"timestamp":"1786854621623"}`)
	b, _ := newTestBridge(t, srv.URL)

	ref, err := b.Send(context.Background(), core.OutboundMessage{
		ChannelID: fixtureGroupID,
		Text:      "plain progress update",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref.MessageID != "1786854621623" {
		t.Errorf("ref.MessageID = %q", ref.MessageID)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/v2/send" {
		t.Errorf("calls = %+v, want one /v2/send", *calls)
	}
}

func TestEditPostsFreshMessage(t *testing.T) {
	srv, calls := apiServer(t, `{"timestamp":"1786854621999"}`)
	b, _ := newTestBridge(t, srv.URL)

	err := b.Edit(context.Background(), core.MessageRef{ChannelID: fixtureGroupID, MessageID: "1"}, "updated")
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/v2/send" {
		t.Errorf("calls = %+v, want one /v2/send (no edit endpoint exists)", *calls)
	}
}

func TestStartRefusesWithoutGroups(t *testing.T) {
	a := New(Config{BaseURL: "http://signal.invalid", Account: "+49"}, nil)
	b := a.NewChatBridge(core.ChatDeps{Handler: &recordingHandler{}})

	err := b.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no groups") {
		t.Errorf("Start error = %v, want no-groups refusal", err)
	}
}
