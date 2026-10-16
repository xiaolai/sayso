package realtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

// Event is one decoded server event (NET-04). Each consumed type has its own
// struct; Ignored and Unknown carry only the type.
type Event interface {
	EventType() string
}

// Server event types the package consumes (spec/session.md, typed events).
const (
	TypeSessionCreated         = "session.created"
	TypeResponseCreated        = "response.created"
	TypeAudioDelta             = "response.output_audio.delta"
	TypeTranscriptDelta        = "response.output_audio_transcript.delta"
	TypeResponseDone           = "response.done"
	TypeRateLimitsUpdated      = "rate_limits.updated"
	TypeError                  = "error"
	TypeInputCommitted         = "input_audio_buffer.committed"
	TypeTranscriptionDelta     = "conversation.item.input_audio_transcription.delta"
	TypeTranscriptionCompleted = "conversation.item.input_audio_transcription.completed"
	TypeTranscriptionFailed    = "conversation.item.input_audio_transcription.failed"
)

// ignoredTypes are documented events the package does not consume. They are not
// drift, so they are not counted as unknown.
var ignoredTypes = map[string]bool{
	"session.updated":                                     true,
	"response.output_item.added":                          true,
	"response.output_item.done":                           true,
	"response.content_part.added":                         true,
	"response.content_part.done":                          true,
	"response.output_audio.done":                          true,
	"response.output_audio_transcript.done":               true,
	"conversation.item.added":                             true,
	"conversation.item.created":                           true,
	"conversation.item.done":                              true,
	"input_audio_buffer.speech_started":                   true,
	"input_audio_buffer.speech_stopped":                   true,
	"input_audio_buffer.cleared":                          true,
	"conversation.item.input_audio_transcription.segment": true,
}

// SessionCreated is session.created.
type SessionCreated struct {
	SessionType string // "realtime" or "transcription"
	Model       string
}

// ResponseCreated is response.created.
type ResponseCreated struct {
	ResponseID string
	Metadata   map[string]string
}

// AudioDelta is response.output_audio.delta, its base64 PCM decoded.
type AudioDelta struct {
	ResponseID string
	PCM        []byte
}

// TranscriptDelta is response.output_audio_transcript.delta: the model's own
// transcript, used only by a guard.
type TranscriptDelta struct {
	ResponseID string
	Delta      string
}

// ResponseDone is response.done.
type ResponseDone struct {
	ResponseID   string
	Status       ResponseStatus
	StatusReason string // status_details.reason, "" when absent
	Usage        TokenUsage
	Metadata     map[string]string
}

// RateLimit is one entry of rate_limits.updated.
type RateLimit struct {
	Name         string
	Limit        int64
	Remaining    int64
	ResetSeconds float64
}

// RateLimitsUpdated is rate_limits.updated.
type RateLimitsUpdated struct {
	Limits []RateLimit
}

// Exhausted returns a rate-limited error when any limit has nothing left
// (spec/session.md), else nil.
func (e RateLimitsUpdated) Exhausted() error {
	for _, l := range e.Limits {
		if l.Remaining <= 0 {
			return &openaihttp.Error{Class: openaihttp.ClassRateLimited, Message: fmt.Sprintf("rate limit %q exhausted", secret.Scrub(l.Name))}
		}
	}
	return nil
}

// ErrorEvent is a server `error` event, classified (NET-05).
type ErrorEvent struct {
	Err *openaihttp.Error
}

// InputCommitted is input_audio_buffer.committed.
type InputCommitted struct {
	ItemID string
}

// TranscriptionDelta is conversation.item.input_audio_transcription.delta.
type TranscriptionDelta struct {
	ItemID string
	Delta  string
}

// TranscriptionCompleted is conversation.item.input_audio_transcription.completed.
type TranscriptionCompleted struct {
	ItemID     string
	Transcript string
}

// TranscriptionFailed is conversation.item.input_audio_transcription.failed.
type TranscriptionFailed struct {
	ItemID string
	Err    *openaihttp.Error
}

// Ignored is a documented event the package does not consume. Raw is the event as received, so a caller can read a
// type the package does not decode.
type Ignored struct {
	Type string
	Raw  []byte
}

// Unknown is an event type the package has never seen: API drift. Raw is the event as received; a Session counts
// it and logs its type at debug.
type Unknown struct {
	Type string
	Raw  []byte
}

func (SessionCreated) EventType() string         { return TypeSessionCreated }
func (ResponseCreated) EventType() string        { return TypeResponseCreated }
func (AudioDelta) EventType() string             { return TypeAudioDelta }
func (TranscriptDelta) EventType() string        { return TypeTranscriptDelta }
func (ResponseDone) EventType() string           { return TypeResponseDone }
func (RateLimitsUpdated) EventType() string      { return TypeRateLimitsUpdated }
func (ErrorEvent) EventType() string             { return TypeError }
func (InputCommitted) EventType() string         { return TypeInputCommitted }
func (TranscriptionDelta) EventType() string     { return TypeTranscriptionDelta }
func (TranscriptionCompleted) EventType() string { return TypeTranscriptionCompleted }
func (TranscriptionFailed) EventType() string    { return TypeTranscriptionFailed }
func (e Ignored) EventType() string              { return e.Type }
func (e Unknown) EventType() string              { return e.Type }

