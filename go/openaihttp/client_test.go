package openaihttp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const canaryBearer = "Bearer eyJcanary.canary.canary"

// recorder is a fake transport below the policy. It records every request it
// receives and answers with respond.
type recorder struct {
	mu      sync.Mutex
	got     []*http.Request
	respond func(*http.Request) *http.Response
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.got = append(r.got, req)
	r.mu.Unlock()
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if r.respond != nil {
		return r.respond(req), nil
	}
	return reply(req, http.StatusOK, nil, "{}"), nil
}

func (r *recorder) requests() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*http.Request(nil), r.got...)
}

func reply(req *http.Request, status int, h http.Header, body string) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

// call shapes a consumer makes with a bearer: mint, HTTP STT and a WebSocket upgrade.
type shape struct {
	name, method, path string
	header             http.Header
}

var shapes = []shape{
	{"mint", http.MethodPost, "/v1/realtime/client_secrets", http.Header{"Content-Type": {"application/json"}}},
	{"stt", http.MethodPost, "/v1/audio/transcriptions", http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}},
	{"websocket", http.MethodGet, "/v1/realtime?model=gpt-realtime-2.1", http.Header{
		"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"},
		"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
	}},
}

func newReq(t *testing.T, s shape, rawURL string) *http.Request {
	t.Helper()
	var body io.Reader
	if s.method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequest(s.method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range s.header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", canaryBearer)
	return req
}

// accept: NET-01#1
func TestOriginPolicyRefusesBeforeDialling(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string // req.Host override
	}{
		{"other host", "https://api.example.org", ""},
		{"lookalike host", "https://api.openai.com.evil.test", ""},
		{"http scheme", "http://api.openai.com", ""},
		{"port 8443", "https://api.openai.com:8443", ""},
		{"userinfo", "https://user:pw@api.openai.com", ""},
		{"Host override", "https://api.openai.com", "evil.test"},
	}
	for _, s := range shapes {
		for _, tc := range cases {
			t.Run(s.name+"/"+tc.name, func(t *testing.T) {
				base := &recorder{}
				c, err := NewClient(WithBaseTransport(base))
				if err != nil {
					t.Fatal(err)
				}
				req := newReq(t, s, tc.origin+s.path)
				req.Host = tc.host
				resp, err := c.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("request allowed, want refusal")
				}
				if n := len(base.requests()); n != 0 {
					t.Fatalf("base transport saw %d requests, want 0", n)
				}
				if got := Classify(err); got != ClassInternal {
					t.Errorf("class %q, want %q", got, ClassInternal)
				}
				if strings.Contains(err.Error(), "eyJcanary") {
					t.Errorf("error leaks bearer: %v", err)
				}
			})
		}
	}
}

// accept: NET-01#1
func TestOriginPolicyAllowsExactOrigin(t *testing.T) {
	for _, s := range shapes {
		for _, origin := range []string{"https://api.openai.com", "https://api.openai.com:443"} {
			base := &recorder{}
			c, err := NewClient(WithBaseTransport(base))
			if err != nil {
				t.Fatal(err)
			}
			req := newReq(t, s, origin+s.path)
			req.Host = req.URL.Host // explicit, equal to the URL host
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", s.name, origin, err)
			}
			_ = resp.Body.Close()
			if n := len(base.requests()); n != 1 {
				t.Fatalf("%s %s: base saw %d requests, want 1", s.name, origin, n)
			}
		}
	}
}

// Requests without a bearer are not the policy's concern.
func TestOriginPolicyIgnoresUnauthenticated(t *testing.T) {
	base := &recorder{}
	c, err := NewClient(WithBaseTransport(base))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://elsewhere.test/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if n := len(base.requests()); n != 1 {
		t.Fatalf("base saw %d requests, want 1", n)
	}
}

// accept: NET-01#1
func TestRedirectsAreNeverFollowed(t *testing.T) {
	targets := map[string]string{
		"same host":  "https://api.openai.com/v1/elsewhere",
		"other host": "https://second.example.org/v1/realtime/client_secrets",
	}
	for _, s := range shapes {
		for name, loc := range targets {
			for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				base := &recorder{respond: func(req *http.Request) *http.Response {
					return reply(req, status, http.Header{"Location": {loc}}, "")
				}}
				c, err := NewClient(WithBaseTransport(base))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := c.Do(newReq(t, s, APIBase+s.path))
				err = redirectRefusal(resp, err)
				got := base.requests()
				if len(got) != 1 {
					t.Fatalf("%s %s %d: base saw %d requests, want 1", s.name, name, status, len(got))
				}
				if got[0].URL.Hostname() != apiHost {
					t.Fatalf("first request went to %q", got[0].URL.Hostname())
				}
				var e *Error
				if !errors.As(err, &e) || e.Class != ClassProtocol || e.Status != status {
					t.Errorf("%s %s %d: error %v, want protocol with status", s.name, name, status, err)
				}
			}
		}
	}
}

