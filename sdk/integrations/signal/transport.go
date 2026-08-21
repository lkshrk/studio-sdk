package signal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Reconnect timing. The floor is deliberately small: signal-cli-rest-api drops
// messages that arrive while no client is attached, so every millisecond
// disconnected is potential data loss rather than mere latency.
const (
	defaultMinBackoff  = 250 * time.Millisecond
	defaultMaxBackoff  = 30 * time.Second
	defaultDialTimeout = 15 * time.Second

	// A half-open socket — pod eviction, conntrack expiry, a silent partition —
	// leaves ReadMessage blocked forever with no error, so the reconnect loop
	// never fires and the stream looks idle while every message is dropped. The
	// deadline is the only thing that turns that into a detectable disconnect.
	pingInterval = 30 * time.Second
	readTimeout  = 70 * time.Second
	writeTimeout = 10 * time.Second

	// A connection that survived this long counts as healthy even if it carried
	// no traffic. Duration alone is a poor health signal in both directions, so
	// it is only the fallback — observed activity is the primary one.
	stabilityWindow = 2 * readTimeout

	// gorilla reads an entire message into memory with no default cap.
	maxFrameBytes = 4 << 20
)

// FatalCloseError reports a disconnect that reconnecting cannot fix.
type FatalCloseError struct {
	Code   int
	Reason string
}

func (e *FatalCloseError) Error() string {
	return fmt.Sprintf("signalcli: fatal websocket close %d: %s", e.Code, e.Reason)
}

// fatalCloseCodes are protocol-level rejections that repeat identically on every
// retry. The set is deliberately narrow: a wrongly-fatal code costs every future
// message, while a wrongly-recoverable one costs a single retry. Notably absent
// is ClosePolicyViolation, which proxies also use for rate limits and idle
// timeouts, and CloseTLSHandshake, which is reserved and never sent by a peer.
var fatalCloseCodes = map[int]bool{
	websocket.CloseProtocolError:           true,
	websocket.CloseUnsupportedData:         true,
	websocket.CloseInvalidFramePayloadData: true,
	websocket.CloseMandatoryExtension:      true,
}

// fatalHandshakeStatuses reject the connection for reasons a retry cannot change.
// 404 is excluded on purpose: an in-cluster Service whose pod is still starting
// answers 404 for a few seconds, and treating that as terminal would strand the
// daemon. 408 and 429 are retryable by definition.
var fatalHandshakeStatuses = map[int]bool{
	http.StatusBadRequest:     true,
	http.StatusUnauthorized:   true,
	http.StatusForbidden:      true,
	http.StatusNotImplemented: true,
}

// Receiver streams received frames from signal-cli-rest-api in json-rpc mode.
type Receiver struct {
	receiveURL  string
	dialer      *websocket.Dialer
	log         *slog.Logger
	minBackoff  time.Duration
	maxBackoff  time.Duration
	onReconnect func(attempt int, connectedFor time.Duration, cause error)
}

// ReceiverOption configures a [Receiver].
type ReceiverOption func(*Receiver)

// WithLogger sets the logger. Disconnects are always logged: a silent one is
// indistinguishable from an idle stream.
func WithLogger(l *slog.Logger) ReceiverOption { return func(c *Receiver) { c.log = l } }

// WithBackoff bounds the reconnect delay. Non-positive or inverted bounds are
// ignored rather than applied: asking for no floor previously yielded the
// ceiling, which is the opposite of what a caller tuning for low latency wants.
func WithBackoff(minDelay, maxDelay time.Duration) ReceiverOption {
	return func(c *Receiver) {
		if minDelay <= 0 || maxDelay < minDelay {
			return
		}
		c.minBackoff, c.maxBackoff = minDelay, maxDelay
	}
}

// WithOnReconnect observes every reconnect, reporting how long the previous
// connection lasted. Repeated short-lived connections are the signature of a
// loss window that would otherwise be invisible.
func WithOnReconnect(fn func(attempt int, connectedFor time.Duration, cause error)) ReceiverOption {
	return func(c *Receiver) { c.onReconnect = fn }
}

