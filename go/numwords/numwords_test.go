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
	if got := Join(Integer("999999999999999")); got != Join(Cardinal(MaxCardinal)) {
		t.Errorf("MaxCardinal: %q", got)
	}
	if got := Integer("1000000000000000"); len(got) != 16 {
		t.Errorf("MaxCardinal+1 is not read digit by digit: %q", got)
	}
}

// Ordinal of no words is no words, never a crash: Integer("") reads as none.
func TestOrdinalOfNothing(t *testing.T) {
	for _, words := range [][]string{nil, {}, Integer("")} {
		if got := Ordinal(words); got != nil {
			t.Errorf("Ordinal(%q) = %q, want nil", words, got)
		}
	}
}

// Digits and Integer read any string without panicking: nil unless it holds only ASCII digits (and, for Integer, is
// not empty); otherwise one word per digit, and a reading.
func FuzzDigits(f *testing.F) {
	for _, s := range []string{"", "0", "007", "1a", "\u0663", "-1", "+1", "12345678901234567890", "9\u0301", "\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w, n := Digits(s), Integer(s)
		if !asciiDigits(s) {
			if w != nil || n != nil {
				t.Fatalf("%q: Digits %q, Integer %q; want nil for a string that is not all digits", s, w, n)
			}
			return
		}
		if len(w) != len(s) || w == nil {
			t.Fatalf("%q: Digits %q, want one word per digit", s, w)
		}
		for i, word := range w {
			if word != ones[s[i]-'0'] {
				t.Fatalf("%q: digit %d read as %q", s, i, word)
			}
		}
		if (s == "") != (n == nil) {
			t.Fatalf("%q: Integer %q", s, n)
		}
	})
}
