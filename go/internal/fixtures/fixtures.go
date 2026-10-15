// Package fixtures loads the shared fixture corpus through its manifest and records each fixture's outcome for the
// parity check (spec/parity.md). The corpus is read from the repository checkout; a missing manifest or file is an
// error, never an empty corpus.
package fixtures

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Dir is the fixture folder relative to a package inside go/.
const Dir = "../../fixtures"

// Entry is one manifest line.
type Entry struct {
	ID, File, Component string
	Requires            []string
}

// Case is one fixture: its id, component and raw JSON.
type Case struct {
	ID        string
	Component string
	Requires  []string
	Raw       json.RawMessage
}

// Load returns every case of component, in manifest order, after checking that the files hold exactly the manifest's
// ids for that component.
func Load(component string) ([]Case, error) {
	data, err := os.ReadFile(filepath.Join(Dir, "MANIFEST.json"))
	if err != nil {
		return nil, fmt.Errorf("fixtures are read from the repository checkout: %w", err)
	}
	var m struct {
		Count    int     `json:"count"`
		Fixtures []Entry `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Count == 0 || m.Count != len(m.Fixtures) {
		return nil, fmt.Errorf("manifest count %d, %d entries", m.Count, len(m.Fixtures))
	}
	files := map[string]bool{}
	want := map[string]Entry{}
	var order []string
	for _, e := range m.Fixtures {
		if e.Component == component {
			files[e.File] = true
			want[e.ID] = e
			order = append(order, e.ID)
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("manifest lists no %s fixtures", component)
	}
	got := map[string]Case{}
	for file := range files {
		raws, err := read(filepath.Join(Dir, file))
		if err != nil {
			return nil, err
		}
		for _, raw := range raws {
			var head struct {
				ID, Component string
				Requires      []string
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			if head.Component != component {
				continue
			}
			if _, dup := got[head.ID]; dup {
				return nil, fmt.Errorf("fixture %s appears twice", head.ID)
			}
			if _, ok := want[head.ID]; !ok {
				return nil, fmt.Errorf("fixture %s is not in the manifest", head.ID)
			}
			got[head.ID] = Case{ID: head.ID, Component: head.Component, Requires: head.Requires, Raw: raw}
		}
	}
	out := make([]Case, 0, len(order))
	for _, id := range order {
		c, ok := got[id]
		if !ok {
			return nil, fmt.Errorf("fixture %s is in the manifest but not in its file", id)
		}
		out = append(out, c)
	}
	return out, nil
}

func read(path string) ([]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(path) == ".jsonl" {
		var out []json.RawMessage
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
				out = append(out, append(json.RawMessage(nil), line...))
			}
		}
		return out, sc.Err()
	}
	var out []json.RawMessage
	return out, json.Unmarshal(data, &out)
}

// JWT builds the unsigned token a {"$jwt": claims} placeholder stands for (spec/fixtures.md).
func JWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	body, _ := json.Marshal(claims) // encoding/json writes map keys sorted, as the contract requires
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc.EncodeToString(body) + "." + enc.EncodeToString([]byte("sig"))
}

// Substitute replaces every {"$jwt": {...}} in v, a decoded JSON value, by its token.
func Substitute(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if claims, ok := t["$jwt"].(map[string]any); ok && len(t) == 1 {
			return JWT(claims)
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = Substitute(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Substitute(x)
		}
		return out
	}
	return v
}

// Outcome is a fixture's result for the parity check.
type Outcome string

// Outcomes.
const (
	Pass   Outcome = "pass"
	Fail   Outcome = "fail"
	Exempt Outcome = "exempt"
)

// Results collects outcomes across a test binary.
type Results struct {
	mu  sync.Mutex
	out map[string]Outcome
}

// Record stores one outcome; a fixture recorded twice is a harness error and records fail.
func (r *Results) Record(id string, o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		r.out = map[string]Outcome{}
	}
	if _, dup := r.out[id]; dup {
		o = Fail
	}
	r.out[id] = o
}

// Write writes results-go.json for tools/parity.py.
func (r *Results) Write(path string, components, capabilities []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sort.Strings(components)
	doc := map[string]any{"implementation": "go", "components": components, "capabilities": capabilities, "results": r.out}
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
