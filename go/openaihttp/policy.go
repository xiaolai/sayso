package openaihttp

import (
	"fmt"
	"net/http"
)

const (
	apiHost = "api.openai.com"
	// APIBase is the only origin a bearer may be sent to.
	APIBase = "https://" + apiHost
)

// originPolicy refuses, before dialling, any request carrying a bearer whose origin is not exactly
// https://api.openai.com (port 443 or empty, no userinfo, no Host override). Requests without Authorization pass
// unchecked.
type originPolicy struct {
	base        http.RoundTripper
	testOrigins map[string]bool
	realNetwork bool         // base is the production transport, not an injected fake
	guard       func() error // WithRealNetworkGuard; nil allows
}

func (p *originPolicy) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		if err := p.check(req); err != nil {
			if req.Body != nil {
				_ = req.Body.Close() // RoundTripper contract: close the body even on error
			}
			return nil, err
		}
	}
	return p.base.RoundTrip(req)
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

// policyError is a programming error: a bearer request was built for a forbidden origin.
func policyError(why string) error {
	return &Error{Class: ClassInternal, Message: "origin policy: bearer request refused: " + why}
}
