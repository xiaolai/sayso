package realtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

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

// Ignored is a documented event the package does not consume.
type Ignored struct{ Type string }

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

// Wire shapes. Required fields are pointers so absence is detectable.
type (
	wireError struct {
		Type    *string         `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Param   *string         `json:"param"`
	}
	wireResponse struct {
		ID            *string `json:"id"`
		Status        *string `json:"status"`
		StatusDetails *struct {
			Reason string `json:"reason"`
		} `json:"status_details"`
		Usage *struct {
			InputTokens       int `json:"input_tokens"`
			OutputTokens      int `json:"output_tokens"`
			InputTokenDetails *struct {
				TextTokens int `json:"text_tokens"`
			} `json:"input_token_details"`
			OutputTokenDetails *struct {
				TextTokens  int `json:"text_tokens"`
				AudioTokens int `json:"audio_tokens"`
			} `json:"output_token_details"`
		} `json:"usage"`
		Metadata map[string]string `json:"metadata"`
	}
	wireEvent struct {
		Type       *string       `json:"type"`
		ResponseID *string       `json:"response_id"`
		ItemID     *string       `json:"item_id"`
		Delta      *string       `json:"delta"`
		Transcript *string       `json:"transcript"`
		Response   *wireResponse `json:"response"`
		Session    *struct {
			Type  *string `json:"type"`
			Model string  `json:"model"`
		} `json:"session"`
		RateLimits *[]struct {
			Name         string  `json:"name"`
			Limit        int64   `json:"limit"`
			Remaining    *int64  `json:"remaining"`
			ResetSeconds float64 `json:"reset_seconds"`
		} `json:"rate_limits"`
		Error *wireError `json:"error"`
	}
)

// protocolError is the NET-04 failure: a malformed event or a missing required
// field. It names the event type, never the payload.
func protocolError(typ, why string) error {
	if typ == "" {
		return &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: "realtime event: " + why}
	}
	return &openaihttp.Error{Class: openaihttp.ClassProtocol, Message: fmt.Sprintf("realtime event %s: %s", secret.Scrub(typ), why)}
}

func missing(typ, field string) error { return protocolError(typ, "missing "+field) }

// DecodeEvent decodes one server event by its type. Unknown types decode to
// Unknown; a malformed event or a missing required field is a protocol error.
func DecodeEvent(data []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, protocolError("", "not valid JSON")
	}
	if w.Type == nil || *w.Type == "" {
		return nil, protocolError("", "missing type")
	}
	typ := *w.Type
	switch typ {
	case TypeSessionCreated:
		if w.Session == nil || w.Session.Type == nil {
			return nil, missing(typ, "session.type")
		}
		return SessionCreated{SessionType: *w.Session.Type, Model: w.Session.Model}, nil
	case TypeResponseCreated:
		if w.Response == nil || w.Response.ID == nil {
			return nil, missing(typ, "response.id")
		}
		return ResponseCreated{ResponseID: *w.Response.ID, Metadata: w.Response.Metadata}, nil
	case TypeAudioDelta:
		if w.ResponseID == nil || w.Delta == nil {
			return nil, missing(typ, "response_id or delta")
		}
		pcm, err := base64.StdEncoding.DecodeString(*w.Delta)
		if err != nil {
			return nil, protocolError(typ, "delta is not base64")
		}
		return AudioDelta{ResponseID: *w.ResponseID, PCM: pcm}, nil
	case TypeTranscriptDelta:
		if w.ResponseID == nil || w.Delta == nil {
			return nil, missing(typ, "response_id or delta")
		}
		return TranscriptDelta{ResponseID: *w.ResponseID, Delta: *w.Delta}, nil
	case TypeResponseDone:
		return decodeDone(typ, w.Response)
	case TypeRateLimitsUpdated:
		if w.RateLimits == nil {
			return nil, missing(typ, "rate_limits")
		}
		ev := RateLimitsUpdated{Limits: make([]RateLimit, 0, len(*w.RateLimits))}
		for _, l := range *w.RateLimits {
			if l.Remaining == nil {
				return nil, missing(typ, "rate_limits[].remaining")
			}
			ev.Limits = append(ev.Limits, RateLimit{Name: l.Name, Limit: l.Limit, Remaining: *l.Remaining, ResetSeconds: l.ResetSeconds})
		}
		return ev, nil
	case TypeError:
		e, err := decodeError(typ, w.Error)
		if err != nil {
			return nil, err
		}
		return ErrorEvent{Err: e}, nil
	case TypeInputCommitted:
		if w.ItemID == nil {
			return nil, missing(typ, "item_id")
		}
		return InputCommitted{ItemID: *w.ItemID}, nil
	case TypeTranscriptionDelta:
		if w.ItemID == nil || w.Delta == nil {
			return nil, missing(typ, "item_id or delta")
		}
		return TranscriptionDelta{ItemID: *w.ItemID, Delta: *w.Delta}, nil
	case TypeTranscriptionCompleted:
		if w.ItemID == nil || w.Transcript == nil {
			return nil, missing(typ, "item_id or transcript")
		}
		return TranscriptionCompleted{ItemID: *w.ItemID, Transcript: *w.Transcript}, nil
	case TypeTranscriptionFailed:
		if w.ItemID == nil {
			return nil, missing(typ, "item_id")
		}
		e, err := decodeError(typ, w.Error)
		if err != nil {
			return nil, err
		}
		return TranscriptionFailed{ItemID: *w.ItemID, Err: e}, nil
	}
	if ignoredTypes[typ] {
		return Ignored{Type: typ}, nil
	}
	return Unknown{Type: typ, Raw: append([]byte(nil), data...)}, nil
}

func decodeDone(typ string, r *wireResponse) (Event, error) {
	if r == nil {
		return nil, missing(typ, "response")
	}
	if r.Status == nil {
		return nil, missing(typ, "response.status")
	}
	if r.ID == nil {
		return nil, missing(typ, "response.id")
	}
	switch ResponseStatus(*r.Status) {
	case StatusCompleted, StatusFailed, StatusIncomplete, StatusCancelled:
	default:
		return nil, protocolError(typ, "unknown response.status") // the value is payload, never named
	}
	ev := ResponseDone{ResponseID: *r.ID, Status: ResponseStatus(*r.Status), Metadata: r.Metadata}
	if r.StatusDetails != nil {
		ev.StatusReason = r.StatusDetails.Reason
	}
	if r.Usage != nil {
		ev.Usage = TokenUsage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
		if d := r.Usage.InputTokenDetails; d != nil {
			ev.Usage.InputTextTokens = d.TextTokens
		}
		if d := r.Usage.OutputTokenDetails; d != nil {
			ev.Usage.OutputTextTokens, ev.Usage.OutputAudioTokens, ev.Usage.Detailed = d.TextTokens, d.AudioTokens, true
		}
	}
	return ev, nil
}

func decodeError(typ string, w *wireError) (*openaihttp.Error, error) {
	if w == nil || w.Type == nil {
		return nil, missing(typ, "error.type")
	}
	param := ""
	if w.Param != nil {
		param = *w.Param
	}
	return openaihttp.ErrorEvent(*w.Type, openaihttp.RawString(w.Code), param, w.Message), nil
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
// output_token_details, their text and audio parts.
type TokenUsage struct {
	InputTokens, OutputTokens int
	InputTextTokens           int
	OutputTextTokens          int
	OutputAudioTokens         int
	// Detailed is set when output_token_details was present, so a zero audio count is real.
	Detailed bool
}
