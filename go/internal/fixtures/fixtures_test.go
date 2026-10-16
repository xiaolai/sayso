package fixtures

import (
	"encoding/json"
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
