package numwords

import (
	"slices"
	"testing"
)

func TestNumberWords(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "zero"}, {"42", "forty-two"}, {"121", "one hundred twenty-one"},
		{"1000001", "one million one"}, {"007", "zero zero seven"},
		{"9999999999999999", "nine nine nine nine nine nine nine nine nine nine nine nine nine nine nine nine"},
	}
	for _, c := range cases {
		if got := Join(Integer(c.in)); got != c.want {
			t.Errorf("%s: %q", c.in, got)
		}
	}
	if got := Join(Ordinal(Cardinal(21))); got != "twenty-first" {
		t.Errorf("21st: %q", got)
	}
	if got := Ordinal(Cardinal(40)); !slices.Equal(got, []string{"fortieth"}) {
		t.Errorf("40th: %q", got)
	}
	if v, ok := SmallValue("ninety"); !ok || v != 90 {
		t.Errorf("ninety: %d %v", v, ok)
	}
	if _, ok := SmallValue("hundred"); ok {
		t.Error("hundred is not a small value")
	}
}