// New builds a client for one account. baseURL is the service root, e.g.
// http://signal-rest-api.flimmerkiste.svc.cluster.local:80.
func NewReceiver(baseURL, account string, opts ...ReceiverOption) (*Receiver, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("signalcli: parse base url: %w", err)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("signalcli: unsupported scheme %q", u.Scheme)
	}
	if account == "" {
		return nil, errors.New("signalcli: account is required")
	}
	u.Path = "/v1/receive/" + account

	c := &Receiver{
		receiveURL: u.String(),
		dialer:     &websocket.Dialer{HandshakeTimeout: defaultDialTimeout},
		log:        slog.Default(),
		minBackoff: defaultMinBackoff,
		maxBackoff: defaultMaxBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Run streams frames to onFrame until ctx is cancelled or a fatal close occurs.
// It reconnects on any recoverable disconnect. Frames arriving while
// disconnected are lost — upstream does not queue them — so the loop reconnects
// eagerly and reports each gap.
func (c *Receiver) Run(ctx context.Context, onFrame func(Frame)) error {
	var attempt, consecutive int
	for {
		connectedFor, active, err := c.stream(ctx, onFrame)

		// Cancellation is a clean stop: the stream error it caused is expected.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		var fatal *FatalCloseError
		if errors.As(err, &fatal) {
			c.log.Error("signal receive stream stopped", "err", err)
			return err
		}

		// Backoff follows consecutive failures, not lifetime ones. Sharing one
		// counter meant a daemon up for a week waited the full cap after a
		// single blip — and that wait is dropped messages, not just latency.
		//
		// Health is judged on observed traffic rather than duration. A proxy
		// that recycles a working connection every minute would never reach the
		// duration window and would sit permanently at the cap, turning every
		// routine reconnect into a loss window; conversely a silent peer
		// survives to the read deadline while carrying nothing.
		if active || connectedFor >= stabilityWindow {
			consecutive = 0
		}
		attempt++
		consecutive++

		delay := c.backoff(consecutive)
		c.log.Warn("signal receive stream dropped; reconnecting",
			"attempt", attempt, "consecutive", consecutive,
			"connected_for", connectedFor, "delay", delay, "cause", err)
		if c.onReconnect != nil {
			c.onReconnect(attempt, connectedFor, err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// stream holds one connection until it fails, returning how long the connection
// itself lasted. That duration excludes dialing: a refused dial and a socket
// that genuinely lived 15 seconds must not report the same number, because
// distinguishing them is the whole point of the reconnect callback.
func (c *Receiver) stream(ctx context.Context, onFrame func(Frame)) (time.Duration, bool, error) {
	conn, resp, err := c.dialer.DialContext(ctx, c.receiveURL, nil)
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			if fatalHandshakeStatuses[resp.StatusCode] {
				return 0, false, &FatalCloseError{Code: resp.StatusCode, Reason: "handshake rejected"}
			}
		}
		return 0, false, fmt.Errorf("signalcli: dial: %w", err)
	}
	connectedAt := time.Now()
	defer func() { _ = conn.Close() }()

	// AfterFunc unregisters on return, so an ordinary disconnect does not leave
	// a goroutine parked on ctx for the life of the daemon.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopWatch()

	done := make(chan struct{})
	defer close(done)

	// active records that the peer proved responsive on this connection, by
	// either a frame or a pong. It is the health signal the backoff reset uses.
	var active atomic.Bool

	conn.SetReadLimit(maxFrameBytes)
	extend := func() { _ = conn.SetReadDeadline(time.Now().Add(readTimeout)) }
	extend()
	conn.SetPongHandler(func(string) error {
		active.Store(true)
		extend()
		return nil
	})

	go c.pingLoop(conn, done)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) && fatalCloseCodes[ce.Code] {
				return time.Since(connectedAt), active.Load(), &FatalCloseError{Code: ce.Code, Reason: ce.Text}
			}
			return time.Since(connectedAt), active.Load(), fmt.Errorf("signalcli: read: %w", err)
		}
		active.Store(true)
		extend()

		frame, err := ParseFrame(data)
		if err != nil {
			// One malformed frame must not tear down a healthy connection; the
			// stream carries kinds this package does not model yet.
			c.log.Warn("skipping unparseable frame", "err", err)
			continue
		}
		onFrame(frame)
	}
}

// pingLoop keeps the read deadline alive on an otherwise idle stream. Without
// it, an idle connection would be indistinguishable from a wedged one.
func (c *Receiver) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}

// backoff grows exponentially to a cap, with jitter so repeated drops do not
// resynchronise into a thundering herd.
func (c *Receiver) backoff(attempt int) time.Duration {
	d := c.minBackoff << min(attempt-1, 16)
	if d > c.maxBackoff || d <= 0 {
		d = c.maxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}
