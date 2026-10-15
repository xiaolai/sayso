package canon

import (
	"slices"
	"strings"
	"testing"
)

// The strict Han parser: a run that is not a well-formed numeral stays as written; the colloquial dropped unit reads.
func TestStrictHanNumerals(t *testing.T) {
	for in, want := range map[string]string{
		"一百一":  "one hundred ten",
		"两万五":  "twenty five thousand",
		"一百一十": "one hundred ten",
		"一五一十": "一五一十",
		"亿万":   "亿万",
	} {
		if got := strings.Join(Canon(in), " "); got != want {
			t.Errorf("Canon(%q) = %q, want %q", in, got, want)
		}
	}
}

// Titles expand only with their capital: "ms" is milliseconds, "M S" spelled letters.
func TestTitlesNeedTheirCapital(t *testing.T) {
	if Equivalent("5 ms", "five miz") {
		t.Error(`"5 ms" read as "five miz"`)
	}
	if !Equivalent("Ms Smith", "miz smith") || !Equivalent("Dr. Lee", "doctor lee") {
		t.Error("a capitalised title did not expand")
	}
	if !Equivalent("apples vs oranges", "apples versus oranges") {
		t.Error("a case-free abbreviation did not expand")
	}
}

func TestExports(t *testing.T) {
	if v, n := YearPairAt(Canon("nineteen ninety nine"), 0); v != 1999 || n != 3 {
		t.Errorf("YearPairAt = %d, %d", v, n)
	}
	if !slices.Equal(Cardinal(21), []string{"twenty", "one"}) {
		t.Errorf("Cardinal(21) = %v", Cardinal(21))
	}
	if !IsNumberWord("seven") || IsNumberWord("hundred") || !IsScale("hundred") || IsScale("seven") {
		t.Error("IsNumberWord / IsScale")
	}
	if !IsHanNumeral('五') || IsHanNumeral('好') || !IsHyphen('-') || !IsApostrophe('’') || IsApostrophe('a') {
		t.Error("rune predicates")
	}
	if !IsAbbreviation("St") || !IsAbbreviation("mr") || IsAbbreviation("street") {
		t.Error("IsAbbreviation")
	}
}

// U+2212 is a minus sign, not a separator (audit row 23).
func TestUnicodeMinusIsASign(t *testing.T) {
	if Equivalent("−7", "7") {
		t.Error(`"−7" compared equal to "7"`)
	}
	if !Equivalent("−7", "minus seven") || !Equivalent("−7", "-7") {
		t.Error(`"−7" did not read as "minus seven"`)
	}
}

// A domain label ending in a digit keeps its dot (audit row 24); a title before a dot still expands.
func TestDigitDomainKeepsItsDot(t *testing.T) {
	if got := strings.Join(Canon("example2.com"), " "); got != "example two dot com" {
		t.Errorf(`Canon("example2.com") = %q`, got)
	}
	if !Equivalent("example2.com", "example two dot com") {
		t.Error(`"example2.com" did not match its spoken form`)
	}
	if got := strings.Join(Canon("Mr.Smith"), " "); got != "mister smith" {
		t.Errorf(`Canon("Mr.Smith") = %q`, got)
	}
	if got := strings.Join(Canon("Dr's office"), " "); got != "doctors office" {
		t.Errorf(`Canon("Dr's office") = %q`, got)
	}
}

// Canon is idempotent when joining letters makes a number word (audit row 25).
func TestIdempotentAfterJoinedNumberWord(t *testing.T) {
	for _, in := range []string{"T W O oh", "O N E o o", "example2.com", "−7", "T E N point oh", "Mr.Smith", "Dr's office"} {
		once := Canon(in)
		if twice := Canon(strings.Join(once, " ")); !slices.Equal(once, twice) {
			t.Errorf("Canon(%q) = %q, then %q", in, once, twice)
		}
	}
}

// Round-2 audit rows: lowercase domains before numbers and abbreviations, a leading decimal, numbers after joined
// letters, a Han value beyond the cardinal range, and the per-compound and per-side alternatives of Equivalent.
func TestRoundTwoCanonFixes(t *testing.T) {
	for in, want := range map[string]string{
		"example2x.com":         "example two x dot com",
		"x.com":                 "x dot com",
		"etc.com":               "et cetera dot com",
		".5":                    "zero point five",
		"-.5":                   "minus zero point five",
		"one hundred and T W O": "one hundred two",
		"一千万亿":                  strings.Join(Canon("1000000000000000"), " "),
	} {
		if got := strings.Join(Canon(in), " "); got != want {
			t.Errorf("Canon(%q) = %q, want %q", in, got, want)
		}
	}
	for _, p := range [][2]string{
		{"e-mail long-term", "email long term"},
		{"in 2010, 20 people", "in twenty ten, 20 people"},
		{"etc.com", "etc dot com"},
		{"x.com", "x dot com"},
	} {
		if !Equivalent(p[0], p[1]) {
			t.Errorf("Equivalent(%q, %q) = false", p[0], p[1])
		}
	}
	for _, p := range [][2]string{{".5", "5"}, {"-.5", ".5"}, {"3-4", "34"}} {
		if Equivalent(p[0], p[1]) {
			t.Errorf("Equivalent(%q, %q) = true", p[0], p[1])
		}
	}
	for _, in := range []string{"example2x.com", "x.com", "etc.com", ".5", "-.5", "one hundred and T W O", "一千万亿", "Mr.Smith"} {
		once := Canon(in)
		if twice := Canon(strings.Join(once, " ")); !slices.Equal(once, twice) {
			t.Errorf("Canon(%q) = %q, then %q", in, once, twice)
		}
	}
}
