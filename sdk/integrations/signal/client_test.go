package signal_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qf-studio/studio-sdk/sdk/integrations/signal"
)

type captured struct {
	method string
	path   string
	body   map[string]any
}

// apiServer records requests and replies with the given status/body.
func apiServer(t *testing.T, status int, reply string) (*httptest.Server, *[]captured) {
	t.Helper()
	var mu sync.Mutex
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, captured{r.Method, r.URL.Path, body})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newSender(t *testing.T, srv *httptest.Server, opts ...signal.SenderOption) *signal.Sender {
	t.Helper()
	s, err := signal.NewSender(srv.URL, testAccount, opts...)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

func TestSendTextReturnsTimestamp(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1786852271208"}`)
	ts, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hello")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if ts != 1786852271208 {
		t.Errorf("timestamp = %d", ts)
	}
	if len(*got) != 1 || (*got)[0].path != "/v2/send" {
		t.Fatalf("requests = %+v", *got)
	}
	if (*got)[0].body["message"] != "hello" {
		t.Errorf("body = %+v", (*got)[0].body)
	}
}

func TestSendTextRejectsEmptyInput(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1"}`)
	s := newSender(t, srv)

	if _, err := s.SendText(t.Context(), "", "hi"); err == nil {
		t.Error("empty recipient accepted")
	}
	if _, err := s.SendText(t.Context(), "group.abc", ""); err == nil {
		t.Error("empty message accepted")
	}
	if len(*got) != 0 {
		t.Errorf("invalid input still hit the API: %+v", *got)
	}
}

// A partial delivery must never look like a success: the caller needs to know
// which chunks landed.
func TestSendChunkedStopsAtFirstFailure(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"timestamp":"1"}`))
	}))
	t.Cleanup(srv.Close)

	s := newSender(t, srv, signal.WithMaxMessageLength(5))
	stamps, err := s.SendChunked(t.Context(), "group.abc", "aaaaa\nbbbbb\nccccc\n")
	if err == nil {
		t.Fatal("partial delivery reported as success")
	}
	if len(stamps) != 1 {
		t.Errorf("stamps = %v, want the one chunk that landed", stamps)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("calls = %d, want to stop after the failure", calls)
	}
}

func TestSendChunkedSplitsOnLines(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1"}`)
	s := newSender(t, srv, signal.WithMaxMessageLength(12))

	if _, err := s.SendChunked(t.Context(), "group.abc", "line one\nline two\nline three\n"); err != nil {
		t.Fatalf("SendChunked: %v", err)
	}
	if len(*got) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(*got))
	}
	for _, c := range *got {
		msg, _ := c.body["message"].(string)
		if len([]rune(msg)) > 12 {
			t.Errorf("chunk exceeds limit: %q", msg)
		}
		if strings.TrimSpace(msg) == "" {
			t.Error("empty chunk sent")
		}
	}
}

func TestSendChunkedHardCutsAnOversizedLine(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1"}`)
	s := newSender(t, srv, signal.WithMaxMessageLength(4))

	if _, err := s.SendChunked(t.Context(), "group.abc", strings.Repeat("x", 14)); err != nil {
		t.Fatalf("SendChunked: %v", err)
	}
	var total int
	for _, c := range *got {
		msg, _ := c.body["message"].(string)
		if len([]rune(msg)) > 4 {
			t.Errorf("chunk exceeds limit: %q", msg)
		}
		total += len([]rune(msg))
	}
	if total != 14 {
		t.Errorf("reassembled length = %d, want 14 (no characters dropped)", total)
	}
}

func TestCreatePollValidatesOptions(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1786853456143"}`)
	s := newSender(t, srv)

	tests := []struct {
		name    string
		q       string
		options []string
	}{
		{"no question", "", []string{"a", "b"}},
		{"one option", "q", []string{"only"}},
		{"eleven options", "q", strings.Split("a,b,c,d,e,f,g,h,i,j,k", ",")},
		{"empty option", "q", []string{"a", ""}},
		{"overlong option", "q", []string{"a", strings.Repeat("x", 101)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreatePoll(t.Context(), "group.abc", tc.q, tc.options, false); err == nil {
				t.Error("invalid poll accepted")
			}
		})
	}
	if len(*got) != 0 {
		t.Errorf("invalid poll still hit the API: %+v", *got)
	}
}

func TestCreatePollSendsExpectedBody(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1786853456143"}`)
	ts, err := newSender(t, srv).CreatePoll(t.Context(), "group.abc",
		"Approve HCL-1?", []string{"Approve", "Reject"}, false)
	if err != nil {
		t.Fatalf("CreatePoll: %v", err)
	}
	if ts != 1786853456143 {
		t.Errorf("timestamp = %d", ts)
	}
	body := (*got)[0].body
	if body["allow_multiple_selections"] != false {
		t.Error("allow_multiple_selections must be false for an approval")
	}
	if (*got)[0].path != "/v1/polls/"+testAccount {
		t.Errorf("path = %q", (*got)[0].path)
	}
}

