package realtime_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/internal/parked"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
)

// A configuration the library cannot honour is internal: a read limit it would wrap, a scheme it cannot dial.
func TestDialRefusesUnusableConfiguration(t *testing.T) {
	c, base := fakeopenai.Client(t, "http://127.0.0.1:1")
	cfg := config(c, base)
	cfg.ReadLimit = math.MaxInt64
	_, err := realtime.Dial(context.Background(), cfg)
	wantClass(t, err, openaihttp.ClassInternal)
	cfg = config(c, base)
	cfg.URL = "ftp://api.openai.com/v1/realtime?model=m"
	_, err = realtime.Dial(context.Background(), cfg)
	wantClass(t, err, openaihttp.ClassInternal)
}

// A refusal whose body never arrives returns at once: it is classified by status, and its body is not read.
func TestStalledRefusalBodyIsNotRead(t *testing.T) {
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusForbidden)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(func() { close(release); hs.Close() })
	c, base := fakeopenai.Client(t, hs.URL)
	start := time.Now()
	_, err := realtime.Dial(context.Background(), config(c, base))
	wantClass(t, err, openaihttp.ClassRouteDenied)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Dial took %v", d)
	}
}

// The caller's cancellation during the handshake is cancelled, not the class of a reply that arrives anyway.
func TestDialKeepsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(hs.Close)
	c, base := fakeopenai.Client(t, hs.URL)
	_, err := realtime.Dial(ctx, config(c, base))
	wantClass(t, err, openaihttp.ClassCancelled)
}

// Next with a cancelled context returns at once and leaves a pending event for the next call.
func TestNextCancelledKeepsThePendingEvent(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) {
		send(c, `{"type":"input_audio_buffer.committed","item_id":"kept"}`)
		_, _, _ = c.Read(context.Background())
	}, nil)
	parked.Wait(t, "realtime.(*sessionCore).readLoop") // the reader is offering the event
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if _, err := s.Next(cancelled); err != context.Canceled {
			t.Fatalf("Next(cancelled) = %v", err)
		}
	}
	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ev.(realtime.InputCommitted); !ok || got.ItemID != "kept" {
		t.Fatalf("got %#v", ev)
	}
}

// After a connection failure, Close makes Next return ErrClosed, as Send does.
func TestNextAfterFailureThenCloseIsClosed(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) {
		_ = c.Close(websocket.StatusInternalError, "gone")
	}, nil)
	if _, err := s.Next(context.Background()); err == nil {
		t.Fatal("Next succeeded on a closed connection")
	}
	_ = s.Close()
	if _, err := s.Next(context.Background()); err != realtime.ErrClosed {
		t.Fatalf("Next after Close = %v", err)
	}
}
