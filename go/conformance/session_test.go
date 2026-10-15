package conformance

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/xiaolai/sayso/go/internal/fakeopenai"
	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
	"github.com/xiaolai/sayso/go/secret"
)

type sessionCase struct {
	Kind         string          `json:"kind"`
	Code         int             `json:"code"`
	Reason       string          `json:"reason"`
	Event        json.RawMessage `json:"event"`
	ReadLimit    int64           `json:"readLimit"`
	MessageBytes int             `json:"messageBytes"`
	Raw          *string         `json:"raw"`
	Expect       map[string]any  `json:"expect"`
}

func TestSession(t *testing.T) {
	run(t, "session", func(t *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
		var sc sessionCase
		if err := decode(c, &sc); err != nil {
			return fixtures.Fail, err.Error()
		}
		switch sc.Kind {
		case "close":
			// The session classifies a received close frame through ReadError, as here.
			err := realtime.ReadError(fmt.Errorf("read: %w", websocket.CloseError{Code: websocket.StatusCode(sc.Code), Reason: sc.Reason}))
			return outcome(checkError(err, sc.Expect))
		case "error-event":
			ev, err := realtime.DecodeEvent(sc.Event)
			if err != nil {
				return fixtures.Fail, "decode: " + err.Error()
			}
			ee, ok := ev.(realtime.ErrorEvent)
			if !ok {
				return fixtures.Fail, fmt.Sprintf("decoded %T", ev)
			}
			return outcome(checkError(ee.Err, sc.Expect))
		case "size":
			return outcome(checkSize(t, sc))
		case "event":
			if sc.Raw == nil {
				return fixtures.Fail, "event case without raw"
			}
			return outcome(checkEvent(*sc.Raw, sc.Expect))
		}
		return fixtures.Fail, "unknown kind " + sc.Kind
	})
}

// checkError compares a classified error with class, fatal, code, type, param and notInDescription.
func checkError(err error, want map[string]any) []string {
	var e *openaihttp.Error
	if !errors.As(err, &e) {
		return []string{fmt.Sprintf("not a classified error: %v", err)}
	}
	var p []string
	if w, ok := want["class"]; ok && string(e.Class) != w {
		p = append(p, fmt.Sprintf("class %q, want %v", e.Class, w))
	}
	if w, ok := want["fatal"]; ok && e.Fatal != w {
		p = append(p, fmt.Sprintf("fatal %v, want %v", e.Fatal, w))
	}
	for key, got := range map[string]string{"code": e.Code, "type": e.Type, "param": e.Param} {
		if w, ok := want[key]; ok && got != w {
			p = append(p, fmt.Sprintf("%s %q, want %v", key, got, w))
		}
	}
	return append(p, leaks(strings_(want["notInDescription"]), err.Error())...)
}

func strings_(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, s := range list {
			out = append(out, fmt.Sprint(s))
		}
	}
	return out
}

// checkSize sends one message of exactly MessageBytes over a real WebSocket to a session with ReadLimit.
func checkSize(t *testing.T, sc sessionCase) []string {
	prefix, suffix := `{"type":"response.output_audio_transcript.delta","response_id":"r","delta":"`, `"}`
	pad := sc.MessageBytes - len(prefix) - len(suffix)
	if pad < 0 {
		return []string{"message size too small for the probe event"}
	}
	msg := prefix + strings.Repeat("a", pad) + suffix
	hs := fakeopenai.WebSocket(t, func(c *websocket.Conn) {
		_ = c.Write(context.Background(), websocket.MessageText, []byte(msg))
		_, _, _ = c.Read(context.Background())
	}, nil)
	client, base := fakeopenai.Client(t, hs.URL)
	s, err := realtime.Dial(context.Background(), realtime.DialConfig{
		Client: client, URL: realtime.WebSocketURL(base, "m"), Secret: secret.NewClientSecret("ek_conformance", time.Unix(1800000000, 0)),
		HandshakeTimeout: 5 * time.Second, ReadLimit: sc.ReadLimit,
	})
	if err != nil {
		return []string{"dial: " + err.Error()}
	}
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := s.Next(ctx)
	if sc.Expect["class"] == nil {
		if d, ok := ev.(realtime.TranscriptDelta); err != nil || !ok || len(d.Delta) != pad {
			return []string{fmt.Sprintf("message at the limit: %T %v", ev, err)}
		}
		return nil
	}
	return checkError(err, sc.Expect)
}