// protocolError is the NET-04 failure: a malformed event or a missing required
// field. It names the event type, never the payload.
func protocolError(typ, why string) error {
	if typ == "" {
		return &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "realtime event: " + why}
	}
	return &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: fmt.Sprintf("realtime event %s: %s", secret.Scrub(typ), why)}
}

func missing(typ, field string) error { return protocolError(typ, "missing "+field) }

// errNotJSON is a fresh error each time, so a caller annotating one cannot change another's.
func errNotJSON() error { return protocolError("", "not valid JSON") }

// DecodeEvent decodes one server event by its type. The whole event must be strict JSON (package wire); then only
// the fields of its type are read, so an unknown or ignored type is never refused for the shape of a field this
// package does not consume. A field of the wrong type, a missing required field, bad base64 or an unknown
// response.status is a protocol error that names the event type only.
func DecodeEvent(data []byte) (Event, error) {
	root, err := wire.Parse(data)
	if err != nil {
		return nil, errNotJSON()
	}
	if root.IsNull() {
		return nil, protocolError("", "missing type")
	}
	w, err := wire.AsObject(root)
	if err != nil {
		return nil, errNotJSON()
	}
	typ, ok, err := w.String("type")
	if err != nil {
		return nil, errNotJSON()
	}
	if !ok || typ == "" {
		return nil, protocolError("", "missing type")
	}
	ev, err := decodeTyped(typ, w)
	if errors.Is(err, wire.ErrInvalid) {
		return nil, protocolError(typ, "a field has the wrong type") // the type is known: name it
	}
	if err != nil {
		return nil, err
	}
	if ev != nil {
		return ev, nil
	}
	if ignoredTypes[typ] {
		return Ignored{Type: typ, Raw: append([]byte(nil), data...)}, nil
	}
	return Unknown{Type: typ, Raw: append([]byte(nil), data...)}, nil
}

// strings reads string members, all required: ok is false when any is absent or null.
func strs(w wire.Object, names ...string) (vals []string, ok bool, err error) {
	ok = true
	for _, n := range names {
		v, present, err := w.String(n)
		if err != nil {
			return nil, false, err
		}
		ok = ok && present
		vals = append(vals, v)
	}
	return vals, ok, nil
}

// decodeTyped decodes the consumed types; nil, nil for any other type.
func decodeTyped(typ string, w wire.Object) (Event, error) {
	switch typ {
	case TypeSessionCreated:
		sess, ok, err := w.Object("session")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "session.type")
		}
		st, ok, err := sess.String("type")
		if err != nil {
			return nil, err
		}
		model, _, err := sess.String("model")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "session.type")
		}
		return SessionCreated{SessionType: st, Model: model}, nil
	case TypeResponseCreated:
		r, ok, err := w.Object("response")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "response.id")
		}
		id, ok, err := r.String("id")
		if err != nil {
			return nil, err
		}
		meta, _, err := r.StringMap("metadata")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "response.id")
		}
		return ResponseCreated{ResponseID: id, Metadata: meta}, nil
	case TypeAudioDelta:
		v, ok, err := strs(w, "response_id", "delta")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "response_id or delta")
		}
		// StdEncoding skips CR and LF; the contract allows neither (Swift's decoder refuses them).
		if strings.ContainsAny(v[1], "\r\n") {
			return nil, protocolError(typ, "delta is not base64")
		}
		pcm, err := base64.StdEncoding.DecodeString(v[1])
		if err != nil {
			return nil, protocolError(typ, "delta is not base64")
		}
		return AudioDelta{ResponseID: v[0], PCM: pcm}, nil
	case TypeTranscriptDelta:
		v, ok, err := strs(w, "response_id", "delta")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "response_id or delta")
		}
		return TranscriptDelta{ResponseID: v[0], Delta: v[1]}, nil
	case TypeResponseDone:
		return decodeDone(typ, w)
	case TypeRateLimitsUpdated:
		list, ok, err := w.Array("rate_limits")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "rate_limits")
		}
		ev := RateLimitsUpdated{Limits: make([]RateLimit, 0, len(list))}
		for _, item := range list {
			var l RateLimit
			var remaining bool
			if !item.IsNull() {
				o, err := wire.AsObject(item)
				if err != nil {
					return nil, err
				}
				if l.Name, _, err = o.String("name"); err != nil {
					return nil, err
				}
				if l.Limit, _, err = o.Int("limit"); err != nil {
					return nil, err
				}
				if l.Remaining, remaining, err = o.Int("remaining"); err != nil {
					return nil, err
				}
				if l.ResetSeconds, _, err = o.Float("reset_seconds"); err != nil {
					return nil, err
				}
			}
			if !remaining {
				return nil, missing(typ, "rate_limits[].remaining")
			}
			ev.Limits = append(ev.Limits, l)
		}
		return ev, nil
	case TypeError:
		e, err := decodeError(typ, w)
		if err != nil {
			return nil, err
		}
		return ErrorEvent{Err: e}, nil
	case TypeInputCommitted:
		v, ok, err := strs(w, "item_id")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "item_id")
		}
		return InputCommitted{ItemID: v[0]}, nil
	case TypeTranscriptionDelta:
		v, ok, err := strs(w, "item_id", "delta")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "item_id or delta")
		}
		return TranscriptionDelta{ItemID: v[0], Delta: v[1]}, nil
	case TypeTranscriptionCompleted:
		v, ok, err := strs(w, "item_id", "transcript")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, missing(typ, "item_id or transcript")
		}
		return TranscriptionCompleted{ItemID: v[0], Transcript: v[1]}, nil
	case TypeTranscriptionFailed:
		v, ok, err := strs(w, "item_id")
		if err != nil {
			return nil, err
		}
		e, eerr := decodeError(typ, w)
		if errors.Is(eerr, wire.ErrInvalid) {
			return nil, eerr
		}
		if !ok {
			return nil, missing(typ, "item_id")
		}
		if eerr != nil {
			return nil, eerr
		}
		return TranscriptionFailed{ItemID: v[0], Err: e}, nil
	}
	return nil, nil
}

