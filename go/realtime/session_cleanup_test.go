package realtime_test

import (
	"context"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/realtime"
)

// A session dropped without Close is closed when it is collected: the peer sees the close, and neither the reader
// goroutine nor the connection outlives it.
func TestDroppedSessionIsClosedWhenCollected(t *testing.T) {
	ended := make(chan error, 1)
	hs := fakeopenai.WebSocket(t, func(c *websocket.Conn) {
		_, _, err := c.Read(context.Background()) // until the client closes
		ended <- err
	}, nil)
	c, base := fakeopenai.Client(t, hs.URL)
	// The goroutines running before the dial (the server's) are ignored, so only what the session started counts.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dropped := func() {
		s, err := realtime.Dial(context.Background(), config(c, base))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Unknown() // used, then dropped without Close
	}
	dropped()
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		select {
		case err := <-ended:
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("the peer saw %v, want a normal close", err)
			}
			c.CloseIdleConnections()
			return
		case <-deadline:
			t.Fatal("a dropped session was never closed")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// countHandler counts the records logged with message msg.
type countHandler struct {
	msg string
	n   *atomic.Int32
}

func (h countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.n.Add(1)
	}
	return nil
}
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countHandler) WithGroup(string) slog.Handler      { return h }

// A session closed before it is dropped is not closed again when collected: only the one dropped open is.
func TestClosedSessionIsNotClosedAgainWhenCollected(t *testing.T) {
	hs := fakeopenai.WebSocket(t, func(c *websocket.Conn) { _, _, _ = c.Read(context.Background()) }, nil)
	c, base := fakeopenai.Client(t, hs.URL)
	const msg = "realtime: a session was dropped without Close; closing it"
	var closedLogs, openLogs atomic.Int32
	dial := func(n *atomic.Int32) *realtime.Session {
		cfg := config(c, base)
		cfg.Logger = slog.New(countHandler{msg, n})
		s, err := realtime.Dial(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	func() {
		_ = dial(&closedLogs).Close()
		_ = dial(&openLogs).Unknown() // the control: dropped open
	}()
	deadline := time.After(10 * time.Second)
	for openLogs.Load() == 0 {
		runtime.GC()
		select {
		case <-deadline:
			t.Fatal("the session dropped open was never closed")
		case <-time.After(20 * time.Millisecond):
		}
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	if n := closedLogs.Load(); n != 0 {
		t.Errorf("a closed session was closed again when collected (%d logs)", n)
	}
}
