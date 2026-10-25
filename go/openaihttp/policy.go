package openaihttp

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	apiHost = "api.openai.com"
	// APIBase is the only origin a bearer may be sent to.
	APIBase = "https://" + apiHost
)

// originPolicy refuses, before dialling, any request carrying a bearer whose origin is not exactly
// https://api.openai.com (port 443 or empty, no userinfo, no Host override). Requests without an Authorization value
// pass unchecked.
type originPolicy struct {
	base        http.RoundTripper
	testOrigins map[string]bool
	realNetwork bool         // base is the production transport, not an injected fake
	guard       func() error // WithRealNetworkGuard; nil allows
}

func (p *originPolicy) RoundTrip(req *http.Request) (*http.Response, error) {
	if hasAuthorization(req.Header) {
		if err := p.check(req); err != nil {
			if req.Body != nil {
				_ = req.Body.Close() // RoundTripper contract: close the body even on error
			}
			return nil, err
		}
	}
	resp, err := p.base.RoundTrip(req)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// A redirect is refused, and its reply delivered as it came, as in Swift: http.Client returns a 3xx without a
		// Location as the response, so CheckResponse classifies it with its status, request id and error body. With
		// the Location, http.Client would parse it before CheckRedirect runs: a malformed one failed as network, and
		// the refusal's *url.Error quoted the Location, which may carry a secret.
		resp.Header.Del("Location")
	}
	return resp, err
}

// hasAuthorization reports whether h holds any Authorization value, under any spelling of the key: the transport
// sends every value of every key as written, not only Header.Get's first value under the canonical one.
func hasAuthorization(h http.Header) bool {
	for k, v := range h {
		if len(v) > 0 && strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// CloseIdleConnections closes the base transport's idle connections, when it keeps any.
func (p *originPolicy) CloseIdleConnections() {
	if c, ok := p.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (p *originPolicy) check(req *http.Request) error {
	u := req.URL
	switch {
	case u == nil:
		return policyError("request without URL")
	case u.User != nil:
		return policyError("userinfo in URL")
	case req.Host != "" && req.Host != u.Host:
		return policyError("Host header override")
	case u.Scheme != "https":
		return policyError(fmt.Sprintf("scheme %q", u.Scheme))
	case p.testOrigins[u.Host]:
		return nil
	case u.Hostname() != apiHost:
		return policyError(fmt.Sprintf("host %q", u.Hostname()))
	case u.Port() != "" && u.Port() != "443":
		return policyError(fmt.Sprintf("port %q", u.Port()))
	case p.realNetwork && p.guard != nil:
		if err := p.guard(); err != nil {
			return policyError("real OpenAI origin over the real network refused: " + err.Error())
		}
	}
	return nil
}

// CheckOrigin reports whether c's origin policy lets a bearer request to rawURL through, checked as a request
// through c would be; a ws or wss URL is checked as the http or https request that opens it. It is for a caller that
// must refuse a forbidden destination before obtaining the bearer. A client not from NewClient is refused.
func CheckOrigin(c *http.Client, rawURL string) error {
	if c == nil {
		return policyError("client without the origin policy")
	}
	p, ok := c.Transport.(*originPolicy)
	if !ok {
		return policyError("client without the origin policy")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return policyError("unparsable URL")
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	return p.check(&http.Request{URL: u, Header: http.Header{}})
}

// policyError is a programming error: a bearer request was built for a forbidden origin.
func policyError(why string) error {
	return &Error{Class: ClassInternal, Message: "origin policy: bearer request refused: " + why}
}
