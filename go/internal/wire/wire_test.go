package wire

import (
	"strings"
	"testing"
)

// Bytes the fixtures cannot carry as JSON text: a byte order mark and invalid UTF-8.
func TestRefusesBytesFixturesCannotCarry(t *testing.T) {
	for name, doc := range map[string]string{
		"bom":             "\xef\xbb\xbf{}",
		"invalid utf-8":   "{\"a\":\"\xff\"}",
		"utf-8 surrogate": "{\"a\":\"\xed\xa0\x80\"}",
		"overlong":        "{\"a\":\"\xc0\xaf\"}",
		"nbsp outside":    "{\"a\":\xc2\xa01}",
		"empty":           "",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFieldRules(t *testing.T) {
	n, err := Parse([]byte(`{"s":"ab","i":-0,"f":1.5,"n":null,"o":{"k":"v","z":null},"code":5e2}`))
	if err != nil {
		t.Fatal(err)
	}
	o, err := AsObject(n)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok, err := o.String("s"); s != "ab" || !ok || err != nil {
		t.Errorf("String = %q %v %v", s, ok, err)
	}
	if _, ok, err := o.String("n"); ok || err != nil {
		t.Errorf("null is absence: %v %v", ok, err)
	}
	if _, _, err := o.String("i"); err == nil {
		t.Error("a number is not a string")
	}
	if _, _, err := o.Int("f"); err == nil {
		t.Error("1.5 is not an integer")
	}
	if m, ok, err := o.StringMap("o"); !ok || err != nil || m["k"] != "v" || m["z"] != "" {
		t.Errorf("StringMap = %v %v %v", m, ok, err)
	}
	if c, err := o.Code("code"); c != "5e2" || err != nil {
		t.Errorf("Code = %q %v", c, err)
	}
	if _, err := Parse([]byte(strings.Repeat("[", 10001) + strings.Repeat("]", 10001))); err == nil {
		t.Error("10,001 levels accepted")
	}
}
