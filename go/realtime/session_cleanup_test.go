package realtime_test

import (
	"context"
	"runtime"
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
