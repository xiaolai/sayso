package saidguard

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/internal/oracle"
)

// The said guard's operations for tools/differential.py. said.tokens is the transcript side's tokens, which the spec
// defines by normalisation alone, so two implementations that differ there differ on some verdict. A side the guard
// refuses unread has none: beyond MaxBytes, or with a run of marks that normalisers differ on (Go's NFKC inserts
// U+034F after 30, Foundation's does not), so the observation is "refused", which compares the refusal itself.
var oracleOps = map[string]oracle.Op{
	"said.matches": func(in json.RawMessage) (any, error) {
		var c struct{ Said, Spoken, Language string }
		if err := json.Unmarshal(in, &c); err != nil {
			return nil, err
		}
		return Matches(c.Said, c.Spoken, c.Language), nil
	},
	"said.tokens": func(in json.RawMessage) (any, error) {
		var c struct{ Text, Language string }
		if err := json.Unmarshal(in, &c); err != nil {
			return nil, err
		}
		if len(c.Text) > MaxBytes || svOverlongMarks(c.Text) {
			return "refused", nil
		}
		out := []any{}
		for _, t := range svTranscriptTokens(c.Text, c.Language) {
			out = append(out, map[string]any{"text": t.text, "link": t.link, "lead": t.lead, "trail": t.trail})
		}
		return out, nil
	},
	"han.parse": func(in json.RawMessage) (any, error) {
		var c struct{ Text string }
		if err := json.Unmarshal(in, &c); err != nil {
			return nil, err
		}
		n, ok := ParseHanNumeral(c.Text)
		if !ok {
			return nil, nil
		}
		return strconv.FormatUint(n, 10), nil // as text: a uint64 does not survive a float64
	},
}

func TestOracle(t *testing.T) {
	oracle.Run(t, oracleOps, []oracle.Check{
		{"said.matches", `{"said":"Hello, world.","spoken":"hello world","language":"en"}`, `true`},
		{"said.matches", `{"said":"twenty one","spoken":"21","language":"en"}`, `true`},
		{"said.matches", `{"said":"21","spoken":"-21","language":"en"}`, `false`},
		{"said.matches", `{"said":"一百一","spoken":"110","language":"zh"}`, `true`},
		{"said.tokens", `{"text":"E-mail ‘me’, 1,2%","language":"en"}`,
			`[{"text":"e","link":true,"lead":0,"trail":0},{"text":"mail","link":false,"lead":0,"trail":0},` +
				`{"text":"me","link":false,"lead":1,"trail":1},{"text":"1","link":false,"lead":0,"trail":0},` +
				`{"text":"2","link":false,"lead":0,"trail":0},{"text":"%","link":false,"lead":0,"trail":0}]`},
		{"said.tokens", `{"text":"","language":"zh"}`, `[]`},
		{"said.tokens", `{"text":"x` + strings.Repeat(`\u0305`, 29) + `","language":"en"}`,
			`[{"text":"x` + strings.Repeat(`\u0305`, 29) + `","link":false,"lead":0,"trail":0}]`},
		{"said.tokens", `{"text":"x` + strings.Repeat(`\u0305`, 30) + `","language":"en"}`, `"refused"`},
		{"han.parse", `{"text":"两万五"}`, `"25000"`},
		{"han.parse", `{"text":"一千万亿"}`, `"1000000000000000"`},
		{"han.parse", `{"text":"三五"}`, `null`},
	})
}