func TestClosePollSendsTimestampAsString(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusNoContent, ``)
	if err := newSender(t, srv).ClosePoll(t.Context(), "group.abc", 1786853456143); err != nil {
		t.Fatalf("ClosePoll: %v", err)
	}
	c := (*got)[0]
	if c.method != http.MethodDelete {
		t.Errorf("method = %q", c.method)
	}
	if c.body["poll_timestamp"] != "1786853456143" {
		t.Errorf("poll_timestamp = %v, want a string", c.body["poll_timestamp"])
	}
}

func TestClosePollRejectsZeroTimestamp(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusNoContent, ``)
	if err := newSender(t, srv).ClosePoll(t.Context(), "group.abc", 0); err == nil {
		t.Error("zero timestamp accepted")
	}
	if len(*got) != 0 {
		t.Error("invalid close still hit the API")
	}
}

// Retrying a validation failure would post duplicate approval polls to a group.
func TestAPIErrorRetryable(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusBadRequest, `{"error":"bad"}`)
	_, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi")

	var apiErr *signal.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.Retryable() {
		t.Error("400 reported as retryable")
	}

	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway} {
		srv, _ := apiServer(t, status, `{}`)
		_, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi")
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			t.Errorf("status %d: not reported retryable", status)
		}
	}
}

func TestNewSenderValidates(t *testing.T) {
	t.Parallel()

	if _, err := signal.NewSender("", testAccount); err == nil {
		t.Error("empty base url accepted")
	}
	if _, err := signal.NewSender("http://host", ""); err == nil {
		t.Error("empty account accepted")
	}
}

func TestWithMaxMessageLengthIgnoresNonPositive(t *testing.T) {
	t.Parallel()

	s, err := signal.NewSender("http://host", testAccount, signal.WithMaxMessageLength(0))
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if s.MaxMessageLength() != signal.DefaultMaxMessageLength {
		t.Errorf("MaxMessageLength = %d, want the default", s.MaxMessageLength())
	}
}

// A 201 does not mean delivered: signal-cli-rest-api reports per-recipient
// failures inside the success body, so a message that reached nobody would
// otherwise look sent.
func TestSendTextSurfacesPerRecipientFailures(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusCreated,
		`{"timestamp":"1786852271208","errors":{"recipients":[{"number":"+490000000002","reason":"UNREGISTERED_FAILURE"}]}}`)

	ts, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi")
	if err == nil {
		t.Fatal("undelivered message reported as sent")
	}
	var de *signal.DeliveryError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want DeliveryError", err)
	}
	if len(de.Failures) != 1 || de.Failures[0].Reason != "UNREGISTERED_FAILURE" {
		t.Errorf("failures = %+v", de.Failures)
	}
	if ts == 0 {
		t.Error("timestamp discarded; the caller cannot correlate the partial send")
	}
}

// SendChunked must stop when a chunk is accepted but undelivered, not pile
// further chunks on top of a message nobody received.
func TestSendChunkedStopsOnUndeliveredChunk(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusCreated,
		`{"timestamp":"1","errors":{"recipients":[{"number":"+49","reason":"NETWORK_FAILURE"}]}}`)
	s := newSender(t, srv, signal.WithMaxMessageLength(5))

	if _, err := s.SendChunked(t.Context(), "group.abc", "aaaaa\nbbbbb\n"); err == nil {
		t.Fatal("undelivered chunk reported as success")
	}
}

// A poll accepted without a usable timestamp is live, votable and unclosable.
// Retrying would add a second approval poll, so the error must be distinct.
func TestCreatePollPostedButUnidentified(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusCreated, `{"timestamp":""}`)
	_, err := newSender(t, srv).CreatePoll(t.Context(), "group.abc", "Approve?",
		[]string{"Approve", "Reject"}, false)

	var pe *signal.PostedUnidentifiedError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want PostedUnidentifiedError", err)
	}
	var ae *signal.APIError
	if errors.As(err, &ae) {
		t.Error("must not be an APIError; Retryable() would be consulted and could re-post")
	}
}

// A megabyte of agent log must not become hundreds of messages in a group.
func TestSendChunkedRefusesToFloodAGroup(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1"}`)
	s := newSender(t, srv, signal.WithMaxMessageLength(10))

	if _, err := s.SendChunked(t.Context(), "group.abc", strings.Repeat("x\n", 500)); err == nil {
		t.Fatal("unbounded chunk count accepted")
	}
	if len(*got) != 0 {
		t.Errorf("sent %d chunks before refusing; must refuse before sending any", len(*got))
	}
}

// A build without poll support answers 501 forever; retrying never helps.
func TestNotImplementedIsNotRetryable(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusNotImplemented, `{"error":"unsupported"}`)
	_, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi")

	var ae *signal.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if ae.Retryable() {
		t.Error("501 reported retryable")
	}
}

