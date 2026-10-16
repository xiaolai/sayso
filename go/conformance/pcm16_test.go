package conformance

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"testing"

	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/pcm16"
)

type pcmCase struct {
	caseHead
	Op            string  `json:"op"`
	Input         []any   `json:"input"`
	ExpectSamples []int   `json:"expectSamples"`
	ExpectHex     *string `json:"expectHex"`
	ExpectError   bool    `json:"expectError"`
	Hex           string  `json:"hex"`
	SliceFrom     int     `json:"sliceFrom"`
	Rate          int     `json:"rate"`
	PCMHex        string  `json:"pcmHex"`
	CapBytes      int     `json:"capBytes"`
	Expect        *bool   `json:"expect"`
	PCM           struct {
		Samples int `json:"samples"`
		Value   int `json:"value"`
	} `json:"pcm"`
	Script string `json:"script"`
}

// pcmMembers lists, per op, the case members it reads besides the head and op.
var pcmMembers = map[string][]string{
	"encode":    {"input", "expectSamples", "expectHex"},
	"decode":    {"hex", "sliceFrom", "expectSamples"},
	"wav":       {"rate", "pcmHex", "expectHex", "expectError"},
	"validWAV":  {"hex", "capBytes", "expect"},
	"plausible": {"pcm", "script", "expect"},
}

func TestPCM16(t *testing.T) {
	run(t, "pcm16", func(t *testing.T, c fixtures.Case) (fixtures.Outcome, string) {
		var pc pcmCase
		if err := decode(c, &pc); err != nil {
			return fixtures.Fail, err.Error()
		}
		if known, ok := pcmMembers[pc.Op]; ok {
			if p := unknownMembers(c, append([]string{"op"}, known...)...); len(p) > 0 {
				return outcome(p)
			}
		}
		return outcome(checkPCM(pc))
	})
}

func checkPCM(pc pcmCase) []string {
	if (pc.Op == "encode" || pc.Op == "decode") && pc.ExpectSamples == nil {
		// Without them the case would check the hex alone, or nothing.
		return []string{map[string]string{"encode": "an encode", "decode": "a decode"}[pc.Op] + " case needs expectSamples"}
	}
	switch pc.Op {
	case "encode":
		in := make([]float32, len(pc.Input))
		for i, v := range pc.Input {
			switch x := v.(type) {
			case float64:
				in[i] = float32(x)
			case string:
				in[i] = map[string]float32{"NaN": float32(math.NaN()), "Infinity": float32(math.Inf(1)), "-Infinity": float32(math.Inf(-1))}[x]
			}
		}
		out := pcm16.Encode(in)
		var p []string
		if got := samples(out); !same(got, pc.ExpectSamples) {
			p = append(p, fmt.Sprintf("samples %v, want %v", got, pc.ExpectSamples))
		}
		if pc.ExpectHex != nil && hex.EncodeToString(out) != *pc.ExpectHex {
			p = append(p, fmt.Sprintf("hex %x, want %s", out, *pc.ExpectHex))
		}
		return p
	case "decode":
		raw, err := hex.DecodeString(pc.Hex)
		if err != nil {
			return []string{err.Error()}
		}
		got := pcm16.Decode(raw[pc.SliceFrom:])
		want := make([]float32, len(pc.ExpectSamples))
		for i, s := range pc.ExpectSamples {
			want[i] = float32(s) / 32768
		}
		if !same(got, want) {
			return []string{fmt.Sprintf("decoded %v, want %v", got, want)}
		}
		return nil
	case "wav":
		raw, err := hex.DecodeString(pc.PCMHex)
		if err != nil {
			return []string{err.Error()}
		}
		wav, err := pcm16.WAV(raw, pc.Rate)
		if pc.ExpectError {
			if err == nil {
				return []string{"wav succeeded, want an error"}
			}
			return nil
		}
		if err != nil {
			return []string{err.Error()}
		}
		if got := hex.EncodeToString(wav); pc.ExpectHex == nil || got != *pc.ExpectHex {
			return []string{fmt.Sprintf("wav %s", got)}
		}
		return nil
	case "validWAV":
		raw, err := hex.DecodeString(pc.Hex)
		if err != nil || pc.Expect == nil {
			return []string{fmt.Sprintf("bad case: %v", err)}
		}
		if got := pcm16.ValidWAV(raw, pc.CapBytes); got != *pc.Expect {
			return []string{fmt.Sprintf("ValidWAV %v, want %v", got, *pc.Expect)}
		}
		return nil
	case "plausible":
		var b bytes.Buffer
		for range pc.PCM.Samples {
			_ = binary.Write(&b, binary.LittleEndian, int16(pc.PCM.Value))
		}
		if pc.Expect == nil {
			return []string{"bad case"}
		}
		if got := pcm16.Plausible(b.Bytes(), pc.Script); got != *pc.Expect {
			return []string{fmt.Sprintf("Plausible %v, want %v", got, *pc.Expect)}
		}
		return nil
	}
	return []string{"unknown op " + pc.Op}
}

func samples(pcm []byte) []int {
	out := make([]int, len(pcm)/2)
	for i := range out {
		out[i] = int(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
	}
	return out
}
