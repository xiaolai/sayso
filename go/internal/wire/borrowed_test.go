package wire

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"
)

// Strings decoded in place equal encoding/json's decoding, escapes of every length included; names match as decoded
// bytes; integers read as encoding/json reads one into an int64.
func TestBorrowedReadsAsEncodingJSON(t *testing.T) {
	for _, s := range []string{
		`""`, `"plain"`, `"\"\\\/\b\f\n\r\t"`, `"Aé€😀"`, `"é€😀"`, `"a\u0000b"`,
		`"\u007f\u0080߿ࠀ￿"`,
	} {
		doc := []byte(s)
		var want string
		if err := json.Unmarshal(doc, &want); err != nil {
			t.Fatal(err)
		}
		got, ok := AppendString(nil, doc, Root(doc))
		if !ok || string(got) != want {
			t.Errorf("%s: %q, want %q", s, got, want)
		}
		if !StringEquals(doc, Root(doc), want) || StringEquals(doc, Root(doc), want+"x") {
			t.Errorf("%s: StringEquals", s)
		}
	}
	// A destination that already holds bytes, with room for the span beyond them, is appended to in place.
	str := []byte(`"abcdef"`)
	dst := make([]byte, 7, 7+len(str))
	if got, ok := AppendString(dst, str, Root(str)); !ok || string(got[7:]) != "abcdef" || &got[0] != &dst[0] {
		t.Errorf("a nonempty destination with room grew or lost the string: %q", got)
	}
	doc := []byte(`{"a":1,"exp":2}`)
	if v, ok := Member(doc, Root(doc), "exp"); !ok || string(doc[v.Start:v.End]) != "2" {
		t.Error("an escaped name did not match")
	}
	if _, ok := Member(doc, Root(doc), "EXP"); ok {
		t.Error("a name matched another case")
	}
	if _, ok := Member([]byte(`[1]`), Root([]byte(`[1]`)), "0"); ok {
		t.Error("an array has members")
	}
	for text, want := range map[string]any{
		"0": int64(0), "-0": int64(0), "7": int64(7), "-7": int64(-7), "9223372036854775807": int64(1<<63 - 1),
		"-9223372036854775808": int64(-1 << 63), "9223372036854775808": nil, "-9223372036854775809": nil,
		"1.0": nil, "1e3": nil, `"1"`: nil, "true": nil, "null": nil,
	} {
		doc := []byte(text)
		got, ok := Int64Of(doc, Root(doc))
		var ref int64
		refOK := json.Unmarshal(doc, &ref) == nil && text != "null" // encoding/json leaves an int64 alone for null
		if ok != refOK || (ok && got != ref) || (want != nil) != ok || (ok && got != want.(int64)) {
			t.Errorf("%s: %d %v, want %v", text, got, ok, want)
		}
	}
}

// The in-place duplicate check refuses exactly what jsontext's own refuses.
func TestUniqueNamesMatchesJsontext(t *testing.T) {
	for _, s := range []string{
		`{}`, `{"a":1,"b":2}`, `{"a":1,"a":2}`, `{"é":1,"é":2}`, `{"é":1,"é":2}`, `{"a":{"a":1},"b":[{"a":1,"a":2}]}`,
		`[{"x":1},{"x":1}]`, `{"a":"a","b":"a"}`, `{"a":{},"a":[]}`, `{"😀":1,"😀":2}`, `{"a\"":1,"a"":2}`,
	} {
		want := jsontext.Value(s).IsValid()
		if got := Valid([]byte(s)); got != want {
			t.Errorf("%s: Valid %v, jsontext %v", s, got, want)
		}
	}
}