func TestRecipientRequiredOnEveryOperation(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1"}`)
	s := newSender(t, srv)

	if err := s.React(t.Context(), "", "+49", 1, "👍"); err == nil {
		t.Error("React accepted an empty recipient")
	}
	if _, err := s.CreatePoll(t.Context(), "", "q", []string{"a", "b"}, false); err == nil {
		t.Error("CreatePoll accepted an empty recipient")
	}
	if err := s.ClosePoll(t.Context(), "", 1); err == nil {
		t.Error("ClosePoll accepted an empty recipient")
	}
	if len(*got) != 0 {
		t.Errorf("invalid input still hit the API: %+v", *got)
	}
}

// A zero timestamp parses cleanly but is not a usable handle: ClosePoll rejects
// it, so the poll would be live, votable and unclosable forever.
func TestCreatePollRejectsZeroTimestampResponse(t *testing.T) {
	t.Parallel()

	for _, reply := range []string{`{"timestamp":"0"}`, `{"timestamp":"-1"}`, `{}`} {
		srv, _ := apiServer(t, http.StatusCreated, reply)
		_, err := newSender(t, srv).CreatePoll(t.Context(), "group.abc", "Approve?",
			[]string{"Approve", "Reject"}, false)

		var pe *signal.PostedUnidentifiedError
		if !errors.As(err, &pe) {
			t.Errorf("reply %s: err = %v, want PostedUnidentifiedError", reply, err)
			continue
		}
		if pe.Retryable() {
			t.Errorf("reply %s: reported retryable; a retry would post a second poll", reply)
		}
	}
}

// An unreadable body after a 2xx means the server acted and the reply was lost —
// not that nothing happened.
func TestUnreadableSuccessBodyIsPostedUnidentified(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusCreated, `{"timestamp": broken`)
	_, err := newSender(t, srv).CreatePoll(t.Context(), "group.abc", "Approve?",
		[]string{"Approve", "Reject"}, false)

	var pe *signal.PostedUnidentifiedError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want PostedUnidentifiedError", err)
	}
}

// The /v2/send response shape changed upstream: 0.100 returns an object, builds
// after 6a225e6 return an array. An image bump must not silently break sending.
func TestSendTextAcceptsBothResponseShapes(t *testing.T) {
	t.Parallel()

	for name, reply := range map[string]string{
		"object (0.100)": `{"timestamp":"1786852271208"}`,
		"array (master)": `[{"timestamp":"1786852271208"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := apiServer(t, http.StatusCreated, reply)
			ts, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi")
			if err != nil {
				t.Fatalf("SendText: %v", err)
			}
			if ts != 1786852271208 {
				t.Errorf("timestamp = %d", ts)
			}
		})
	}
}

func TestSendTextRejectsAmbiguousMultiResult(t *testing.T) {
	t.Parallel()

	srv, _ := apiServer(t, http.StatusCreated, `[{"timestamp":"1"},{"timestamp":"2"}]`)
	if _, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi"); err == nil {
		t.Fatal("multiple results for a single recipient accepted")
	}
}

// Closing a poll is what makes an approval immutable, so anything other than the
// documented 204 must not read as success.
func TestClosePollRequiresExactly204(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusCreated} {
		srv, _ := apiServer(t, status, `{}`)
		if err := newSender(t, srv).ClosePoll(t.Context(), "group.abc", 1); err == nil {
			t.Errorf("status %d accepted as a successful close", status)
		}
	}
	srv, _ := apiServer(t, http.StatusNoContent, ``)
	if err := newSender(t, srv).ClosePoll(t.Context(), "group.abc", 1); err != nil {
		t.Errorf("204 rejected: %v", err)
	}
}

// A redirect rewrites DELETE into GET and drops the body; following it would
// report a close that never happened.
func TestSenderDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var target int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, backend.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	if err := newSender(t, srv).ClosePoll(t.Context(), "group.abc", 1); err == nil {
		t.Fatal("redirect followed and reported as a successful close")
	}
	if target != 0 {
		t.Errorf("redirect target was contacted %d times", target)
	}
}

func TestSendTextStyledMode(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1786852271208"}`)
	if _, err := newSender(t, srv, signal.WithStyledText()).SendText(t.Context(), "group.abc", "**hi**"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if (*got)[0].body["text_mode"] != "styled" {
		t.Errorf("text_mode missing from styled send: %+v", (*got)[0].body)
	}
}

func TestSendTextPlainModeOmitsTextMode(t *testing.T) {
	t.Parallel()

	srv, got := apiServer(t, http.StatusCreated, `{"timestamp":"1786852271208"}`)
	if _, err := newSender(t, srv).SendText(t.Context(), "group.abc", "hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if _, ok := (*got)[0].body["text_mode"]; ok {
		t.Errorf("text_mode set on plain send: %+v", (*got)[0].body)
	}
}
