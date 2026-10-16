package conformance

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/xiaolai/sayso/go/codexapp"
	"github.com/xiaolai/sayso/go/internal/oracle"
	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/pcm16"
	"github.com/xiaolai/sayso/go/realtime"
)

// The operations of tools/differential.py that exported API reaches. Bytes travel as hex, so a document that is not
// UTF-8 reaches the implementation as it was generated; every integer that may exceed 2^53 is a string.
var oracleOps = map[string]oracle.Op{
	// The transport itself: a text input's UTF-8, so a decoding difference shows as itself, not as a verdict.
	"echo": func(in json.RawMessage) (any, error) {
		var c struct{ Text string }
		err := json.Unmarshal(in, &c)
		return hex.EncodeToString([]byte(c.Text)), err
	},
	"wire.valid":  bytesOp(func(b []byte) any { return wire.Valid(b) }),
	"codex.valid": bytesOp(func(b []byte) any { return codexapp.ValidJSON(b) }),
	"event.decode": bytesOp(func(b []byte) any {
		ev, err := realtime.DecodeEvent(b)
		if err != nil {
			var e *openaihttp.Error
			if !errors.As(err, &e) {
				return map[string]any{"unclassified": err.Error()}
			}
			return map[string]any{"failure": errorObservation(e)}
		}
		return map[string]any{"event": eventObservation(ev)}
	}),
	"pcm.encode": func(in json.RawMessage) (any, error) {
		var c struct{ Bits []uint32 }
		if err := json.Unmarshal(in, &c); err != nil {
			return nil, err
		}
		samples := make([]float32, len(c.Bits))
		for i, b := range c.Bits {
			samples[i] = math.Float32frombits(b)
		}
		return hex.EncodeToString(pcm16.Encode(samples)), nil
	},
	"pcm.decode": bytesOp(func(b []byte) any {
		out := []any{}
		for _, f := range pcm16.Decode(b) {
			out = append(out, fmt.Sprintf("%08x", math.Float32bits(f)))
		}
		return out
	}),
	"pcm.wav": func(in json.RawMessage) (any, error) {
		var c struct {
			Hex  string
			Rate int
		}
		b, err := hexInput(in, &c, &c.Hex)
		if err != nil {
			return nil, err
		}
		w, err := pcm16.WAV(b, c.Rate)
		if err != nil {
			return nil, nil
		}
		return hex.EncodeToString(w), nil
	},
	"pcm.validWAV": func(in json.RawMessage) (any, error) {
		var c struct {
			Hex string
			Max int
		}
		b, err := hexInput(in, &c, &c.Hex)
		if err != nil {
			return nil, err
		}
		return pcm16.ValidWAV(b, c.Max), nil
	},
	"pcm.plausible": func(in json.RawMessage) (any, error) {
		var c struct{ Hex, Script string }
		b, err := hexInput(in, &c, &c.Hex)
		if err != nil {
			return nil, err
		}
		return pcm16.Plausible(b, c.Script), nil
	},
}

// bytesOp is an operation on {"hex"} alone.
func bytesOp(f func([]byte) any) oracle.Op {
	return func(in json.RawMessage) (any, error) {
		var c struct{ Hex string }
		b, err := hexInput(in, &c, &c.Hex)
		if err != nil {
			return nil, err
		}
		return f(b), nil
	}
}

// hexInput decodes in into v and returns the bytes of the hex field *h points into.
func hexInput(in json.RawMessage, v any, h *string) ([]byte, error) {
	if err := json.Unmarshal(in, v); err != nil {
		return nil, err
	}
	return hex.DecodeString(*h)
}

func errorObservation(e *openaihttp.Error) map[string]any {
	return map[string]any{"class": string(e.Class), "type": e.Type, "code": e.Code, "param": e.Param, "message": e.Message,
		"fatal": e.Fatal}
}

func metadataObservation(m map[string]string) any {
	if m == nil {
		return nil
	}
	return m
}

