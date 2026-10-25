package fixtures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRepeatText(t *testing.T) {
	good := map[string]string{
		`{"repeat": "ab", "times": 3, "prefix": "<", "suffix": ">"}`:     "<ababab>",
		`{"repeat": "一亿", "times": 2, "prefix": "", "suffix": ""}`:       "一亿一亿",
		`{"suffix": "x", "prefix": "", "times": 1, "repeat": "dot "}`:    "dot x",
		`{"repeat": "’", "times": 3, "prefix": "五", "suffix": "\n"}`:     "五’’’\n",
		`{"repeat": "a", "times": 1048575, "prefix": "b", "suffix": ""}`: "b" + strings.Repeat("a", 1048575),
	}
	for spec, want := range good {
		if got, err := repeatText(json.RawMessage(spec)); err != nil || got != want {
			t.Errorf("%.70s: got %d bytes, %v; want %d bytes", spec, len(got), err, len(want))
		}
	}
	bad := []string{
		`{"times": 3, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 3, "suffix": ""}`,
		`{"repeat": "a", "times": 3, "prefix": ""}`,
		`{"repeat": null, "times": 3, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": null, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 3, "prefix": "", "suffix": "", "count": 3}`,
		`{"Repeat": "a", "times": 3, "prefix": "", "suffix": ""}`,
		`{"repeat": "", "times": 3, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 0, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": -1, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 1.5, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 2.0, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 1e3, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": "3", "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": true, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 1048577, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 99999999999999999999, "prefix": "", "suffix": ""}`,
		`{"repeat": "ab", "times": 524289, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 1048576, "prefix": "b", "suffix": ""}`,
		`{"repeat": 5, "times": 3, "prefix": "", "suffix": ""}`,
		`{"repeat": "a", "times": 3, "prefix": 1, "suffix": ""}`,
		`{"repeat": "a", "times": 3, "prefix": "", "suffix": ["x"]}`,
		`["a", 3]`,
	}
	for _, spec := range bad {
		if got, err := repeatText(json.RawMessage(spec)); err == nil {
			t.Errorf("%s: accepted as %d bytes", spec, len(got))
		}
	}
}

func TestExpandRepeats(t *testing.T) {
	names := []string{"script", "transcript"}
	// A case without a repeat is passed on as read, byte for byte; only a listed member is expanded.
	for _, raw := range []string{
		`{"id": "x", "script": "a  b", "transcript": "a b", "expect": true}`,
		`{"id": "x", "script": "a", "transcript": "a", "note": {"repeat": "a", "times": 2, "prefix": "", "suffix": ""}}`,
	} {
		if got, err := expandRepeats(json.RawMessage(raw), names); err != nil || string(got) != raw {
			t.Errorf("%s: got %s, %v", raw, got, err)
		}
	}
	got, err := expandRepeats(json.RawMessage(`{"id": "x", "script": {"repeat": "a ", "times": 3, "prefix": "<", "suffix": "b"},
		"transcript": "c&d", "language": "en", "expect": false, "requires": ["spellout:fr"]}`), names)
	var c struct {
		ID, Script, Transcript, Language string
		Expect                           *bool
		Requires                         []string
	}
	if err == nil {
		err = json.Unmarshal(got, &c)
	}
	if err != nil || c.ID != "x" || c.Script != "<a a a b" || c.Transcript != "c&d" || c.Language != "en" ||
		c.Expect == nil || *c.Expect || len(c.Requires) != 1 {
		t.Errorf("expanded case: %+v, %v", c, err)
	}
	// A malformed repeat fails the case and names its member.
	_, err = expandRepeats(json.RawMessage(`{"script": "a", "transcript": {"repeat": "a", "times": -1, "prefix": "", "suffix": ""}}`), names)
	if err == nil || !strings.Contains(err.Error(), "transcript") {
		t.Errorf("malformed transcript repeat: %v", err)
	}
}

// corpus writes a manifest and its files into a fresh folder.
func corpus(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Load refuses a manifest that lists an id twice, a case in another file than its entry names, and a case whose
// requirements differ from its entry's; a corpus that agrees with its manifest loads.
func TestLoadChecksTheManifestEntry(t *testing.T) {
	entry := func(id, file string, requires string) string {
		return `{"id":"` + id + `","file":"` + file + `","component":"c","requires":` + requires + `}`
	}
	manifest := func(entries ...string) string {
		return `{"count":` + strconv.Itoa(len(entries)) + `,"fixtures":[` + strings.Join(entries, ",") + `]}`
	}
	files := map[string]string{
		"a.jsonl": `{"id":"one","component":"c","requires":["x"]}` + "\n",
		"b.jsonl": `{"id":"two","component":"c"}` + "\n",
	}
	good := corpus(t, manifest(entry("one", "a.jsonl", `["x"]`), entry("two", "b.jsonl", `[]`)), files)
	if cases, err := load(good, "c"); err != nil || len(cases) != 2 {
		t.Fatalf("a consistent corpus: %d cases, %v", len(cases), err)
	}
	for name, m := range map[string]string{
		"duplicate id":     manifest(entry("one", "a.jsonl", `["x"]`), entry("one", "a.jsonl", `["x"]`), entry("two", "b.jsonl", `[]`)),
		"wrong file":       manifest(entry("one", "a.jsonl", `["x"]`), entry("two", "a.jsonl", `[]`), entry("pad", "b.jsonl", `[]`)),
		"stale requires":   manifest(entry("one", "a.jsonl", `[]`), entry("two", "b.jsonl", `[]`)),
		"missing requires": manifest(entry("one", "a.jsonl", `["x"]`), entry("two", "b.jsonl", `["y"]`)),
	} {
		if _, err := load(corpus(t, m, files), "c"); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// Write leaves the caller's component list as it was given.
func TestWriteKeepsTheCallersSlice(t *testing.T) {
	components := []string{"z", "a"}
	var r Results
	if err := r.Write(filepath.Join(t.TempDir(), "results.json"), components, nil); err != nil {
		t.Fatal(err)
	}
	if components[0] != "z" || components[1] != "a" {
		t.Errorf("components reordered to %q", components)
	}
}
