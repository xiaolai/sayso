// Package openaihttp is the HTTP client every bearer request goes through, with the origin policy, and the failure
// taxonomy shared by HTTP replies, WebSocket closes and realtime error events (spec/http-and-mint.md, spec/session.md).
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
	"strings"
	"syscall"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// Class is a failure class. It decides retry and, in a consumer, the exit code or the message shown. A consumer may
// define further classes of its own; Classify only ever returns these.
type Class string

// Classes (spec/http-and-mint.md, classification).
const (
	ClassAuthExpired    Class = "auth-expired"
	ClassAuthMode       Class = "auth-mode"
	ClassCodexError     Class = "codex-error"
	ClassRouteDenied    Class = "route-denied"
	ClassInvalidRequest Class = "invalid-request"
	ClassRateLimited    Class = "rate-limited"
	ClassServer         Class = "server"
	ClassNetwork        Class = "network"
	ClassProtocol       Class = "protocol"
	ClassCancelled      Class = "cancelled"
	ClassInternal       Class = "internal" // programming errors only
)

// Error is a classified failure. Type, Code, Param and Message come from the provider's error body or event and are
// scrubbed where they are read; Error scrubs its whole text again, so a field set without scrubbing cannot leak a
// secret. No header or bearer is ever stored. Message is for diagnostics, never for display.
type Error struct {
	Class     Class
	Status    int    // HTTP status, 0 if none
	Type      string // provider error type
	Code      string // provider error code; a number is kept as its decimal string
	Param     string // provider error param
	Message   string
	RequestID string // x-request-id
	// Route names the call a route-denied failure is tracked against; the caller chooses the names.
	Route string
	// Fatal is set for a realtime error the session cannot continue after (spec/session.md).
	Fatal bool
	Err   error // underlying cause, if any
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("openai: ")
	b.WriteString(string(e.Class))
	if e.Route != "" {
		fmt.Fprintf(&b, " (%s)", e.Route)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " code=%s", e.Code)
	}
	if e.Param != "" {
		fmt.Fprintf(&b, " param=%s", e.Param)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request id %s)", e.RequestID)
	}
	return secret.Scrub(b.String()) // again, so a field set without scrubbing cannot leak a secret
}

func (e *Error) Unwrap() error { return e.Err }

// Classify returns the class of err. A classified *Error anywhere in the chain wins; then a WebSocket close frame;
// then cancellation; then transport failures. Anything else is internal.
func Classify(err error) Class {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	if c := classifyWebSocket(err); c != "" {
		return c
	}
	if errors.Is(err, context.Canceled) {
		return ClassCancelled
	}
	if isNetwork(err) {
		return ClassNetwork
	}
	return ClassInternal
}

// isNetwork reports DNS, TLS, connection and timeout failures.
func isNetwork(err error) bool {
	var (
		netErr  net.Error
		recErr  tls.RecordHeaderError
		alert   tls.AlertError
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
	)
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &netErr) ||
		errors.As(err, &recErr) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &unknown) || errors.As(err, &host) || errors.As(err, &invalid) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE)
}

// TransportError classifies a broken connection. One the classifier does not recognise is still a broken connection,
// never a programming error.
func TransportError(msg string, err error) error {
	class := Classify(err)
	if class == ClassInternal {
		class = ClassNetwork
	}
	return &Error{Class: class, Message: msg, Err: err}
}

// maxErrorBody bounds how much of an error response is read.
const maxErrorBody = 64 << 10

// providerError reads the provider error shape {"error":{"message","type","code","param"}} with the wire rules
// (package wire): a body that is not strict JSON, or a field of the wrong type, yields no fields at all.
func providerError(body []byte) (message, typ, code, param string) {
	root, err := wire.Parse(body)
	if err != nil {
		return "", "", "", ""
	}
	o, err := wire.AsObject(root)
	if err != nil {
		return "", "", "", ""
	}
	e, ok, err := o.Object("error")
	if err != nil || !ok {
		return "", "", "", ""
	}
	m, _, err1 := e.String("message")
	t, _, err2 := e.String("type")
	p, _, err3 := e.String("param")
	c, err4 := e.Code("code")
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return "", "", "", ""
	}
	return m, t, c, p
}

// CheckResponse returns nil for a 2xx response and a classified *Error otherwise, reading at most 64 KiB of the body
// and closing it in that case. route is kept only on a route-denied error.
func CheckResponse(resp *http.Response, route string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	e := &Error{
		Status:    resp.StatusCode,
		RequestID: secret.Scrub(resp.Header.Get("x-request-id")),
		Route:     route,
	}
	message, typ, code, param := providerError(body)
	e.Message, e.Type, e.Code, e.Param = secret.Scrub(message), secret.Scrub(typ), secret.Scrub(code), secret.Scrub(param)
	e.Class = classifyStatus(resp.StatusCode, e.Code)
	if e.Class != ClassRouteDenied {
		e.Route = ""
	}
	return e
}

// classifyStatus holds the HTTP rows of the taxonomy. The WebSocket rows are in wsclass.go.
func classifyStatus(status int, code string) Class {
	switch {
	case status == http.StatusUnauthorized && code == "missing_scope":
		return ClassRouteDenied
	case status == http.StatusUnauthorized:
		return ClassAuthExpired
	case status == http.StatusForbidden, status == http.StatusNotFound:
		return ClassRouteDenied
	case status == http.StatusTooManyRequests:
		return ClassRateLimited
	case status >= 500:
		return ClassServer
	case status >= 400:
		return ClassInvalidRequest
	default: // 1xx/3xx where a final 2xx was expected
		return ClassProtocol
	}
}
