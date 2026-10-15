package realtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

const readLimit = 16 << 20

func config(c *http.Client, base string) realtime.DialConfig {
	return realtime.DialConfig{
		Client: c, URL: realtime.WebSocketURL(base, "m"), Secret: secret.NewClientSecret("ek_canary", time.Unix(1800000000, 0)),
		HandshakeTimeout: 5 * time.Second, ReadLimit: readLimit,
	}
}

// dialHandler opens a session to a test server running fn on the accepted conn.
func dialHandler(t *testing.T, fn func(*websocket.Conn), headers func(http.Header)) *realtime.Session {
	t.Helper()
	hs := fakeopenai.WebSocket(t, fn, headers)
	c, base := fakeopenai.Client(t, hs.URL)
	s, err := realtime.Dial(context.Background(), config(c, base))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func wantClass(t testing.TB, err error, class openaihttp.Class) {
	t.Helper()
	var e *openaihttp.Error
	if !errors.As(err, &e) || openaihttp.Classify(err) != class {
		t.Fatalf("err = %v, want class %s", err, class)
	}
}

func send(c *websocket.Conn, s string) {
	_ = c.Write(context.Background(), websocket.MessageText, []byte(s))
}

// The upgrade carries the client secret, and audio deltas stream until response.done.
func TestUpgradeCarriesTheSecret(t *testing.T) {
	var mu sync.Mutex
	var auth string
	s := dialHandler(t, func(c *websocket.Conn) {
		for range 5 {
			send(c, `{"type":"response.output_audio.delta","response_id":"r","delta":"AAEC"}`)
		}
		send(c, `{"type":"response.done","response":{"id":"r","status":"completed"}}`)
		_, _, _ = c.Read(context.Background())
	}, func(h http.Header) { mu.Lock(); auth = h.Get("Authorization"); mu.Unlock() })
	for {
		ev, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(realtime.ResponseDone); ok {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer ek_canary" {
		t.Fatalf("upgrade Authorization %q", auth)
	}
}

func TestContextCancelUnblocksRead(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) {
		if _, _, err := c.Read(context.Background()); err != nil { // wait for a client event
			return
		}
		send(c, `{"type":"response.done","response":{"id":"r","status":"completed"}}`)
		_, _, _ = c.Read(context.Background())
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx)
		errc <- err
	}()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read not unblocked within 2 s")
	}
	// The session survives a cancelled read.
	if err := s.Send(context.Background(), map[string]string{"type": "response.create"}); err != nil {
		t.Fatal(err)
	}
	if ev, err := s.Next(context.Background()); err != nil || ev.EventType() != realtime.TypeResponseDone {
		t.Fatalf("after a cancelled read: %v %v", ev, err)
	}
}

func TestCloseWithSilentPeerDropsTheConnection(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-release // never read, so the close frame is never acknowledged
		_ = c.CloseNow()
	}))
	c, base := fakeopenai.Client(t, hs.URL)
	s, err := realtime.Dial(context.Background(), config(c, base))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("Close took %v, want ≤ 2.5 s", d)
	}
	if _, err := s.Next(context.Background()); !errors.Is(err, realtime.ErrClosed) {
		t.Fatalf("Next after Close: %v", err)
	}
	if err := s.Send(context.Background(), map[string]string{"type": "x"}); !errors.Is(err, realtime.ErrClosed) {
		t.Fatalf("Send after Close: %v", err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	close(release)
	hs.Close()
	c.CloseIdleConnections()
}

// Ignored and unknown events are delivered with their raw bytes; unknown ones are also counted.
func TestUnknownCountedAndDelivered(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) {
		send(c, `{"type":"session.updated"}`)
		send(c, `{"type":"response.future_thing"}`)
		send(c, `{"type":"response.future_thing"}`)
		send(c, `{"type":"response.done","response":{"id":"r","status":"completed"}}`)
		_, _, _ = c.Read(context.Background())
	}, nil)
	want := []string{"session.updated", "response.future_thing", "response.future_thing", realtime.TypeResponseDone}
	for i, typ := range want {
		ev, err := s.Next(context.Background())
		if err != nil || ev.EventType() != typ {
			t.Fatalf("event %d: got %v %v, want %s", i, ev, err, typ)
		}
		switch e := ev.(type) {
		case realtime.Ignored:
			if len(e.Raw) == 0 {
				t.Fatal("an ignored event lost its raw bytes")
			}
		case realtime.Unknown:
			if len(e.Raw) == 0 {
				t.Fatal("an unknown event lost its raw bytes")
			}
		}
	}
	if got := s.Unknown(); got["response.future_thing"] != 2 || len(got) != 1 {
		t.Fatalf("unknown counts %v", got)
	}
}

