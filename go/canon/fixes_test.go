package canon

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"
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
	// The work counter, not a clock, is the measure: each unit costs constant time and memory, and a clock on a
	// loaded machine fails a correct walk. Work linear in the input means nothing was enumerated.
	if eq, work := equivalentWithin(a.String(), b.String()); !eq || work > 8*(a.Len()+b.Len()) {
		t.Errorf("Equivalent = %v with work %d, want true within %d", eq, work, 8*(a.Len()+b.Len()))
	}
	if Equivalent(a.String(), b.String()+" more") {
		t.Error("Equivalent with an added word = true, want false")
	}
}

// Every cut is sound for every reading, not only the written one: for random texts of compounds and the words a
// neighbour rule reads (number, scale and zero words, "and", "a", "point", single letters, abbreviations), each
// combination of compound choices canonicalises, piece by piece, to the whole text's tokens with the same choices.
func TestCutsHoldForEveryReading(t *testing.T) {
	vocab := []string{
		"o-ring", "oh-well", "oh-five", "o-seven", "x-ray", "a-list", "and-or", "point-blank", "one-way", "twenty-one", "nineteen-ninety",
		"e-mail", "well-known", "t-shirt", "o", "oh", "a", "and", "point", "x", "b", "Dr", "St.", "Mr", "hundred",
		"five", "nineteen", "ninety", "zero", "ring", "list", "way", "blank", "seven", "ten", "twenty",
	}
	rng := rand.New(rand.NewPCG(105, 94))
	for range 4000 {
		words := make([]string, 2+rng.IntN(4))
		for i := range words {
			words[i] = vocab[rng.IntN(len(vocab))]
		}
		s := strings.Join(words, " ")
		ps := cuts([]rune(s))
		// The hyphens of each piece, in order, so one mask chooses them all across the pieces and the whole.
		var hy []int
		rs := []rune(s)
		for i := 1; i+1 < len(rs); i++ {
			if isHyphen(rs[i]) && unicode.IsLetter(rs[i-1]) && unicode.IsLetter(rs[i+1]) {
				hy = append(hy, i)
			}
		}
		for m := 0; m < 1<<len(hy); m++ {
			drop := map[int]bool{}
			for k, at := range hy {
				if m&(1<<k) != 0 {
					drop[at] = true
				}
			}
			read := func(from, to int) string { // rs[from:to] with this reading's hyphens dropped
				var b strings.Builder
				for i := from; i < to; i++ {
					if !drop[i] {
						b.WriteRune(rs[i])
					}
				}
				return b.String()
			}
			var got []string
			at := 0 // the pieces are separated by one space
			for _, p := range ps {
				n := len([]rune(p))
				got = append(got, Canon(read(at, at+n))...)
				at += n + 1
			}
			if want := Canon(read(0, len(rs))); !slices.Equal(got, want) {
				t.Fatalf("%q read as %q: pieces %q give %q, want %q", s, read(0, len(rs)), ps, got, want)
			}
		}
	}
}

// Five, eight and thirty o-rings, each written joined or split on its own, are one reading: "o" stands beside
// "ring" or "oring" only, so no zero rule reads across a space.
func TestManyORings(t *testing.T) {
	for _, n := range []int{5, 8, 30} {
		a := strings.TrimSpace(strings.Repeat("o-ring ", n))
		var mixed, wrong []string
		for i := range n {
			if i%3 == 0 {
				mixed = append(mixed, "oring")
			} else {
				mixed = append(mixed, "o ring")
			}
			wrong = append(wrong, "o ring")
		}
		wrong[n-1] = "o rings"
		if !Equivalent(a, strings.Join(mixed, " ")) || !Equivalent(strings.Join(mixed, " "), a) {
			t.Errorf("%d o-rings: mixed joined and split readings refused", n)
		}
		if Equivalent(a, strings.Join(wrong, " ")) {
			t.Errorf("%d o-rings: a different last word accepted", n)
		}
		if Equivalent(a, strings.Join(mixed[1:], " ")) {
			t.Errorf("%d o-rings: a missing compound accepted", n)
		}
	}
	// Active words on both sides of a space still read each other: "oh five" is a zero, "a hundred" one hundred.
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"oh-well five", "zero well five", false},
		{"twenty o-five", "twenty zero five", true},
		{"a-list hundred", "a list hundred", true},
	} {
		if got := Equivalent(c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	long := strings.TrimSpace(strings.Repeat("o-ring a-list oh-well and-or ", 300))
	if eq, work := equivalentWithin(long, strings.ReplaceAll(long, "-", "")); !eq || work > 16*len(long) {
		t.Errorf("1,200 compounds with active edges: equivalent %v with work %d, want true within %d", eq, work, 16*len(long))
	}
}

// Words that read each other ("one twenty") keep a piece whole, and every compound in it is still chosen on its own:
// nine, fifty and two hundred number compounds, thirty "twenty-something", and letters spelling number words, read
// mixed. The lattice grows with the spans, so each stays well inside the bound (the race detector slows it tenfold).
func TestInteractingCompounds(t *testing.T) {
	mixed := func(word string, n int, joined func(i int) bool) (string, string, string) {
		var a, b []string
		for i := range n {
			a = append(a, word)
			if joined(i) {
				b = append(b, strings.ReplaceAll(word, "-", ""))
			} else {
				b = append(b, strings.ReplaceAll(word, "-", " "))
			}
		}
		return strings.Join(a, " "), strings.Join(b, " "), strings.Join(b[1:], " ")
	}
	for _, c := range []struct {
		word string
		n    int
	}{{"twenty-one", 5}, {"twenty-one", 9}, {"twenty-one", 50}, {"twenty-one", 200}, {"twenty-something", 30}, {"o-n-e", 12}, {"t-w-o", 9}, {"o-h", 20}} {
		for _, joined := range []func(int) bool{
			func(i int) bool { return i%2 == 0 }, func(i int) bool { return i%3 == 1 }, func(int) bool { return true },
			func(int) bool { return false },
		} {
			a, b, short := mixed(c.word, c.n, joined)
			if !Equivalent(a, b) || !Equivalent(b, a) {
				t.Errorf("%d x %s: the reading %q refused", c.n, c.word, b)
			}
			if Equivalent(a, short) {
				t.Errorf("%d x %s: a missing compound accepted", c.n, c.word)
			}
		}
	}
	if Equivalent("twenty-one twenty-one", "twentyone twenty") || Equivalent("o-n-e o-n-e", "one one one") {
		t.Error("a reading with a different word accepted")
	}
	// One word of 200 compound hyphens has 20,301 spans, each read once; listing its readings would be 2^200.
	var long, mixedLong strings.Builder
	for i := range 200 {
		long.WriteString("ab-")
		mixedLong.WriteString("ab")
		if i%7 == 0 {
			mixedLong.WriteString(" ")
		}
	}
	long.WriteString("ab")
	mixedLong.WriteString("ab")
	// Each of the 20,301 spans costs a few units; listing the readings would cost 2^200.
	eq, work := equivalentWithin(long.String(), mixedLong.String())
	if !eq || work > 10*20301 || Equivalent(long.String(), mixedLong.String()+"x") {
		t.Errorf("one word of 200 compounds: equivalent %v with work %d (want true within %d), or a longer one accepted", eq, work, 10*20301)
	}
}
