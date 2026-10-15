package realtime_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/realtime"
)

// FuzzDecodeEvent: any input decodes to exactly one of an event or a protocol error.
func FuzzDecodeEvent(f *testing.F) {
	// The checked-in corpus is testdata/fuzz/FuzzDecodeEvent; every shared event fixture seeds it too.
	data, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "events", "events.json"))
	if err != nil {
		f.Fatal(err)
	}
	var cases []struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) == 0 {
		f.Fatalf("event fixtures: %v", err)
	}
	for _, c := range cases {
		f.Add([]byte(c.Raw))
	}
	if _, err := os.Stat(filepath.Join("testdata", "fuzz", "FuzzDecodeEvent")); err != nil {
		f.Fatalf("seed corpus missing: %v", err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ev, err := realtime.DecodeEvent(data)
		if (ev == nil) == (err == nil) {
			t.Fatalf("event %v and error %v", ev, err)
		}
		if err != nil && openaihttp.Classify(err) != openaihttp.ClassProtocol {
			t.Fatalf("decode error of class %s", openaihttp.Classify(err))
		}
	})
}

// The usage details split text from audio tokens.
func TestResponseDoneUsageDetails(t *testing.T) {
	ev, err := realtime.DecodeEvent([]byte(`{"type":"response.done","response":{"id":"r","status":"completed",` +
		`"usage":{"input_tokens":120,"output_tokens":435,"input_token_details":{"text_tokens":118,"audio_tokens":2},` +
		`"output_token_details":{"text_tokens":80,"audio_tokens":355}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ev.(realtime.ResponseDone).Usage
	want := realtime.TokenUsage{InputTokens: 120, OutputTokens: 435, InputTextTokens: 118, OutputTextTokens: 80, OutputAudioTokens: 355, Detailed: true}
	if got != want {
		t.Fatalf("usage %+v, want %+v", got, want)
	}
}

// An unknown event keeps its raw bytes, copied, so a caller can pass it through.
func TestUnknownKeepsRaw(t *testing.T) {
	raw := []byte(`{"type":"response.future","x":1}`)
	ev, err := realtime.DecodeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := ev.(realtime.Unknown)
	if !ok || string(u.Raw) != string(raw) {
		t.Fatalf("got %#v", ev)
	}
	raw[0] = 'X'
	if u.Raw[0] != '{' {
		t.Fatal("Raw aliases the input")
	}
}
