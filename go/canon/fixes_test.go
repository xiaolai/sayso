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
