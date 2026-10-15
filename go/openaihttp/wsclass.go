package openaihttp

import (
	"errors"
	"fmt"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/secret"
)

// The WebSocket rows of the taxonomy (spec/session.md) live here, beside the HTTP rows of classifyStatus; Classify
// dispatches to both, so there is one classification.

// ClassifyClose maps a close code received from the peer. 1006 (abnormal closure) never travels on the wire: the
// library reports it for a dropped connection, which is a network failure.
func ClassifyClose(code int) Class {
	switch websocket.StatusCode(code) {
	case websocket.StatusAbnormalClosure:
		return ClassNetwork
	case websocket.StatusInternalError, // 1011, e.g. "keepalive ping timeout"
		websocket.StatusServiceRestart, websocket.StatusTryAgainLater, websocket.StatusBadGateway,
		websocket.StatusGoingAway:
		return ClassServer
	case websocket.StatusPolicyViolation:
		return ClassInvalidRequest
	case websocket.StatusMessageTooBig, websocket.StatusProtocolError, websocket.StatusUnsupportedData,
		websocket.StatusInvalidFramePayloadData, websocket.StatusNormalClosure, websocket.StatusNoStatusRcvd:
		// The peer ended a session that was still expected to run.
		return ClassProtocol
	default:
		return ClassServer
	}
}

// ConnClosed classifies a WebSocket close received while more events were expected. The reason is scrubbed.
func ConnClosed(code int, reason string, cause error) *Error {
	return &Error{
		Class:   ClassifyClose(code),
		Code:    fmt.Sprint(code),
		Message: "realtime connection closed: " + secret.Scrub(reason),
		Err:     cause,
	}
}

// fatalCodes and fatalTypes end a session: it cannot continue and must be replaced.
var (
	fatalCodes = map[string]bool{"session_expired": true, "invalid_api_key": true, "session_not_found": true}
	fatalTypes = map[string]bool{"authentication_error": true, "permission_error": true}
)

// ErrorEvent classifies a realtime `error` event by its error.type and error.code, and marks it fatal when the
// session cannot continue. All strings are scrubbed.
func ErrorEvent(typ, code, param, message string) *Error {
	e := &Error{
		Type:    secret.Scrub(typ),
		Code:    secret.Scrub(code),
		Param:   secret.Scrub(param),
		Message: secret.Scrub(message),
		Fatal:   fatalCodes[code] || fatalTypes[typ],
	}
	switch {
	case code == "rate_limit_exceeded" || code == "insufficient_quota" ||
		typ == "rate_limit_error" || typ == "insufficient_quota":
		e.Class = ClassRateLimited
	case code == "missing_scope":
		e.Class = ClassRouteDenied // the caller sets Route: it knows which session this was
	case typ == "invalid_request_error":
		e.Class = ClassInvalidRequest
	default: // server_error and anything a later API adds
		e.Class = ClassServer
	}
	return e
}

// classifyWebSocket returns the class of a library error carrying a close frame, or "" when err holds none.
func classifyWebSocket(err error) Class {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ClassifyClose(int(ce.Code))
	}
	return ""
}
