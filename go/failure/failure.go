// Package failure is the failure taxonomy every sayso package reports in (spec/http-and-mint.md, classification):
// the classes and the classified error. It is a leaf, so the app-server client and the credentials classify their
// failures without the HTTP stack; openaihttp names the same types and classifies transport failures too.
package failure

import (
	"fmt"
	"strings"

	"github.com/xiaolai/sayso/go/secret"
)

// Class is a failure class. It decides retry and, in a consumer, the exit code or the message shown. A consumer may
// define further classes of its own; this module only ever returns these.
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
	// EventType is, for a realtime event that could not be decoded, its type when that could be read, scrubbed; ""
	// for any other failure (spec/session.md).
	EventType string
	Err       error // underlying cause, if any
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