// observe flattens a decoded event into the fixture's expectation keys.
func observe(ev realtime.Event) map[string]any {
	m := map[string]any{"type": ev.EventType()}
	switch e := ev.(type) {
	case realtime.SessionCreated:
		m["sessionType"], m["model"] = e.SessionType, e.Model
	case realtime.ResponseCreated:
		m["responseId"], m["metadata"] = e.ResponseID, e.Metadata
	case realtime.AudioDelta:
		m["responseId"], m["pcmHex"] = e.ResponseID, hex.EncodeToString(e.PCM)
	case realtime.TranscriptDelta:
		m["responseId"], m["delta"] = e.ResponseID, e.Delta
	case realtime.ResponseDone:
		m["responseId"], m["status"], m["statusReason"], m["metadata"] = e.ResponseID, string(e.Status), e.StatusReason, e.Metadata
		m["usage"] = map[string]int{
			"inputTokens": e.Usage.InputTokens, "outputTokens": e.Usage.OutputTokens, "inputTextTokens": e.Usage.InputTextTokens,
			"outputTextTokens": e.Usage.OutputTextTokens, "outputAudioTokens": e.Usage.OutputAudioTokens,
		}
	case realtime.RateLimitsUpdated:
		var limits []map[string]any
		for _, l := range e.Limits {
			limits = append(limits, map[string]any{"name": l.Name, "limit": l.Limit, "remaining": l.Remaining, "resetSeconds": l.ResetSeconds})
		}
		m["limits"], m["exhausted"] = limits, e.Exhausted() != nil
	case realtime.ErrorEvent:
		m["class"], m["code"], m["param"] = string(e.Err.Class), e.Err.Code, e.Err.Param
	case realtime.InputCommitted:
		m["itemId"] = e.ItemID
	case realtime.TranscriptionDelta:
		m["itemId"], m["delta"] = e.ItemID, e.Delta
	case realtime.TranscriptionCompleted:
		m["itemId"], m["transcript"] = e.ItemID, e.Transcript
	case realtime.TranscriptionFailed:
		m["itemId"], m["class"] = e.ItemID, string(e.Err.Class)
	case realtime.Ignored:
		m = map[string]any{"ignored": e.Type}
	case realtime.Unknown:
		m = map[string]any{"unknown": e.Type}
	}
	return m
}

// checkEvent decodes raw and compares it with want: the typed fields, or a protocol error naming the type
// (protocolError null: the type need not be named).
func checkEvent(raw string, want map[string]any) []string {
	ev, err := realtime.DecodeEvent([]byte(raw))
	if pe, ok := want["protocolError"]; ok {
		if err == nil {
			return []string{fmt.Sprintf("decoded %T, want a protocol error", ev)}
		}
		p := []string{}
		if c := openaihttp.Classify(err); c != openaihttp.ClassProtocol {
			p = append(p, fmt.Sprintf("class %q, want protocol", c))
		}
		if typ, ok := pe.(string); ok && !strings.Contains(err.Error(), typ) {
			p = append(p, fmt.Sprintf("error %q does not name %s", err, typ))
		}
		return append(p, leaks(strings_(want["notInDescription"]), err.Error())...)
	}
	if err != nil {
		return []string{"decode: " + err.Error()}
	}
	got := observe(ev)
	var p []string
	for key, w := range want {
		if key == "notInDescription" {
			continue
		}
		if !same(got[key], w) {
			p = append(p, fmt.Sprintf("%s = %v, want %v", key, normal(got[key]), w))
		}
	}
	return p
}