func eventObservation(ev realtime.Event) map[string]any {
	o := map[string]any{"type": ev.EventType()}
	switch e := ev.(type) {
	case realtime.SessionCreated:
		o["sessionType"], o["model"] = e.SessionType, e.Model
	case realtime.ResponseCreated:
		o["responseID"], o["metadata"] = e.ResponseID, metadataObservation(e.Metadata)
	case realtime.AudioDelta:
		o["responseID"], o["pcm"] = e.ResponseID, hex.EncodeToString(e.PCM)
	case realtime.TranscriptDelta:
		o["responseID"], o["delta"] = e.ResponseID, e.Delta
	case realtime.ResponseDone:
		u := e.Usage
		o["responseID"], o["status"], o["statusReason"], o["metadata"] = e.ResponseID, string(e.Status), e.StatusReason,
			metadataObservation(e.Metadata)
		o["usage"] = map[string]any{"input": i64(u.InputTokens), "output": i64(u.OutputTokens),
			"inputText": i64(u.InputTextTokens), "outputText": i64(u.OutputTextTokens),
			"outputAudio": i64(u.OutputAudioTokens), "detailed": u.Detailed}
	case realtime.RateLimitsUpdated:
		limits := []any{}
		for _, l := range e.Limits {
			limits = append(limits, map[string]any{"name": l.Name, "limit": i64(l.Limit), "remaining": i64(l.Remaining),
				"resetSeconds": fmt.Sprintf("%016x", math.Float64bits(l.ResetSeconds))})
		}
		o["limits"] = limits
	case realtime.ErrorEvent:
		o["error"] = errorObservation(e.Err)
	case realtime.InputCommitted:
		o["itemID"] = e.ItemID
	case realtime.TranscriptionDelta:
		o["itemID"], o["delta"] = e.ItemID, e.Delta
	case realtime.TranscriptionCompleted:
		o["itemID"], o["transcript"] = e.ItemID, e.Transcript
	case realtime.TranscriptionFailed:
		o["itemID"], o["error"] = e.ItemID, errorObservation(e.Err)
	case realtime.Ignored:
		o["ignored"] = true
	case realtime.Unknown:
		o["unknown"] = true
	default:
		o["undescribed"] = fmt.Sprintf("%T", ev)
	}
	return o
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }

func TestOracle(t *testing.T) {
	h := func(s string) string { return `{"hex":"` + hex.EncodeToString([]byte(s)) + `"}` }
	oracle.Run(t, oracleOps, []oracle.Check{
		{"echo", `{"text":"a\u00e9\ud83d\ude00"}`, `"61c3a9f09f9880"`},
		{"echo", `{"text":"\ufeffa"}`, `"efbbbf61"`}, // a leading U+FEFF survives the transport
		{"wire.valid", h(` {"a":[1,-0.5e3,"\ud83d\ude00"]} `), `true`},
		{"wire.valid", h(`{"a":1,"a":2}`), `false`},
		{"wire.valid", `{"hex":"7b2261223a22ff227d"}`, `false`},
		{"codex.valid", h(`{"n":1e308}`), `true`},
		{"codex.valid", h(`{"n":1e309}`), `false`},
		{"event.decode", h(`{"type":"response.done","response":{"id":"r","status":"completed","usage":{"input_tokens":9223372036854775807}}}`),
			`{"event":{"type":"response.done","responseID":"r","status":"completed","statusReason":"","metadata":null,` +
				`"usage":{"input":"9223372036854775807","output":"0","inputText":"0","outputText":"0","outputAudio":"0","detailed":false}}}`},
		{"event.decode", h(`{"type":"rate_limits.updated","rate_limits":[{"name":"n","limit":1,"remaining":0,"reset_seconds":0.5}]}`),
			`{"event":{"type":"rate_limits.updated","limits":[{"name":"n","limit":"1","remaining":"0","resetSeconds":"3fe0000000000000"}]}}`},
		{"event.decode", h(`{"type":"error","error":{"type":"invalid_request_error","code":5,"param":"p","message":"m"}}`),
			`{"event":{"type":"error","error":{"class":"invalid-request","type":"invalid_request_error","code":"5","param":"p","message":"m","fatal":false}}}`},
		{"event.decode", h(`{"type":"response.output_audio.delta","response_id":"r","delta":"AAE="}`),
			`{"event":{"type":"response.output_audio.delta","responseID":"r","pcm":"0001"}}`},
		{"event.decode", h(`{"type":"session.updated","x":1e999}`), `{"event":{"type":"session.updated","ignored":true}}`},
		{"event.decode", h(`{"type":"response.created","response":{"id":1}}`),
			`{"failure":{"class":"protocol","type":"","code":"","param":"","message":"realtime event response.created: a field has the wrong type","fatal":false}}`},
		{"event.decode", h(`[`),
			`{"failure":{"class":"protocol","type":"","code":"","param":"","message":"realtime event: not valid JSON","fatal":false}}`},
		{"pcm.encode", `{"bits":[1065353216,3212836864,2143289344,1056964608]}`, `"ff7f01800000ff3f"`},
		{"pcm.decode", `{"hex":"0080ff7f01"}`, `["bf800000","3f7ffe00"]`},
		{"pcm.wav", `{"hex":"0100","rate":24000}`, `"524946462600000057415645666d74201000000001000100c05d000080bb000002001000646174610200000001` + `00"`},
		{"pcm.wav", `{"hex":"","rate":0}`, `null`},
		{"pcm.validWAV", `{"hex":"524946462600000057415645666d74201000000001000100c05d000080bb0000020010006461746102000000` + `0100","max":2}`, `true`},
		{"pcm.validWAV", `{"hex":"524946462600000057415645666d74201000000001000100c05d000080bb0000020010006461746102000000` + `0100","max":1}`, `false`},
		{"pcm.plausible", `{"hex":"4801","script":"a"}`, `false`},
		{"pcm.plausible", `{"hex":"` + repeatHex("4801", 960) + `","script":"a"}`, `true`},
	})
}

func repeatHex(h string, n int) string {
	out := make([]byte, 0, len(h)*n)
	for range n {
		out = append(out, h...)
	}
	return string(out)
}
