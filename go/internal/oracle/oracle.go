// Package oracle answers the differential harness (tools/differential.py) for one package's operations: it reads
// {"id", "op", "input"} lines from the file SAYSO_ORACLE_IN names and writes one {"id", "op", "output"} line per case
// to SAYSO_ORACLE_OUT. Without SAYSO_ORACLE_IN it runs the caller's built-in checks through the same files instead, so
// every test run exercises the oracle and none skips it.
package oracle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// Op computes one operation's observation from its input. An error means the input does not have the operation's
// shape, which is a harness bug, never an observation.
type Op func(input json.RawMessage) (any, error)

// Check is one built-in case: an operation, its input and the observation it must produce, both as JSON. An array,
// not a struct, so the checks read as a table without vet's objection to unkeyed fields.
type Check [3]string

// Run answers the cases in SAYSO_ORACLE_IN, or runs checks when it is unset. Every operation must have a check.
func Run(t *testing.T, ops map[string]Op, checks []Check) {
	in := os.Getenv("SAYSO_ORACLE_IN")
	if in == "" {
		selfCheck(t, ops, checks)
		return
	}
	out := os.Getenv("SAYSO_ORACLE_OUT")
	if out == "" {
		t.Fatal("SAYSO_ORACLE_IN is set without SAYSO_ORACLE_OUT")
	}
	n, err := answer(in, out, ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("oracle: %d cases", n)
}

type request struct {
	ID    string          `json:"id"`
	Op    string          `json:"op"`
	Input json.RawMessage `json:"input"`
}

type response struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Output any    `json:"output"`
}

// answer writes each case's line as soon as it is computed, unbuffered, so a case that crashes the process is the
// first one missing from out.
func answer(in, out string, ops map[string]Op) (int, error) {
	data, err := os.ReadFile(in)
	if err != nil {
		return 0, err
	}
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r request
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return n, fmt.Errorf("line %d: %w", n+1, err)
		}
		op, ok := ops[r.Op]
		if !ok {
			return n, fmt.Errorf("%s: op %q is not answered by this package", r.ID, r.Op)
		}
		v, err := op(r.Input)
		if err != nil {
			return n, fmt.Errorf("%s: %s: %w", r.ID, r.Op, err)
		}
		var line bytes.Buffer
		enc := json.NewEncoder(&line) // one line, newline included; < > & as written
		enc.SetEscapeHTML(false)
		if err := enc.Encode(response{r.ID, r.Op, v}); err != nil {
			return n, fmt.Errorf("%s: %w", r.ID, err)
		}
		if _, err := f.Write(line.Bytes()); err != nil {
			return n, err
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, f.Close()
}

// selfCheck runs checks through answer's files and compares each observation with its expected one by JSON meaning.
func selfCheck(t *testing.T, ops map[string]Op, checks []Check) {
	covered := map[string]bool{}
	var lines bytes.Buffer
	for i, c := range checks {
		if ops[c[0]] == nil {
			t.Fatalf("check %d names op %q, which this package does not answer", i, c[0])
		}
		covered[c[0]] = true
		fmt.Fprintf(&lines, `{"id":"check-%d","op":%q,"input":%s}`+"\n", i, c[0], c[1])
	}
	var missing []string
	for name := range ops {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("ops without a built-in check: %v", missing)
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.jsonl"), filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(in, lines.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := answer(in, out, ops)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(checks) {
		t.Fatalf("answered %d of %d checks", n, len(checks))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(got) != len(checks) {
		t.Fatalf("wrote %d lines for %d checks", len(got), len(checks))
	}
	for i, c := range checks {
		var r struct {
			ID     string
			Output any
		}
		var want any
		if err := json.Unmarshal(got[i], &r); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if err := json.Unmarshal([]byte(c[2]), &want); err != nil {
			t.Fatalf("check %d: expected output: %v", i, err)
		}
		if r.ID != fmt.Sprintf("check-%d", i) || !reflect.DeepEqual(r.Output, want) {
			t.Errorf("check %d (%s %s): got %s, want %s", i, c[0], c[1], got[i], c[2])
		}
	}
	t.Logf("oracle: %d built-in checks", len(checks))
}
