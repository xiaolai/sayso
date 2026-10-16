package canon

import (
	"slices"
	"strings"
	"testing"
	"time"
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

// Audit round 3: domains with a capital or one letter before a lowercase label, "X O" as a code, unified
// apostrophes, any whitespace before a saint's name, normalised compounds, guided compounds beyond the bound, and
// year pairs that never continue a number.
func TestRoundThree(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Example2.com", "example two dot com", true},
		{"ETC.com", "etc dot com", true},
		{"X.com", "x dot com", true},
		{"Mr.Smith", "mister smith", true},
		{"XO 7", "X O 7", true},
		{"four point oh", "4.0", true},
		{"St.\nLouis", "saint louis", true},
		{"Drʼs office", "doctors office", true},
		{"e－mail", "email", true},
		{"café-au-lait", "caféaulait", true},
		{"e-mail long-term well-known part-time full-time", "email long term well known part time full time", true},
		{"e-mail long-term well-known part-time full-time", "email longterm wellknown parttime fulltime", true},
		{"e-mail long-term well-known part-time full-time", "e mail long term well known part time full time", true},
		{"in 2010, 20 people", "in twenty ten, 20 people", true},
		{"2010, 20 people; nineteen ninety nine", "twenty ten, 20 people; 1999", true},
		{"2010, 20 people", "1020 people", false},
	} {
		if got := Equivalent(c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		for _, s := range []string{c.a, c.b} {
			once := Canon(s)
			if twice := Canon(strings.Join(once, " ")); !slices.Equal(once, twice) {
				t.Errorf("Canon(%q) = %q, then %q", s, once, twice)
			}
		}
	}
}

// Each compound and each year pair is decided on its own, however many there are, and a year pair may follow a number
// across punctuation but never continues one.
func TestIndependentChoices(t *testing.T) {
	five := strings.Repeat("e-mail ", 5)
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{five, "email e mail e mail e mail e mail", true},
		{five, "e mail email e mail email e mail", true},
		{five, "email e mail e mail e mail", false},
		{"e-mail long-term well-known part-time full-time x-ray", "email long term wellknown part time fulltime x ray", true},
		{"e-mail long-term well-known part-time full-time x-ray", "email long term wellknown part time fulltime x rays", false},
		{"e-mail, long-term; well-known part-time. Full-time", "e mail longterm well known parttime full time", true},
		{"1999; 2010", "nineteen ninety nine; twenty ten", true},
		{"nineteen ninety nine, twenty ten", "1999 2010", true},
		{"1999 2010", "nineteen ninety nine twenty ten", false}, // no punctuation: "nine twenty ten" continues a number
		{"2010, 20 people", "2000, 1020 people", false},
		{"2010, 20 people; nineteen ninety nine", "twenty ten, 20 people; 1999", true},
		{"nineteen twenty people, twenty ten", "19 20 people, 2010", true},
		{"nineteen ninety-nine", "1999", true},
		{"x-ray a-b o-k e-mail u-turn i-pod", "xray ab ok email uturn ipod", true},
	} {
		if got := Equivalent(c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := Equivalent(c.b, c.a); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

// The cuts canonicalise to the text's tokens one after the other, also beside the words a neighbour rule reads: the
// check in pieces is a safety net, not what makes these hold.
func TestPiecesCompose(t *testing.T) {
	for _, s := range []string{
		"e-mail e-mail e-mail", "St. Louis e-mail", "Main St. e-mail", "x-ray X J", "a hundred e-mail and five",
		"one hundred and e-mail five", "oh e-mail seven", "Mr Smith e-mail", "e.g. e-mail", "nineteen e-mail ninety",
		"café-au-lait au-pair", "’Tis the students’ e-mail", "e-mail, long-term. Well-known!", "二十 e-mail 一",
	} {
		ps := cuts([]rune(s))
		var got []string
		for _, p := range ps {
			got = append(got, Canon(p)...)
		}
		if !slices.Equal(got, Canon(s)) {
			t.Errorf("cuts(%q) = %q: %q, want %q", s, ps, got, Canon(s))
		}
	}
	if ps := cuts([]rune("e-mail e-mail e-mail")); len(ps) != 3 {
		t.Errorf("cuts = %q, want three pieces", ps)
	}
}

// A long text of compounds and years stays linear: listing every combination of 1,000 compounds is 2^1000 readings.
// The bound is loose on purpose (the race detector slows this tenfold); enumeration would not finish.
func TestEquivalentScales(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 500; i++ {
		a.WriteString("e-mail long-term 1999; ")
		if i%2 == 0 {
			b.WriteString("email long term nineteen ninety nine; ")
		} else {
			b.WriteString("e mail longterm one thousand nine hundred ninety nine, ")
		}
	}
	start := time.Now()
	if !Equivalent(a.String(), b.String()) {
		t.Error("Equivalent = false, want true")
	}
	if Equivalent(a.String(), b.String()+" more") {
		t.Error("Equivalent with an added word = true, want false")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}
