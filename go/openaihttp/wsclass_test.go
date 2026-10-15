package openaihttp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// One row per WebSocket signal of the taxonomy (spec/session.md): close codes and `error` events.
// accept: NET-05#1
func TestClassifyWebSocketRows(t *testing.T) {
	closeErr := func(code websocket.StatusCode) error {
		return fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: code, Reason: "keepalive ping timeout"})
	}
	rows := []struct {
		name  string
		err   error
		class Class
	}{
		{"close 1011", closeErr(websocket.StatusInternalError), ClassServer},
		{"close 1012", closeErr(websocket.StatusServiceRestart), ClassServer},
		{"close 1013", closeErr(websocket.StatusTryAgainLater), ClassServer},
		{"close 1014", closeErr(websocket.StatusBadGateway), ClassServer},
		{"close 1001", closeErr(websocket.StatusGoingAway), ClassServer},
		{"close 1006 abnormal", closeErr(websocket.StatusAbnormalClosure), ClassNetwork},
		{"close 1008 policy", closeErr(websocket.StatusPolicyViolation), ClassInvalidRequest},
		{"close 1009 too big", closeErr(websocket.StatusMessageTooBig), ClassProtocol},
		{"close 1002", closeErr(websocket.StatusProtocolError), ClassProtocol},
		{"close 1000 mid-response", closeErr(websocket.StatusNormalClosure), ClassProtocol},
		{"close 4000 unknown", closeErr(4000), ClassServer},
		{"ConnClosed 1011", ConnClosed(1011, "keepalive ping timeout", nil), ClassServer},
		{"ConnClosed 1006", ConnClosed(1006, "", nil), ClassNetwork},
		{"event server_error", ErrorEvent("server_error", "server_error", "", "boom"), ClassServer},
		{"event invalid_value", ErrorEvent("invalid_request_error", "invalid_value", "response.audio.output.voice", "bad voice"), ClassInvalidRequest},
		{"event rate_limit_exceeded", ErrorEvent("invalid_request_error", "rate_limit_exceeded", "", "slow"), ClassRateLimited},
		{"event insufficient_quota", ErrorEvent("insufficient_quota", "", "", "quota"), ClassRateLimited},
		{"event missing_scope", ErrorEvent("invalid_request_error", "missing_scope", "", "scopes"), ClassRouteDenied},
		{"event unknown type", ErrorEvent("brand_new_error", "", "", "?"), ClassServer},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := Classify(r.err); got != r.class {
				t.Errorf("Classify = %q, want %q", got, r.class)
			}
		})
	}
}

func TestErrorEventScrubsAndNamesParam(t *testing.T) {
	e := ErrorEvent("invalid_request_error", "invalid_value", "response.audio.output.voice", "bad ek_canary value")
	if strings.Contains(e.Error(), "ek_canary") || !strings.Contains(e.Error(), "param=response.audio.output.voice") {
		t.Fatalf("error = %q", e.Error())
	}
	c := ConnClosed(1011, "reason eyJcanary.canary.canary", nil)
	if strings.Contains(c.Error(), "canary") || c.Code != "1011" {
		t.Fatalf("close error = %q", c.Error())
	}
}

func TestFatalErrors(t *testing.T) {
	for _, e := range []*Error{
		ErrorEvent("invalid_request_error", "session_expired", "", ""),
		ErrorEvent("invalid_request_error", "invalid_api_key", "", ""),
		ErrorEvent("invalid_request_error", "session_not_found", "", ""),
		ErrorEvent("authentication_error", "", "", ""),
		ErrorEvent("permission_error", "", "", ""),
	} {
		if !e.Fatal {
			t.Errorf("%v: not fatal", e)
		}
	}
	if ErrorEvent("server_error", "", "", "").Fatal || ErrorEvent("invalid_request_error", "invalid_value", "", "").Fatal {
		t.Error("an ordinary error is fatal")
	}
}
