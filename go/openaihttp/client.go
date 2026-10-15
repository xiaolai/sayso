package openaihttp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Transport-level limits. They bound a stuck connection; the caller's per-call deadlines are the real limits.
const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 5 * time.Minute // long uploads answer only after processing
	idleConnTimeout       = 90 * time.Second
)

// Option configures NewClient.
type Option func(*config) error

type config struct {
	base        http.RoundTripper
	rootCAs     *x509.CertPool
	testOrigins map[string]bool
	guard       func() error
}

// WithBaseTransport replaces the transport below the origin policy. For tests: fakes go below the policy, so the
// policy code path is always exercised.
func WithBaseTransport(rt http.RoundTripper) Option {
	return func(c *config) error {
		if rt == nil {
			return fmt.Errorf("openaihttp: nil base transport")
		}
		c.base = rt
		return nil
	}
}

// WithRootCAs makes the production transport trust pool instead of the system roots. For tests against an httptest
// TLS server.
func WithRootCAs(pool *x509.CertPool) Option {
	return func(c *config) error {
		c.rootCAs = pool
		return nil
	}
}

// WithTestOrigin additionally allows https://hostport for bearer requests. For tests only; hostport must name a
// reserved host (loopback, localhost, example.com, or a .test name), so a misuse can never send a bearer to a real
// third party.
func WithTestOrigin(hostport string) Option {
	return func(c *config) error {
		host, port, err := net.SplitHostPort(hostport)
		if err != nil || port == "" {
			return fmt.Errorf("openaihttp: test origin %q: want host:port", hostport)
		}
		if !reservedHost(host) {
			return fmt.Errorf("openaihttp: test origin %q is not a reserved test host", hostport)
		}
		if c.testOrigins == nil {
			c.testOrigins = map[string]bool{}
		}
		c.testOrigins[hostport] = true
		return nil
	}
}

// WithRealNetworkGuard installs a check run before every bearer request to the real origin over the production
// transport; a non-nil error refuses the request as internal. A consumer uses it to stop unit tests from spending
// real usage.
func WithRealNetworkGuard(guard func() error) Option {
	return func(c *config) error {
		c.guard = guard
		return nil
	}
}

func reservedHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	h := strings.ToLower(host)
	return h == "localhost" || h == "example.com" || strings.HasSuffix(h, ".test")
}

// NewClient returns the http.Client for REST calls and WebSocket dials. Its transport is wrapped by the origin
// policy, the proxy comes from the environment, and it follows no redirect. It has no overall timeout: callers set
// per-call deadlines, and a WebSocket dial rejects a client with Timeout set.
func NewClient(opts ...Option) (*http.Client, error) {
	var c config
	for _, o := range opts {
		if err := o(&c); err != nil {
			return nil, err
		}
	}
	// A test origin may name a real host (example.com). Only allow it when the system trust store is replaced, so a
	// bearer can never reach the real host: its certificate would not verify against a test-only pool.
	if len(c.testOrigins) > 0 && c.base == nil && c.rootCAs == nil {
		return nil, fmt.Errorf("openaihttp: a test origin requires WithRootCAs or WithBaseTransport")
	}
	base := c.base
	if base == nil {
		base = newTransport(c.rootCAs)
	}
	return &http.Client{
		Transport:     &originPolicy{base: base, testOrigins: c.testOrigins, realNetwork: c.base == nil, guard: c.guard},
		CheckRedirect: refuseRedirect,
	}, nil
}

// newTransport is the production transport. HTTP/2 stays off: WebSocket upgrades need HTTP/1.1, and one client
// serves both.
func newTransport(roots *x509.CertPool) *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConnsPerHost:   4,
	}
}

// refuseRedirect stops every redirect, same host or not, so a bearer never follows a 3xx.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	status := 0
	if req.Response != nil {
		status = req.Response.StatusCode
	}
	return &Error{Class: ClassProtocol, Status: status, Message: "redirect refused"}
}

// Guarded reports whether c sends through the origin policy, that is, whether it came from NewClient. Packages that
// attach a bearer refuse any other client.
func Guarded(c *http.Client) bool {
	if c == nil {
		return false
	}
	_, ok := c.Transport.(*originPolicy)
	return ok
}
