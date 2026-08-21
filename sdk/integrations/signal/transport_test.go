package signal_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qf-studio/studio-sdk/sdk/integrations/signal"
)

const testAccount = "+490000000001"

// wsServer serves the receive endpoint. Each connection is handed to handle,
// which returns when that connection should end.
func wsServer(t *testing.T, handle func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/v1/receive/" + testAccount; r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func quietLogger() signal.ReceiverOption {
	return signal.WithLogger(slog.New(slog.DiscardHandler))
}

func TestRunDeliversFrames(t *testing.T) {
	t.Parallel()

	msg := fixtureBytes(t, "group_data_message.json")
	srv := wsServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, msg)
		time.Sleep(50 * time.Millisecond)
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	got := make(chan signal.Frame, 1)
	go func() {
		_ = c.Run(ctx, func(f signal.Frame) {
			select {
			case got <- f:
			default:
			}
		})
	}()

	select {
	case f := <-got:
		if f.Envelope.Kind() != signal.KindData {
			t.Errorf("Kind = %q", f.Envelope.Kind())
		}
	case <-ctx.Done():
		t.Fatal("no frame delivered")
	}
}

// The upstream API drops messages arriving while no client is attached, so a
// dropped connection must be re-established rather than ending the stream.
func TestRunReconnectsAfterDrop(t *testing.T) {
	t.Parallel()

	msg := fixtureBytes(t, "group_data_message.json")
	var mu sync.Mutex
	var conns int

	srv := wsServer(t, func(conn *websocket.Conn) {
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()
		if n == 1 {
			return // hang up immediately, forcing a reconnect
		}
		_ = conn.WriteMessage(websocket.TextMessage, msg)
		time.Sleep(50 * time.Millisecond)
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
		signal.WithBackoff(time.Millisecond, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	got := make(chan struct{}, 1)
	go func() {
		_ = c.Run(ctx, func(signal.Frame) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
	}()

	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("no frame after reconnect")
	}

	mu.Lock()
	defer mu.Unlock()
	if conns < 2 {
		t.Errorf("connections = %d, want at least 2", conns)
	}
}

func TestRunReportsReconnects(t *testing.T) {
	t.Parallel()

	srv := wsServer(t, func(*websocket.Conn) {})

	var mu sync.Mutex
	var attempts []int
	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
		signal.WithBackoff(time.Millisecond, 5*time.Millisecond),
		signal.WithOnReconnect(func(attempt int, _ time.Duration, _ error) {
			mu.Lock()
			attempts = append(attempts, attempt)
			mu.Unlock()
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx, func(signal.Frame) {})

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) == 0 {
		t.Fatal("no reconnect reported; a loss window would be invisible")
	}
	if attempts[0] != 1 {
		t.Errorf("first attempt = %d, want 1", attempts[0])
	}
}

// A protocol-error close repeats identically on every retry, so the loop must
// surface it instead of spinning.
func TestRunStopsOnFatalClose(t *testing.T) {
	t.Parallel()

	srv := wsServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseProtocolError, "nope"),
			time.Now().Add(time.Second))
		time.Sleep(20 * time.Millisecond)
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
		signal.WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	err = c.Run(ctx, func(signal.Frame) {})
	if err == nil {
		t.Fatal("fatal close did not stop the loop")
	}
	var fatal *signal.FatalCloseError
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v, want FatalCloseError", err)
	}
	if fatal.Code != websocket.CloseProtocolError {
		t.Errorf("Code = %d", fatal.Code)
	}
}

func TestRunStopsOnRejectedHandshake(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
		signal.WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	if err := c.Run(ctx, func(signal.Frame) {}); err == nil {
		t.Fatal("rejected handshake did not stop the loop")
	}
}

// A frame this package cannot parse must not tear down a healthy connection.
func TestRunSkipsUnparseableFrames(t *testing.T) {
	t.Parallel()

	good := fixtureBytes(t, "group_data_message.json")
	srv := wsServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{not json"))
		_ = conn.WriteMessage(websocket.TextMessage, good)
		time.Sleep(50 * time.Millisecond)
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	got := make(chan signal.Frame, 1)
	go func() {
		_ = c.Run(ctx, func(f signal.Frame) {
			select {
			case got <- f:
			default:
			}
		})
	}()

	select {
	case f := <-got:
		if f.Envelope.Kind() != signal.KindData {
			t.Errorf("Kind = %q", f.Envelope.Kind())
		}
	case <-ctx.Done():
		t.Fatal("good frame not delivered after a malformed one")
	}
}

func TestRunReturnsNilOnContextCancel(t *testing.T) {
	t.Parallel()

	srv := wsServer(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	if err := c.Run(ctx, func(signal.Frame) {}); err != nil {
		t.Errorf("Run = %v, want nil on cancel", err)
	}
}

func TestNewValidatesInput(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, base, account string }{
		{"bad scheme", "ftp://host", testAccount},
		{"empty account", "http://host", ""},
		{"unparseable url", "://", testAccount},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := signal.NewReceiver(tc.base, tc.account); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNewBuildsReceiveURL(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = c.Run(ctx, func(signal.Frame) {})

	if want := "/v1/receive/" + testAccount; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// A 404 from an in-cluster Service whose pod is still starting, and a 429 rate
// limit, are transient. Treating either as terminal would strand the daemon with
// every subsequent message dropped.
func TestRunRetriesTransientHandshakeRejections(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				hits++
				mu.Unlock()
				w.WriteHeader(status)
			}))
			t.Cleanup(srv.Close)

			c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
				signal.WithBackoff(time.Millisecond, 5*time.Millisecond))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			if err := c.Run(ctx, func(signal.Frame) {}); err != nil {
				t.Fatalf("Run = %v, want nil (transient, keep retrying)", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if hits < 2 {
				t.Errorf("dial attempts = %d, want repeated retries", hits)
			}
		})
	}
}

// A policy-violation close is what proxies emit for rate limits and idle
// timeouts, so it must reconnect rather than stop permanently.
func TestRunRetriesPolicyViolationClose(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var conns int
	srv := wsServer(t, func(conn *websocket.Conn) {
		mu.Lock()
		conns++
		mu.Unlock()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "slow down"),
			time.Now().Add(time.Second))
		time.Sleep(10 * time.Millisecond)
	})

	c, err := signal.NewReceiver(srv.URL, testAccount, quietLogger(),
		signal.WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()

	if err := c.Run(ctx, func(signal.Frame) {}); err != nil {
		t.Fatalf("Run = %v, want nil (1008 is transient)", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if conns < 2 {
		t.Errorf("connections = %d, want reconnects", conns)
	}
}

// Invalid bounds must be ignored rather than applied: minDelay=0 previously fell
// through to the ceiling, and a negative maxDelay panicked in the jitter.
func TestWithBackoffRejectsInvalidBounds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		lo, hi time.Duration
	}{
		{"zero floor", 0, 30 * time.Second},
		{"negative ceiling", time.Second, -time.Second},
		{"inverted", 30 * time.Second, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := signal.NewReceiver("http://host", testAccount,
				signal.WithBackoff(tc.lo, tc.hi)); err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}
