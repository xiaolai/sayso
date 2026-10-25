// Package fakeopenai wires the production client to plain-HTTP test servers: a request the client addresses to
// https://<test origin> is sent to the server over plain HTTP, below the origin policy, so the policy still checks
// every bearer request.
package fakeopenai

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/openaihttp"
)

type plainHTTP struct {
	rt   http.RoundTripper
	host string // the test server's; a request to any other host is refused, never sent in the clear
}

func (p plainHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != p.host {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("fakeopenai: %s is not the test server", req.URL.Host)
	}
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	return p.rt.RoundTrip(r)
}

// Client returns the production client wired to the server at rawURL, and the https base URL to address it by.
func Client(t testing.TB, rawURL string) (*http.Client, string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	c, err := openaihttp.NewClient(openaihttp.WithTestOrigin(u.Host), openaihttp.WithBaseTransport(plainHTTP{tr, u.Host}))
	if err != nil {
		t.Fatal(err)
	}
	return c, "https://" + u.Host
}

// WebSocket starts a server that accepts one WebSocket per connection and runs fn on it, then drops it. It records
// each upgrade request's headers in headers, if non-nil. The test's cleanup closes every connection still open, one
// accepted during the cleanup too, since closing the server does not close a hijacked connection.
func WebSocket(t testing.TB, fn func(*websocket.Conn), headers func(http.Header)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	open := map[*websocket.Conn]struct{}{}
	done := false
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headers != nil {
			headers(r.Header.Clone())
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		mu.Lock()
		if done {
			mu.Unlock()
			return
		}
		open[c] = struct{}{}
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(open, c)
			mu.Unlock()
		}()
		fn(c)
	}))
	t.Cleanup(func() {
		mu.Lock()
		done = true
		conns := slices.Collect(maps.Keys(open))
		mu.Unlock()
		for _, c := range conns {
			_ = c.CloseNow()
		}
	})
	t.Cleanup(hs.Close)
	return hs
}
