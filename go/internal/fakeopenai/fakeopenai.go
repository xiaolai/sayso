// Package fakeopenai wires the production client to plain-HTTP test servers: a request the client addresses to
// https://<test origin> is sent to the server over plain HTTP, below the origin policy, so the policy still checks
// every bearer request.
package fakeopenai

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/openaihttp"
)

type plainHTTP struct{ rt http.RoundTripper }

func (p plainHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
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
	c, err := openaihttp.NewClient(openaihttp.WithTestOrigin(u.Host), openaihttp.WithBaseTransport(plainHTTP{tr}))
	if err != nil {
		t.Fatal(err)
	}
	return c, "https://" + u.Host
}

// WebSocket starts a server that accepts one WebSocket per connection and runs fn on it, then drops it. It records
// each upgrade request's headers in headers, if non-nil.
func WebSocket(t testing.TB, fn func(*websocket.Conn), headers func(http.Header)) *httptest.Server {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headers != nil {
			headers(r.Header.Clone())
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		fn(c)
	}))
	t.Cleanup(hs.Close)
	return hs
}
