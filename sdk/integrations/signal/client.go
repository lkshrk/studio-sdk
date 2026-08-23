package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultMaxMessageLength is the chunking threshold. Signal rejects messages
// beyond roughly this many characters, but the API exposes no limit to read, so
// this is a conservative default rather than a discovered value; override it
// with [WithMaxMessageLength] once the real ceiling is established.
const DefaultMaxMessageLength = 2000

// fieldRecipient is the request key naming the target group or contact.
const fieldRecipient = "recipient"

// maxChunks bounds a single SendChunked call. A megabyte of agent log would
// otherwise become hundreds of back-to-back messages in a group.
const maxChunks = 20

// pollOptionLimits are signal-cli's documented bounds on poll answers.
const (
	minPollOptions   = 2
	maxPollOptions   = 10
	maxPollOptionLen = 100
)

// APIError is a non-2xx response from signal-cli-rest-api.
type APIError struct {
	Status int
	Op     string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("signalcli: %s: http %d: %s", e.Op, e.Status, e.Body)
}

// Retryable reports whether repeating the request could plausibly succeed.
//
// This is a property of the response, not of the operation, and the two pull in
// opposite directions — so it is necessary but not sufficient:
//
//   - CreatePoll must never be retried automatically. The API offers no
//     idempotency key, so a retry posts a second votable approval poll. Even a
//     5xx may have dispatched the poll before failing.
//   - ClosePoll should be retried aggressively, including where this returns
//     false. It is idempotent, and not retrying leaves an approval mutable
//     after the merge it authorised.
//
// 501 is excluded: poll support is not in every build, and a server that lacks
// it will answer 501 forever.
func (e *APIError) Retryable() bool {
	if e.Status == http.StatusNotImplemented {
		return false
	}
	return e.Status == http.StatusRequestTimeout ||
		e.Status == http.StatusTooManyRequests ||
		e.Status >= http.StatusInternalServerError
}

// DeliveryError reports recipients the server accepted the request for but
// could not deliver to. signal-cli-rest-api answers 201 and lists these in the
// response body, so a send that reached nobody otherwise looks successful.
type DeliveryError struct {
	Timestamp int64
	Failures  []RecipientFailure
}

// RecipientFailure is one undelivered recipient and the server's reason.
type RecipientFailure struct {
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Reason string `json:"reason"`
}

func (e *DeliveryError) Error() string {
	reasons := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		reasons = append(reasons, f.Reason)
	}
	return fmt.Sprintf("signalcli: delivery failed for %d recipient(s): %s",
		len(e.Failures), strings.Join(reasons, ", "))
}

// PostedUnidentifiedError reports a poll the server accepted but whose timestamp
// could not be read. The poll is live and votable while its handle is unknown,
// so it can never be closed or correlated with a vote. Retrying would add a
// second votable approval poll, so callers must not.
type PostedUnidentifiedError struct {
	Op  string
	Raw string
}

// Retryable is always false: a retry would post a second votable approval poll.
func (e *PostedUnidentifiedError) Retryable() bool { return false }

func (e *PostedUnidentifiedError) Error() string {
	return fmt.Sprintf("signalcli: %s succeeded but returned no usable timestamp (%q); "+
		"a poll may be live and unclosable — do not retry", e.Op, e.Raw)
}

// sendResponse is the /v2/send body.
//
// The shape changed upstream: 0.100 serializes a single object, while builds
// after commit 6a225e6 serialize an array. Both are accepted so an image bump
// does not silently break every send. The errors field likewise exists only on
// the newer shape — on 0.100 a failed recipient surfaces as a 400 instead.
type sendResponse struct {
	Timestamp string `json:"timestamp"`
	Errors    *struct {
		Recipients []RecipientFailure `json:"recipients"`
	} `json:"errors,omitempty"`
}

// UnmarshalJSON accepts either the object or the single-element array form.
func (r *sendResponse) UnmarshalJSON(b []byte) error {
	type raw sendResponse
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		var many []raw
		if err := json.Unmarshal(b, &many); err != nil {
			return fmt.Errorf("decode send result array: %w", err)
		}
		if len(many) != 1 {
			return fmt.Errorf("expected exactly one send result, got %d", len(many))
		}
		*r = sendResponse(many[0])
		return nil
	}
	var one raw
	if err := json.Unmarshal(b, &one); err != nil {
		return fmt.Errorf("decode send result: %w", err)
	}
	*r = sendResponse(one)
	return nil
}

// Sender posts to signal-cli-rest-api on behalf of one account.
type Sender struct {
	baseURL   string
	account   string
	http      *http.Client
	maxMsgLen int
	styled    bool
}

