package oracle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func echo(input json.RawMessage) (any, error) {
	var v any
	err := json.Unmarshal(input, &v)
	return v, err
}

// An op name holding a character Go quotes differently from JSON, a NUL here, still makes a valid request line.
func TestSelfCheckEncodesAnyOpName(t *testing.T) {
	Run(t, map[string]Op{"a\x00b": echo}, []Check{{"a\x00b", `{"k":"<&>"}`, `{"k":"<&>"}`}})
}

// No ops and no checks is no lines, not one empty line.
func TestSelfCheckOfNothing(t *testing.T) {
	Run(t, map[string]Op{}, nil)
}

// A malformed request is reported at its physical line, blank lines counted.
func TestAnswerReportsThePhysicalLine(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	if err := os.WriteFile(in, []byte(`{"id":"1","op":"echo","input":1}`+"\n\n\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := answer(in, filepath.Join(dir, "out.jsonl"), map[string]Op{"echo": echo})
	if err == nil || !strings.HasPrefix(err.Error(), "line 4:") {
		t.Errorf("err = %v, want it at line 4", err)
	}
}
