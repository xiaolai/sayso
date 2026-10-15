package openaihttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const proxyChildEnv = "SAYSO_OPENAI_PROXY_CHILD"

// connectProxy is a minimal in-process HTTPS (CONNECT) proxy. It tunnels every
// target to 127.0.0.1 on the requested port and records the targets.
type connectProxy struct {
	mu      sync.Mutex
	targets []string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, r.Host)
	p.mu.Unlock()
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstream, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, buf); _ = upstream.(*net.TCPConn).CloseWrite() }()
	go func() { defer wg.Done(); _, _ = io.Copy(conn, upstream); _ = conn.Close() }()
	wg.Wait()
	_ = upstream.Close()
}

func (p *connectProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// http.ProxyFromEnvironment reads the environment once per process, so the
// proxy run happens in a child process whose environment is set before any
// HTTP use.
// accept: NET-01#2
func TestMintAndWebSocketThroughConnectProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) == "1" {
		proxyChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMintAndWebSocketThroughConnectProxy$", "-test.v", "-test.count=1")
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "REQUEST_METHOD", proxyChildEnv:
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, proxyChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestMintAndWebSocketThroughConnectProxy") {
		t.Fatalf("proxy child did not run the test:\n%s", out)
	}
}

func proxyChild(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != canaryBearer {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/realtime/client_secrets":
			_, _ = io.WriteString(w, `{"value":"ek_canary"}`)
		case "/v1/realtime":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created"}`))
			_, _, _ = c.Read(r.Context()) // wait for the client's close
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	proxy := &connectProxy{}
	ps := httptest.NewServer(proxy)
	defer ps.Close()

	// example.com is in the test certificate and, unlike loopback, is proxied.
	_, port, _ := net.SplitHostPort(hostport(t, origin.URL))
	host := net.JoinHostPort("example.com", port)
	t.Setenv("HTTPS_PROXY", ps.URL)
	t.Setenv("NO_PROXY", "")

	c, err := NewClient(WithRootCAs(certPool(origin)), WithTestOrigin(host))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Mint.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/v1/realtime/client_secrets", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", canaryBearer)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("mint through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := CheckResponse(resp, "speech-mint"); err != nil || !strings.Contains(string(body), "ek_canary") {
		t.Fatalf("mint: %v %q", err, body)
	}

	// WebSocket session over the same client.
	ws, _, err := websocket.Dial(ctx, "wss://"+host+"/v1/realtime?model=gpt-realtime-2.1", &websocket.DialOptions{
		HTTPClient: c,
		HTTPHeader: http.Header{"Authorization": {canaryBearer}},
	})
	if err != nil {
		t.Fatalf("websocket through proxy: %v", err)
	}
	_, msg, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(msg), "session.created") {
		t.Fatalf("first event %q, %v", msg, err)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")

	// The policy still applies to proxied requests.
	bad, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://other.test:"+port+"/v1/realtime/client_secrets", nil)
	if err != nil {
		t.Fatal(err)
	}
	bad.Header.Set("Authorization", canaryBearer)
	before := len(proxy.seen())
	if resp, err := c.Do(bad); err == nil {
		_ = resp.Body.Close()
		t.Fatal("proxied request to another host allowed")
	} else if Classify(err) != ClassInternal {
		t.Fatalf("class %q, want internal", Classify(err))
	}
	if got := len(proxy.seen()); got != before {
		t.Fatal("refused request reached the proxy")
	}

	seen := proxy.seen()
	if len(seen) == 0 {
		t.Fatal("proxy saw no CONNECT: requests bypassed it")
	}
	for _, s := range seen {
		if s != host {
			t.Errorf("CONNECT target %q, want %q", s, host)
		}
	}
}
