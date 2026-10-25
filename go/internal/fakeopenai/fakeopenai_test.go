package fakeopenai

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A request addressed anywhere but the test server is refused, never sent over plain HTTP.
func TestClientRefusesAnotherHost(t *testing.T) {
	hs := WebSocket(t, func(*websocket.Conn) {}, nil)
	c, _ := Client(t, hs.URL)
	body := &closeRecorder{Reader: strings.NewReader("x")}
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/x", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer x")
	if resp, err := c.Transport.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a request to another host was sent")
	}
	if !body.closed {
		t.Error("the refused request's body was not closed")
	}
}

type closeRecorder struct {
	*strings.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// A connection the client never closes is closed by the test's cleanup, so a handler blocked reading it returns.
func TestCleanupClosesAnOpenWebSocket(t *testing.T) {
	returned := make(chan struct{})
	t.Run("server", func(t *testing.T) {
		accepted := make(chan struct{})
		hs := WebSocket(t, func(c *websocket.Conn) {
			defer close(returned)
			close(accepted)
			_, _, _ = c.Read(context.Background())
		}, nil)
		conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn // never closed
		<-accepted
	})
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still reading after cleanup")
	}
}
