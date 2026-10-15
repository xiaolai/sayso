package openaihttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

func errBody(code, param, msg string) string {
	p := "null"
	if param != "" {
		p = fmt.Sprintf("%q", param)
	}
	return fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error","code":%q,"param":%s}}`, msg, code, p)
}

// One row per HTTP signal of the taxonomy (spec/http-and-mint.md).
// accept: NET-05#1
func TestCheckResponseHTTPRows(t *testing.T) {
	rows := []struct {
		name   string
		status int
		body   string
		class  Class
		route  bool // route kept on the error
	}{
		{"400 bad voice", 400, errBody("invalid_value", "session.audio.output.voice", "Invalid voice"), ClassInvalidRequest, false},
		{"401 invalid_token", 401, errBody("invalid_token", "", "token expired"), ClassAuthExpired, false},
		{"401 no body", 401, "", ClassAuthExpired, false},
		{"401 missing_scope", 401, errBody("missing_scope", "", "Missing scopes: api.model.audio.request"), ClassRouteDenied, true},
		{"403", 403, errBody("missing_scope", "", "forbidden"), ClassRouteDenied, true},
		{"403 no code", 403, "", ClassRouteDenied, true},
		{"404 endpoint", 404, errBody("", "", "Not found"), ClassRouteDenied, true},
		{"413", 413, "", ClassInvalidRequest, false},
		{"429", 429, errBody("rate_limit_exceeded", "", "slow down"), ClassRateLimited, false},
		{"500", 500, errBody("server_error", "", "oops"), ClassServer, false},
		{"502 html", 502, "<html>bad gateway</html>", ClassServer, false},
		{"503", 503, "", ClassServer, false},
		{"304", 304, "", ClassProtocol, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			resp := reply(nil, r.status, http.Header{"X-Request-Id": {"req_123"}}, r.body)
			err := CheckResponse(resp, "speech-mint")
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("got %v, want *Error", err)
			}
			if e.Class != r.class || Classify(err) != r.class {
				t.Errorf("class %q, want %q", e.Class, r.class)
			}
			if got := Classify(fmt.Errorf("wrapped: %w", err)); got != r.class {
				t.Errorf("wrapped class %q, want %q", got, r.class)
			}
			if e.Status != r.status || e.RequestID != "req_123" {
				t.Errorf("status %d id %q", e.Status, e.RequestID)
			}
			if (e.Route == "speech-mint") != r.route {
				t.Errorf("route %q, want kept=%v", e.Route, r.route)
			}
		})
	}
}

// accept: NET-05#1
func TestInvalidRequestNamesParam(t *testing.T) {
	resp := reply(nil, 400, nil, errBody("invalid_value", "session.audio.output.speed", "speed out of range"))
	err := CheckResponse(resp, "speech-mint")
	var e *Error
	if !errors.As(err, &e) || e.Param != "session.audio.output.speed" || e.Code != "invalid_value" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), "param=session.audio.output.speed") {
		t.Errorf("message does not name the param: %v", err)
	}
}

func TestCheckResponseScrubsAndAcceptsSuccess(t *testing.T) {
	if err := CheckResponse(reply(nil, 200, nil, "{}"), "http-stt"); err != nil {
		t.Fatalf("2xx: %v", err)
	}
	body := `{"error":{"message":"bad token eyJcanary.canary.canary and ek_canary","type":"x","code":401,"param":null}}`
	err := CheckResponse(reply(nil, 401, nil, body), "http-stt")
	if s := err.Error(); strings.Contains(s, "canary") {
		t.Errorf("error leaks secret: %s", s)
	}
	var e *Error
	if errors.As(err, &e) && e.Code != "401" {
		t.Errorf("numeric code %q, want \"401\"", e.Code)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// accept: NET-05#1
func TestClassifyTransportAndOtherRows(t *testing.T) {
	urlErr := func(err error) error { return &url.Error{Op: "Post", URL: APIBase, Err: err} }
	rows := []struct {
		name  string
		err   error
		class Class
	}{
		{"DNS", urlErr(&net.DNSError{Err: "no such host", Name: "api.openai.com", IsNotFound: true}), ClassNetwork},
		{"connection reset", urlErr(&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), ClassNetwork},
		{"bare reset", syscall.ECONNRESET, ClassNetwork},
		{"refused", syscall.ECONNREFUSED, ClassNetwork},
		{"TLS unknown authority", urlErr(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), ClassNetwork},
		{"TLS hostname", urlErr(x509.HostnameError{Host: "x"}), ClassNetwork},
		{"TLS record header", urlErr(tls.RecordHeaderError{Msg: "bad"}), ClassNetwork},
		{"TLS alert", urlErr(tls.AlertError(40)), ClassNetwork},
		{"timeout", urlErr(timeoutErr{}), ClassNetwork},
		{"deadline", urlErr(context.DeadlineExceeded), ClassNetwork},
		{"unexpected EOF", urlErr(io.ErrUnexpectedEOF), ClassNetwork},
		{"cancelled", urlErr(context.Canceled), ClassCancelled},
		{"auth-mode", &Error{Class: ClassAuthMode}, ClassAuthMode},
		{"codex-error", fmt.Errorf("codex: %w", &Error{Class: ClassCodexError}), ClassCodexError},
		{"protocol", &Error{Class: ClassProtocol}, ClassProtocol},
		{"a consumer class", &Error{Class: "verify-failed"}, "verify-failed"},
		{"unknown is internal", errors.New("nil map"), ClassInternal},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := Classify(r.err); got != r.class {
				t.Errorf("Classify = %q, want %q", got, r.class)
			}
		})
	}
	if Classify(nil) != "" {
		t.Error("nil error must be unclassified")
	}
}

// A real dial failure through the client is network-class.
func TestClassifyRealDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c, err := NewClient(WithTestOrigin(addr), WithRootCAs(x509.NewCertPool()))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(newReq(t, shapes[0], "https://"+addr+shapes[0].path))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("dial to closed port succeeded")
	}
	if got := Classify(err); got != ClassNetwork {
		t.Errorf("class %q, want network (%v)", got, err)
	}
}

// Error scrubs its whole text, so a field set without scrubbing cannot leak a secret.
func TestErrorStringScrubsEveryField(t *testing.T) {
	e := &Error{Class: ClassServer, Message: "ek_canary_message", Code: "ek_canary_code", Param: "ek_canary_param",
		Route: "ek_canary_route", RequestID: "ek_canary_request"}
	if got := e.Error(); strings.Contains(got, "canary") {
		t.Fatalf("Error() = %q", got)
	}
}