func decodeDone(typ string, w wire.Object) (Event, error) {
	r, ok, err := w.Object("response")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, missing(typ, "response")
	}
	v, _, err := strs(r, "id", "status")
	if err != nil {
		return nil, err
	}
	_, hasID := r.Field("id")
	_, hasStatus := r.Field("status")
	ev := ResponseDone{ResponseID: v[0], Status: ResponseStatus(v[1])}
	if details, ok, err := r.Object("status_details"); err != nil {
		return nil, err
	} else if ok {
		if ev.StatusReason, _, err = details.String("reason"); err != nil {
			return nil, err
		}
	}
	if u, ok, err := r.Object("usage"); err != nil {
		return nil, err
	} else if ok {
		if ev.Usage, err = decodeUsage(u); err != nil {
			return nil, err
		}
	}
	if ev.Metadata, _, err = r.StringMap("metadata"); err != nil {
		return nil, err
	}
	if !hasStatus {
		return nil, missing(typ, "response.status")
	}
	if !hasID {
		return nil, missing(typ, "response.id")
	}
	switch ev.Status {
	case StatusCompleted, StatusFailed, StatusIncomplete, StatusCancelled:
	default:
		return nil, protocolError(typ, "unknown response.status") // the value is payload, never named
	}
	return ev, nil
}

func decodeUsage(u wire.Object) (TokenUsage, error) {
	var t TokenUsage
	in, _, err := u.Int("input_tokens")
	if err != nil {
		return t, err
	}
	out, _, err := u.Int("output_tokens")
	if err != nil {
		return t, err
	}
	t.InputTokens, t.OutputTokens = in, out
	if d, ok, err := u.Object("input_token_details"); err != nil {
		return t, err
	} else if ok {
		text, _, err := d.Int("text_tokens")
		if err != nil {
			return t, err
		}
		t.InputTextTokens = text
	}
	if d, ok, err := u.Object("output_token_details"); err != nil {
		return t, err
	} else if ok {
		text, _, err := d.Int("text_tokens")
		if err != nil {
			return t, err
		}
		audio, _, err := d.Int("audio_tokens")
		if err != nil {
			return t, err
		}
		t.OutputTextTokens, t.OutputAudioTokens, t.Detailed = text, audio, true
	}
	return t, nil
}

// decodeError reads the event's error object; a missing error or error.type is a protocol error.
func decodeError(typ string, w wire.Object) (*openaihttp.Error, error) {
	e, ok, err := w.Object("error")
	if err != nil {
		return nil, err
	}
	var errType, message, param, code string
	var hasType bool
	if ok {
		if errType, hasType, err = e.String("type"); err != nil {
			return nil, err
		}
		if message, _, err = e.String("message"); err != nil {
			return nil, err
		}
		if param, _, err = e.String("param"); err != nil {
			return nil, err
		}
		if code, err = e.Code("code"); err != nil {
			return nil, err
		}
	}
	if !hasType {
		return nil, missing(typ, "error.type")
	}
	return openaihttp.ErrorEvent(errType, code, param, message), nil
}

// ResponseStatus is response.done.status: "completed" | "failed" | "incomplete" | "cancelled".
type ResponseStatus string

// Response statuses reported by response.done.
const (
	StatusCompleted  ResponseStatus = "completed"
	StatusFailed     ResponseStatus = "failed"
	StatusIncomplete ResponseStatus = "incomplete"
	StatusCancelled  ResponseStatus = "cancelled"
)

// TokenUsage is the response.done usage: the totals and, when the server sends input_token_details /
// output_token_details, their text and audio parts. Counts are signed 64-bit on every platform, as the wire allows
// and as Swift keeps them: int is 32 bits on some.
type TokenUsage struct {
	InputTokens, OutputTokens int64
	InputTextTokens           int64
	OutputTextTokens          int64
	OutputAudioTokens         int64
	// Detailed is set when output_token_details was present, so a zero audio count is real.
	Detailed bool
}