// The second host never receives a request on a cross-host redirect, checked
// against a real listener.
// accept: NET-01#1
func TestCrossHostRedirectNeverReachesSecondHost(t *testing.T) {
	var hits int
	var mu sync.Mutex
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	c, err := NewClient(WithRootCAs(certPool(first, second)), WithTestOrigin(hostport(t, first.URL)), WithTestOrigin(hostport(t, second.URL)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(newReq(t, shapes[0], first.URL+shapes[0].path))
	var e *Error
	if err = redirectRefusal(resp, err); !errors.As(err, &e) || e.Class != ClassProtocol || e.Status != http.StatusTemporaryRedirect {
		t.Fatalf("error %v, want protocol with status 307", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("second host received %d requests", hits)
	}
}

func TestProductionTransportSettings(t *testing.T) {
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 0 {
		t.Errorf("client timeout %v; websocket.Dial needs 0", c.Timeout)
	}
	p, ok := c.Transport.(*originPolicy)
	if !ok {
		t.Fatalf("transport %T, want origin policy on top", c.Transport)
	}
	tr, ok := p.base.(*http.Transport)
	if !ok {
		t.Fatalf("base %T, want *http.Transport", p.base)
	}
	if tr.Proxy == nil {
		t.Error("no proxy function")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion %x, want TLS 1.2", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSHandshakeTimeout == 0 || tr.ResponseHeaderTimeout == 0 || tr.DialContext == nil {
		t.Error("missing transport timeouts")
	}
}

// A server that offers only TLS 1.1 is refused, and the failure is a network error.
// accept: NET-01#1
func TestTLSMinimumIsEnforced(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS11}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected
	srv.StartTLS()
	defer srv.Close()

	c, err := NewClient(WithRootCAs(certPool(srv)), WithTestOrigin(hostport(t, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(newReq(t, shapes[0], srv.URL+shapes[0].path))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("TLS 1.1 handshake succeeded")
	}
	if got := Classify(err); got != ClassNetwork {
		t.Errorf("class %q, want network (%v)", got, err)
	}
}

func TestWithTestOriginRejectsRealHosts(t *testing.T) {
	for _, hp := range []string{"api.example.org:443", "evil.com:443", "127.0.0.1", "10.0.0.1:443"} {
		if _, err := NewClient(WithTestOrigin(hp)); err == nil {
			t.Errorf("WithTestOrigin(%q) accepted", hp)
		}
	}
	for _, hp := range []string{"127.0.0.1:1", "[::1]:1", "localhost:1", "example.com:1", "origin.test:1"} {
		if _, err := NewClient(WithTestOrigin(hp), WithRootCAs(x509.NewCertPool())); err != nil {
			t.Errorf("WithTestOrigin(%q): %v", hp, err)
		}
	}
	if _, err := NewClient(WithBaseTransport(nil)); err == nil {
		t.Error("nil base transport accepted")
	}
}

// accept: NET-01#1
func TestTestOriginRequiresReplacedTrust(t *testing.T) {
	// With the system trust store, a test origin such as example.com could be the real host.
	if _, err := NewClient(WithTestOrigin("example.com:443")); err == nil {
		t.Fatal("NewClient accepted a test origin with the system trust store")
	}
	if _, err := NewClient(WithTestOrigin("example.com:443"), WithRootCAs(x509.NewCertPool())); err != nil {
		t.Fatalf("with a test-only root pool: %v", err)
	}
}

// The real-network guard runs only for the real origin over the production transport, and refuses as internal.
func TestRealNetworkGuard(t *testing.T) {
	refuse := func() error { return errors.New("unit test") }
	c, err := NewClient(WithRealNetworkGuard(refuse))
	if err != nil {
		t.Fatal(err)
	}
	req := newReq(t, shapes[0], APIBase+shapes[0].path)
	req.Header.Set("Authorization", canaryBearer)
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("guarded request was sent")
	}
	if got := Classify(err); got != ClassInternal || !strings.Contains(err.Error(), "unit test") {
		t.Fatalf("got %q: %v", got, err)
	}
	// Below an injected fake transport, nothing reaches the network, so the guard does not apply.
	rec := &recorder{}
	c, err = NewClient(WithRealNetworkGuard(refuse), WithBaseTransport(rec))
	if err != nil {
		t.Fatal(err)
	}
	req = newReq(t, shapes[0], APIBase+shapes[0].path)
	req.Header.Set("Authorization", canaryBearer)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatalf("fake transport refused: %v", err)
	}
	_ = resp.Body.Close()
	if len(rec.requests()) != 1 {
		t.Fatal("request did not reach the fake")
	}
}

// redirectRefusal is the error a caller sees for a redirect: the client returns the 3xx reply unfollowed, and
// CheckResponse classifies it. A followed redirect would be a 2xx here, which yields nil.
func redirectRefusal(resp *http.Response, err error) error {
	if err != nil {
		return err
	}
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil
	}
	return CheckResponse(resp, "")
}