// SenderOption configures a [Sender].
type SenderOption func(*Sender)

// WithStyledText sends messages with text_mode "styled", so Signal renders
// *italic*, **bold**, `monospace`, ~strikethrough~ and ||spoiler|| markers as
// text styles instead of literal characters. Outbound text is translated from
// markdown into that syntax first; without this option it is sent verbatim.
func WithStyledText() SenderOption { return func(s *Sender) { s.styled = true } }

// WithHTTPClient overrides the default client, which carries a 30s timeout.
func WithHTTPClient(h *http.Client) SenderOption { return func(s *Sender) { s.http = h } }

// WithMaxMessageLength sets the chunking threshold. Non-positive values are
// ignored: a zero threshold would split every message into empty pieces.
func WithMaxMessageLength(n int) SenderOption {
	return func(s *Sender) {
		if n > 0 {
			s.maxMsgLen = n
		}
	}
}

// NewSender builds a sender. baseURL is the service root, e.g.
// http://signal-rest-api.flimmerkiste.svc.cluster.local:80.
func NewSender(baseURL, account string, opts ...SenderOption) (*Sender, error) {
	if account == "" {
		return nil, errors.New("signalcli: account is required")
	}
	if baseURL == "" {
		return nil, errors.New("signalcli: base url is required")
	}
	s := &Sender{
		baseURL: strings.TrimRight(baseURL, "/"),
		account: account,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect rewrites DELETE into GET and drops the body, so a 2xx
			// from the target would read as a successful close that never
			// happened. The service is a fixed in-cluster address; it has no
			// business redirecting.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxMsgLen: DefaultMaxMessageLength,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// MaxMessageLength is the threshold beyond which [Sender.SendChunked] splits.
func (s *Sender) MaxMessageLength() int { return s.maxMsgLen }

// render translates markdown into Signal's styled-text syntax, and is the one
// place outbound text is rewritten: the public send methods call it exactly
// once, so no text is translated twice or escaped twice.
func (s *Sender) render(text string) string {
	if !s.styled {
		return text
	}
	return styledText(text)
}

// SendText delivers one message and returns its timestamp, which is the handle
// used to react to or quote it later.
func (s *Sender) SendText(ctx context.Context, recipient, text string) (int64, error) {
	return s.send(ctx, recipient, s.render(text))
}

func (s *Sender) send(ctx context.Context, recipient, text string) (int64, error) {
	if recipient == "" {
		return 0, errors.New("signalcli: recipient is required")
	}
	if text == "" {
		return 0, errors.New("signalcli: message is empty")
	}
	payload := map[string]any{
		"number":     s.account,
		"recipients": []string{recipient},
		"message":    text,
	}
	if s.styled {
		payload["text_mode"] = "styled"
	}
	var out sendResponse
	err := s.do(ctx, http.MethodPost, "/v2/send", payload, &out)
	if err != nil {
		return 0, err
	}
	ts, err := strconv.ParseInt(out.Timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return 0, &PostedUnidentifiedError{Op: "send", Raw: out.Timestamp}
	}
	// A 201 does not mean delivered: the server reports per-recipient failures
	// in the body, so a message that reached nobody looks successful otherwise.
	if out.Errors != nil && len(out.Errors.Recipients) > 0 {
		return ts, &DeliveryError{Timestamp: ts, Failures: out.Errors.Recipients}
	}
	return ts, nil
}

// SendChunked splits text at the message limit and sends the pieces in order,
// stopping at the first failure so a caller is never told a partial delivery
// succeeded. Splitting prefers line boundaries, since agent output is mostly
// logs and diffs where a mid-line break destroys readability.
func (s *Sender) SendChunked(ctx context.Context, recipient, text string) ([]int64, error) {
	// Split the rendered text, so escaping cannot push a chunk past the limit.
	chunks := splitMessage(s.render(text), s.maxMsgLen)
	if len(chunks) > maxChunks {
		return nil, fmt.Errorf("signalcli: %d chunks exceeds the %d limit; "+
			"truncate or link the output instead of flooding the group",
			len(chunks), maxChunks)
	}
	stamps := make([]int64, 0, len(chunks))
	for i, chunk := range chunks {
		ts, err := s.send(ctx, recipient, chunk)
		if err != nil {
			return stamps, fmt.Errorf("signalcli: chunk %d of %d: %w", i+1, len(chunks), err)
		}
		stamps = append(stamps, ts)
	}
	return stamps, nil
}

// React adds an emoji reaction to a message, identified by its author and
// timestamp.
func (s *Sender) React(ctx context.Context, recipient, targetAuthor string, targetTimestamp int64, emoji string) error {
	if recipient == "" {
		return errors.New("signalcli: recipient is required")
	}
	if emoji == "" || targetAuthor == "" || targetTimestamp == 0 {
		return errors.New("signalcli: react requires emoji, target author and timestamp")
	}
	return s.do(ctx, http.MethodPost, "/v1/reactions/"+s.account, map[string]any{
		fieldRecipient:  recipient,
		"reaction":      emoji,
		"target_author": targetAuthor,
		"timestamp":     targetTimestamp,
	}, nil)
}

// CreatePoll posts a poll and returns its timestamp, which is the key a later
// vote refers to via PollVote.TargetSentTimestamp and the handle ClosePoll
// needs.
func (s *Sender) CreatePoll(ctx context.Context, recipient, question string, options []string, allowMultiple bool) (int64, error) {
	if recipient == "" {
		return 0, errors.New("signalcli: recipient is required")
	}
	if question == "" {
		return 0, errors.New("signalcli: poll question is required")
	}
	if len(options) < minPollOptions || len(options) > maxPollOptions {
		return 0, fmt.Errorf("signalcli: poll needs %d-%d options, got %d",
			minPollOptions, maxPollOptions, len(options))
	}
	for _, o := range options {
		if o == "" || utf8.RuneCountInString(o) > maxPollOptionLen {
			return 0, fmt.Errorf("signalcli: poll option %q must be 1-%d characters", o, maxPollOptionLen)
		}
	}
	var out struct {
		Timestamp string `json:"timestamp"`
	}
	err := s.do(ctx, http.MethodPost, "/v1/polls/"+s.account, map[string]any{
		fieldRecipient:              recipient,
		"question":                  question,
		"answers":                   options,
		"allow_multiple_selections": allowMultiple,
	}, &out)
	if err != nil {
		return 0, err
	}
	// A zero timestamp parses cleanly but is not a usable handle: ClosePoll
	// rejects it, so the poll would be live, votable and unclosable forever.
	ts, err := strconv.ParseInt(out.Timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return 0, &PostedUnidentifiedError{Op: "create poll", Raw: out.Timestamp}
	}
	return ts, nil
}

// ClosePoll finalises a poll. Signal permits changing an answer while a poll is
// open, so closing on the first vote is what makes an approval final — verified
// against a live deployment: after closing, the client refuses to switch and no
// further vote frame is emitted.
func (s *Sender) ClosePoll(ctx context.Context, recipient string, pollTimestamp int64) error {
	if recipient == "" {
		return errors.New("signalcli: recipient is required")
	}
	if pollTimestamp == 0 {
		return errors.New("signalcli: poll timestamp is required")
	}
	// The spec documents exactly 204 for a successful close. Accepting any 2xx
	// would let a redirect-rewritten GET, or a proxy that swallowed the DELETE
	// body, report an approval as immutable while the poll is still open and
	// every voter can still switch.
	return s.doExpect(ctx, http.MethodDelete, "/v1/polls/"+s.account, map[string]any{
		fieldRecipient:   recipient,
		"poll_timestamp": strconv.FormatInt(pollTimestamp, 10),
	}, nil, http.StatusNoContent)
}

func (s *Sender) do(ctx context.Context, method, path string, payload, out any) error {
	return s.doExpect(ctx, method, path, payload, out, 0)
}

// doExpect issues the request, requiring exactly wantStatus when non-zero.
func (s *Sender) doExpect(ctx context.Context, method, path string, payload, out any, wantStatus int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("signalcli: encode %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("signalcli: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("signalcli: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("signalcli: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Op: method + " " + path, Body: truncate(strings.TrimSpace(string(raw)))}
	}
	if wantStatus != 0 && resp.StatusCode != wantStatus {
		return &APIError{
			Status: resp.StatusCode,
			Op:     method + " " + path,
			Body:   fmt.Sprintf("expected %d, got %d — treating as not applied", wantStatus, resp.StatusCode),
		}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// The server accepted the request; only the reply was unreadable. The
		// caller must not treat this as "nothing happened".
		return &PostedUnidentifiedError{Op: method + " " + path, Raw: truncate(string(raw))}
	}
	return nil
}

// splitMessage breaks text into pieces no longer than limit runes, preferring
// line boundaries and falling back to a hard cut for a single oversized line.
func splitMessage(text string, limit int) []string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	var chunks []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		for utf8.RuneCountInString(line) > limit {
			flush()
			cut := runePrefix(line, limit)
			chunks = append(chunks, cut)
			line = line[len(cut):]
		}
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > limit {
			flush()
		}
		b.WriteString(line)
	}
	flush()
	return chunks
}

// truncate bounds server-controlled text before it reaches an error string.
func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func runePrefix(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