// A malformed event is a protocol error that does not end the session.
func TestMalformedEventDoesNotEndTheSession(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) {
		send(c, `{"type":"response.done"`)
		send(c, `{"type":"response.done","response":{"id":"r","status":"completed"}}`)
		_, _, _ = c.Read(context.Background())
	}, nil)
	_, err := s.Next(context.Background())
	wantClass(t, err, openaihttp.ClassProtocol)
	if ev, err := s.Next(context.Background()); err != nil || ev.EventType() != realtime.TypeResponseDone {
		t.Fatalf("after a malformed event: %v %v", ev, err)
	}
}

func TestSessionClassifiesWebSocketFailures(t *testing.T) {
	t.Run("close 1011 is server", func(t *testing.T) {
		s := dialHandler(t, func(c *websocket.Conn) {
			_ = c.Close(websocket.StatusInternalError, "keepalive ping timeout ek_canary")
		}, nil)
		_, err := s.Next(context.Background())
		wantClass(t, err, openaihttp.ClassServer)
		if strings.Contains(err.Error(), "ek_") {
			t.Fatal("secret in error")
		}
	})
	t.Run("abrupt drop is network", func(t *testing.T) {
		s := dialHandler(t, func(c *websocket.Conn) { _ = c.CloseNow() }, nil)
		_, err := s.Next(context.Background())
		wantClass(t, err, openaihttp.ClassNetwork)
	})
	t.Run("handshake 403 is route-denied", func(t *testing.T) {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"no","type":"invalid_request_error","code":"missing_scope","param":null}}`))
		}))
		t.Cleanup(hs.Close)
		c, base := fakeopenai.Client(t, hs.URL)
		cfg := config(c, base)
		cfg.Route = "speech"
		_, err := realtime.Dial(context.Background(), cfg)
		wantClass(t, err, openaihttp.ClassRouteDenied)
		var e *openaihttp.Error
		if errors.As(err, &e); e.Route != "speech" {
			t.Fatalf("route %q", e.Route)
		}
	})
}

func TestReadLimitAndSerialisedWrites(t *testing.T) {
	big := strings.Repeat("A", 12<<20) // base64 of 9 MiB, under the 16 MiB limit
	var mu sync.Mutex
	got := 0
	s := dialHandler(t, func(c *websocket.Conn) {
		ctx := context.Background()
		send(c, `{"type":"response.output_audio.delta","response_id":"r","delta":"`+big+`"}`)
		over := `{"type":"x","pad":"` + strings.Repeat("A", readLimit) + `"}`
		for range 8 {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
			mu.Lock()
			got++
			mu.Unlock()
		}
		send(c, over)
		_, _, _ = c.Read(ctx)
	}, nil)
	ev, err := s.Next(context.Background())
	if d, ok := ev.(realtime.AudioDelta); err != nil || !ok || len(d.PCM) != 9<<20 {
		t.Fatalf("large message: %T %v", ev, err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := s.Send(context.Background(), map[string]any{"type": "input_audio_buffer.append", "n": i}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	_, err = s.Next(context.Background())
	wantClass(t, err, openaihttp.ClassProtocol)
	mu.Lock()
	defer mu.Unlock()
	if got != 8 {
		t.Fatalf("server read %d of 8 concurrent writes", got)
	}
}

// Dial and the Minter refuse a client that does not send through the origin policy, and missing required limits.
func TestRequiredArguments(t *testing.T) {
	plain := &http.Client{}
	cfg := config(plain, openaihttp.APIBase)
	_, err := realtime.Dial(context.Background(), cfg)
	wantClass(t, err, openaihttp.ClassInternal)
	c, err := openaihttp.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*realtime.DialConfig){
		func(d *realtime.DialConfig) { d.ReadLimit = 0 },
		func(d *realtime.DialConfig) { d.HandshakeTimeout = 0 },
	} {
		cfg := config(c, openaihttp.APIBase)
		mut(&cfg)
		_, err := realtime.Dial(context.Background(), cfg)
		wantClass(t, err, openaihttp.ClassInternal)
	}
	m := &realtime.Minter{Client: plain, BaseURL: openaihttp.APIBase, Creds: nil, Timeout: time.Second, Lifetime: time.Minute}
	_, err = m.Secret(context.Background(), map[string]string{"type": "realtime"})
	wantClass(t, err, openaihttp.ClassInternal)
}

func TestMintBody(t *testing.T) {
	b, err := realtime.MintBody(map[string]any{"type": "realtime", "instructions": "<<<x>>> & y"}, 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session":{"instructions":"<<<x>>> & y","type":"realtime"},"expires_after":{"anchor":"created_at","seconds":600}}`
	if string(b) != want {
		t.Fatalf("body %s\nwant %s", b, want)
	}
	for _, bad := range []time.Duration{0, -time.Second, 1500 * time.Millisecond} {
		if _, err := realtime.MintBody(map[string]any{}, bad); err == nil {
			t.Errorf("lifetime %v accepted", bad)
		}
	}
	if _, err := realtime.MintBody(nil, time.Minute); err == nil {
		t.Error("nil session accepted")
	}
}
