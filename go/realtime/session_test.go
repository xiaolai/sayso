package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// A 12 MiB message under the read limit is delivered whole; eight concurrent Sends each reach the server as a whole
// message; and a message over the limit then ends the session as protocol.
func TestReadLimitBoundsMessagesAndConcurrentSendsArriveWhole(t *testing.T) {
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

// A refused handshake is classified from its status and request id, never its body (spec/session.md): Swift cannot
// read it, so neither implementation does. A reply that should have upgraded is protocol.
func TestHandshakeRefusalIsClassifiedByStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		class  openaihttp.Class
	}{
		{"401 with missing_scope in the body is auth-expired", http.StatusUnauthorized, openaihttp.ClassAuthExpired},
		{"200 without an upgrade is protocol", http.StatusOK, openaihttp.ClassProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "req_ws")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"missing_scope","type":"invalid_request_error"}}`))
			}))
			t.Cleanup(hs.Close)
			c, base := fakeopenai.Client(t, hs.URL)
			_, err := realtime.Dial(context.Background(), config(c, base))
			wantClass(t, err, tc.class)
			var e *openaihttp.Error
			if errors.As(err, &e); e.Status != tc.status || e.RequestID != "req_ws" || e.Code != "" {
				t.Fatalf("status %d request id %q code %q", e.Status, e.RequestID, e.Code)
			}
		})
	}
}

// A malformed 101 that the server holds open is bounded by the handshake timeout, not by the library's 3 s cleanup.
func TestMalformedUpgradeIsBoundedByTheHandshakeTimeout(t *testing.T) {
	hold := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		<-hold
	}))
	t.Cleanup(func() { close(hold); hs.Close() })
	c, base := fakeopenai.Client(t, hs.URL)
	cfg := config(c, base)
	cfg.HandshakeTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := realtime.Dial(context.Background(), cfg)
	if err == nil {
		t.Fatal("a malformed upgrade opened")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Dial took %v with a 300 ms handshake timeout", d)
	}
}

type badEvent struct{}

func (badEvent) MarshalJSON() ([]byte, error) { return nil, errors.New("ek_canary_from_marshaller") }

// An event that does not encode is internal with a fixed message: the marshaller's text is never returned.
func TestSendEncodeFailureHasAFixedMessage(t *testing.T) {
	s := dialHandler(t, func(c *websocket.Conn) { _, _, _ = c.Read(context.Background()) }, nil)
	err := s.Send(context.Background(), badEvent{})
	wantClass(t, err, openaihttp.ClassInternal)
	if strings.Contains(err.Error(), "canary") {
		t.Fatalf("err = %v", err)
	}
}

// A write stalled on a peer that stopped reading does not hold later sends past their deadline, and Close ends both
// with ErrClosed.
func TestStalledWriteDoesNotHoldLaterSends(t *testing.T) {
	s, first := stalledSession(t, context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := s.Send(ctx, map[string]string{"type": "y"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Send = %v, want its deadline", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("second Send took %v", d)
	}
	_ = s.Close()
	select {
	case err := <-first:
		if !errors.Is(err, realtime.ErrClosed) {
			t.Fatalf("stalled Send after Close = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled Send did not end after Close")
	}
}

// stalledSession opens a session to a peer that never reads and sends large events with ctx until a write stalls:
// the socket buffers are full, and a write has started and not finished for half a second. No event marks a stall,
// so it is the absence of progress, sampled. The stalled Send's error arrives on the channel.
func stalledSession(t *testing.T, ctx context.Context) (*realtime.Session, <-chan error) {
	t.Helper()
	stopped := make(chan struct{})
	hs := fakeopenai.WebSocket(t, func(c *websocket.Conn) { <-stopped }, nil) // never reads
	t.Cleanup(func() { close(stopped) })
	c, base := fakeopenai.Client(t, hs.URL)
	cfg := config(c, base)
	var started, sent atomic.Int64
	cfg.Trace = func(dir string, _ []byte) { // runs holding the write turn, just before the write
		if dir == "client" {
			started.Add(1)
		}
	}
	s, err := realtime.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	big := map[string]string{"type": "x", "pad": strings.Repeat("A", 8<<20)}
	first := make(chan error, 1)
	go func() {
		for {
			if err := s.Send(ctx, big); err != nil {
				first <- err
				return
			}
			sent.Add(1)
		}
	}()
	for quiet, start := 0, time.Now(); quiet < 5; time.Sleep(100 * time.Millisecond) {
		if started.Load() == sent.Load()+1 {
			quiet++
		} else {
			quiet = 0
		}
		if time.Since(start) > 30*time.Second {
			t.Fatal("writes never stalled")
		}
	}
	return s, first
}

// A redirected handshake is refused with its reply kept: protocol, its status and request id, and no Location in
// the error, which may carry a secret; a malformed Location is the same refusal, not a network failure.
func TestHandshakeRedirectIsRefusedWithItsReply(t *testing.T) {
	for _, loc := range []string{"https://elsewhere.test/v1/realtime?k=ek_canary_in_location", "/ek_canary_malformed%zz"} {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", loc)
			w.Header().Set("x-request-id", "req_redirect")
			w.WriteHeader(http.StatusFound)
		}))
		c, base := fakeopenai.Client(t, hs.URL)
		_, err := realtime.Dial(context.Background(), config(c, base))
		hs.Close()
		wantClass(t, err, openaihttp.ClassProtocol)
		var e *openaihttp.Error
		if errors.As(err, &e); e.Status != http.StatusFound || e.RequestID != "req_redirect" {
			t.Fatalf("%s: status %d request id %q", loc, e.Status, e.RequestID)
		}
		for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(text, "canary") {
				t.Fatalf("%s: the error quotes the Location", loc)
			}
		}
	}
}

// A call whose context is already cancelled sends nothing and leaves the session open.
func TestCancelledSendLeavesTheSessionOpen(t *testing.T) {
	got := make(chan string, 4)
	s := dialHandler(t, func(c *websocket.Conn) {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			got <- string(b)
		}
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 { // a select picks at random among ready cases: try it often enough to meet the race
		if err := s.Send(ctx, map[string]string{"type": "cancelled"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Send returned %v", err)
		}
	}
	if err := s.Send(context.Background(), map[string]string{"type": "after"}); err != nil {
		t.Fatalf("Send after a cancelled one: %v", err)
	}
	select {
	case m := <-got:
		if !strings.Contains(m, "after") {
			t.Fatalf("the peer received %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer received nothing")
	}
}

// An injected transport may refuse with no body at all; that is a classified refusal, not a panic.
func TestRefusalWithoutABody(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Request-Id": {"req_nobody"}}, Request: req}, nil
	})
	c, err := openaihttp.NewClient(openaihttp.WithBaseTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	_, err = realtime.Dial(context.Background(), config(c, openaihttp.APIBase))
	wantClass(t, err, openaihttp.ClassRouteDenied)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Every trailing slash of the base is removed, as in Swift.
func TestWebSocketURLTrimsEverySlash(t *testing.T) {
	if got := realtime.WebSocketURL("https://api.openai.com//", "m"); got != "https://api.openai.com/v1/realtime?model=m" {
		t.Fatalf("got %q", got)
	}
}

// Invalid UTF-8 is refused before anything is sent, and the session stays open.
func TestSendRefusesInvalidUTF8(t *testing.T) {
	got := make(chan string, 1)
	s := dialHandler(t, func(c *websocket.Conn) {
		_, b, _ := c.Read(context.Background())
		got <- string(b)
	}, nil)
	for _, bad := range []string{"{\"a\":\"\xff\"}", "{\"a\":\"\xed\xa0\x80\"}", "{\"a\":\"\xc0\xaf\"}"} {
		err := s.Send(context.Background(), json.RawMessage(bad))
		wantClass(t, err, openaihttp.ClassInternal)
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
	if err := s.Send(context.Background(), map[string]string{"type": "input_audio_buffer.clear", "é": "ok"}); err != nil {
		t.Fatal(err)
	}
	if b := <-got; b != `{"type":"input_audio_buffer.clear","é":"ok"}` {
		t.Fatalf("the peer read %q", b)
	}
}

type countingSource struct{ calls atomic.Int32 }

func (c *countingSource) Secret(context.Context, any) (secret.ClientSecret, error) {
	c.calls.Add(1)
	return secret.NewClientSecret("ek_counted", time.Unix(1800000000, 0)), nil
}

// Open checks the configuration and the destination before it asks the source for a secret.
func TestOpenChecksBeforeTheSecret(t *testing.T) {
	c, base := fakeopenai.Client(t, "http://127.0.0.1:1")
	src := &countingSource{}
	for name, edit := range map[string]func(*realtime.DialConfig){
		"no read limit":    func(cfg *realtime.DialConfig) { cfg.ReadLimit = 0 },
		"no timeout":       func(cfg *realtime.DialConfig) { cfg.HandshakeTimeout = 0 },
		"unguarded client": func(cfg *realtime.DialConfig) { cfg.Client = http.DefaultClient },
		"forbidden origin": func(cfg *realtime.DialConfig) { cfg.URL = realtime.WebSocketURL("https://example.invalid", "m") },
		"plain scheme":     func(cfg *realtime.DialConfig) { cfg.URL = "ws://api.openai.com/v1/realtime?model=m" },
	} {
		cfg := config(c, base)
		edit(&cfg)
		_, err := realtime.Open(context.Background(), src, map[string]string{"type": "realtime"}, cfg)
		var e *openaihttp.Error
		if !errors.As(err, &e) || e.Class != openaihttp.ClassInternal {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("the source was asked %d times", n)
	}
}
