// Package canon is a symmetric canonicaliser for scoring speech-to-text against a script: both sides are reduced
// to comparison tokens and compared. It suits a tolerant score; the said guard (package saidguard) is the strict
// check that a reading said the script. It comes from recite (github.com/xiaolai/recite), whose STT scoring uses it.
package canon

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xiaolai/sayso/go/numwords"
	"github.com/xiaolai/sayso/go/saidguard"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// CanonVersion identifies the canonicaliser's behaviour. Bump it on any change
// to Canon's output or to Equivalent's verdicts: cache keys and verification provenance record it.
const CanonVersion = 16 // 16: Equivalent reports false past MaxBytes or its work budget; 15: it reads every combination of a piece's compounds, through a lattice of its spans

// Canon canonicalises s into comparison tokens : NFKC and width
// folding, abbreviation expansion, lower case, numbers to English words (one
// canonical form for digits and words), apostrophes removed, hyphens to a
// space, every other punctuation or symbol removed, and a run of single Latin letters joined
// into one token ("X J" = "XJ": a spelled-out code). Canon is idempotent:
// Canon(strings.Join(Canon(x), " ")) equals Canon(x).
func Canon(s string) []string { return texts(canon(s)) }

// Equivalent reports whether a and b canonicalise equal, also trying the
// alternatives Canon cannot decide alone, each on its own: every hyphenated
// compound joined or split ("e-mail long-term" = "email long term") and every
// year read as a pair on either side ("nineteen ninety-nine" = "1999", whose
// canonical form is the cardinal). A pair reads as a year where a number starts,
// or after punctuation: "1999; twenty ten" holds two years, "2010, 20" no pair
// "ten twenty". Each side is a graph of its readings, built without listing the
// combinations of its choices (lattice.go), and the two are walked together.
//
// Equivalent takes untrusted text, so it fails closed, reporting false as for texts that differ, on a text longer than
// MaxBytes and on a call that would do more than equivalentWork units of work (see budget).
func Equivalent(a, b string) bool {
	if len(a) > MaxBytes || len(b) > MaxBytes {
		return false
	}
	w := &budget{left: equivalentWork}
	if !w.spend(len(a) + len(b)) {
		return false
	}
	ga, ok := readings(a, w)
	if !ok {
		return false
	}
	gb, ok := readings(b, w)
	return ok && ga.meets(gb, w)
}

// MaxBytes is the longest text Equivalent compares, as saidguard.MaxBytes is the longest the said guard reads.
const MaxBytes = 64 << 10

// equivalentWork is the work one Equivalent call may do, in budget units: 4,194,304. Two 64 KiB texts of prose with a
// compound or a number in every other word need about 350,000, and the costliest real shape measured, runs of
// hyphenated zeros, about 1,300,000 (TestEquivalentWorstCase).
const equivalentWork = 1 << 22

// budget counts the work of one Equivalent call beyond Canon's own passes over the two texts, which are linear in
// their length. A unit is a byte of input; a span, or a rune of a span's text that Canon's text passes read (a word of
// h compound hyphens has (h+1)(h+2)/2 spans, counted before any is built); a graph node, with the letters its state
// holds; a field read along a span; a token emitted (an arc, which shares its token's bytes with the text or with the
// state, already paid for, that built it); or a pair of nodes or of arcs compared in meets. Every loop of readings
// and meets spends before it works, and none does more than a constant amount of work or allocation per unit spent
// (the ways one field can go are at most a constant number, and the walk merges ways by state after every field), so
// the time and memory of one call are linear in MaxBytes and equivalentWork whatever the input.
type budget struct{ left int }

func (b *budget) spend(n int) bool {
	b.left -= n
	return b.left >= 0
}

// tok is a Canon token, and whether punctuation stood before it in the text: Equivalent reads a year pair after a
// number only across punctuation.
type tok struct {
	text string
	brk  bool
}

func texts(toks []tok) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}

// readings is the graph of the token strings s may canonicalise to: s is cut where Canon reads the text on either side
// on its own (pieces), and each piece adds the graph of its readings (pieceGraph).
// It reports false when the budget runs out.
func readings(s string, w *budget) (*graph, bool) {
	g := &graph{next: [][]arc{nil}}
	at := 0
	for _, piece := range pieces(s) {
		var ok bool
		if at, ok = g.pieceGraph(at, piece, w); !ok {
			return nil, false
		}
	}
	g.end = at
	return g, true
}

// graph is an automaton over tokens: a path from node 0 to end spells one reading. An arc with no token is empty.
type graph struct {
	next [][]arc
	end  int
}

type arc struct {
	tok string
	to  int
}

func (g *graph) node() int {
	g.next = append(g.next, nil)
	return len(g.next) - 1
}

// path adds arcs spelling words from node a to node b.
func (g *graph) path(a, b int, words []string) {
	if len(words) == 0 {
		g.next[a] = append(g.next[a], arc{to: b})
		return
	}
	for i, w := range words {
		to := b
		if i < len(words)-1 {
			to = g.node()
		}
		g.next[a] = append(g.next[a], arc{w, to})
		a = to
	}
}

// meets reports whether g and h share a reading: a walk over pairs of nodes, each step one token on both sides or an
// empty arc on one. It reports false when the budget runs out.
func (g *graph) meets(h *graph, w *budget) bool {
	type pair struct{ x, y int }
	seen := map[pair]bool{}
	var todo []pair
	visit := func(p pair) {
		if !seen[p] {
			seen[p] = true
			todo = append(todo, p)
		}
	}
	visit(pair{0, 0})
	for len(todo) > 0 {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if p.x == g.end && p.y == h.end {
			return true
		}
		if !w.spend(1 + len(g.next[p.x])*(1+len(h.next[p.y]))) {
			return false
		}
		for _, e := range g.next[p.x] {
			if e.tok == "" {
				visit(pair{e.to, p.y})
				continue
			}
			for _, f := range h.next[p.y] {
				if f.tok == e.tok {
					visit(pair{e.to, f.to})
				}
			}
		}
		for _, f := range h.next[p.y] {
			if f.tok == "" {
				visit(pair{p.x, f.to})
			}
		}
	}
	return false
}

// pieces cuts s, as Canon reads it, at each space between two words Canon reads as it reads each alone (separable),
// so the pieces canonicalise to s's tokens one after the other and each compound's choice is made in its own piece.
// When they would not, s is one piece.
func pieces(s string) []string {
	rs := []rune(width.Fold.String(norm.NFKC.String(strings.ToValidUTF8(s, " "))))
	out := cuts(rs)
	// Compared without the punctuation marks: a piece's first token has none, and a pair at a piece's start is read
	// either way.
	var whole []string
	for _, p := range out {
		whole = append(whole, Canon(p)...)
	}
	if len(out) > 1 && !slices.Equal(whole, Canon(string(rs))) {
		return []string{string(rs)}
	}
	return out
}

// cuts is rs cut at each space between two separable words.
func cuts(rs []rune) []string {
	var out []string
	start := 0
	for i := 0; i < len(rs); {
		if !unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && unicode.IsSpace(rs[j]) {
			j++
		}
		k, m := i, j // the words before and after this space
		for k > start && !unicode.IsSpace(rs[k-1]) {
			k--
		}
		for m < len(rs) && !unicode.IsSpace(rs[m]) {
			m++
		}
		if k < i && j < m && separable(string(rs[k:i]), string(rs[j:m])) {
			out = append(out, string(rs[start:i]))
			start = j
		}
		i = j
	}
	return append(out, string(rs[start:]))
}

// separable reports whether Canon reads the words left and right, written with a space between them, as it reads
// each alone. Every token rule that looks at a neighbour needs an active token on both sides of the space: a zero
// word ("oh", "o") beside a number word, "point" or a single letter; "and" between a scale and a number word; "a"
// before a scale word; a year pair, two number words; a run of single letters. Abbreviations and titles read a
// neighbour too. So each word is letters, marks, hyphens and apostrophes, after closing punctuation on the left one
// and opening punctuation on the right one; every form the word ends (left) or starts (right) with, split or joined,
// is one Canon token and no abbreviation; when a form on one side is active (edge), every form on the other is inert
// and no single letter; and the two are not both single letters at the space.
func separable(left, right string) bool {
	left = strings.TrimRightFunc(left, func(r rune) bool { return strings.ContainsRune(".,;:!?)]}\"”»…", r) })
	right = strings.TrimLeftFunc(right, func(r rune) bool { return strings.ContainsRune("([{\"“«¿¡", r) })
	lp, rp := strings.FieldsFunc(left, isHyphen), strings.FieldsFunc(right, isHyphen)
	// Not cutting is always sound, so a word of many parts is never cut beside: its forms would cost its parts times its
	// length here, and the lattice reads the larger piece within the budget.
	if len(lp) == 0 || len(rp) == 0 || len(lp)+len(rp) > maxCutParts || !wordShaped(left) || !wordShaped(right) {
		return false
	}
	var lf, rf []string // the forms at the space
	for i := range lp {
		lf = append(lf, strings.Join(lp[i:], ""))
	}
	for i := range rp {
		rf = append(rf, strings.Join(rp[:i+1], ""))
	}
	side := func(forms []string) (ok, active, letter bool) {
		for _, f := range forms {
			k, one := edge(f)
			switch k {
			case edgeNone:
				return false, false, false
			case edgeActive:
				active = true
			}
			letter = letter || one
		}
		return true, active, letter
	}
	lok, lact, llet := side(lf)
	rok, ract, rlet := side(rf)
	switch {
	case !lok || !rok, lact && (ract || rlet), ract && llet:
		return false
	}
	// The forms nearest the space are the last part of left and the first of right.
	_, lone := edge(lf[len(lf)-1])
	_, rone := edge(rf[0])
	return !lone || !rone
}

// maxCutParts bounds the hyphen-separated parts of the two words separable reads.
const maxCutParts = 16

func wordShaped(w string) bool {
	for _, r := range w {
		if !unicode.IsLetter(r) && !unicode.IsMark(r) && !isHyphen(r) && !isApostrophe(r) {
			return false
		}
	}
	return true
}

// How a word at a space can take part in a neighbour rule (separable).
const (
	edgeNone   = iota // not one Canon token, or an abbreviation: never cut beside it
	edgeActive        // a number, scale or zero word, "and", "a" or "point": cut only beside an inert word
	edgeInert         // read the same beside any word
)

// edge is how w takes part in a neighbour rule, and whether it is a single letter.
func edge(w string) (kind int, letter bool) {
	if IsAbbreviation(w) {
		return edgeNone, false
	}
	t := canon(w)
	if len(t) != 1 {
		return edgeNone, false
	}
	x := t[0].text
	letter = len(x) == 1
	switch {
	case isNumberWord(x), isScale(x), x == "oh", x == "o", x == "and", x == "a", x == "point":
		return edgeActive, letter
	}
	return edgeInert, letter
}

func canon(s string) []tok { return tokenPasses(marked(fields(s))) }

// fields is s after the passes that read text, as Canon's fields: tokens, and "\x00" for each punctuation mark. None of
// these passes reads across a space or a hyphen, but "St" reads the capital of the next word: readings relies on it.
func fields(s string) []string {
	// An invalid byte ends up a separator anyway; making it one first keeps the number pass
	// from stalling on it ("\xf7¾" would keep "3 4" as digits, and a second pass read them).
	s = strings.ToValidUTF8(s, " ")
	s = width.Fold.String(norm.NFKC.String(s))
	// NFKC keeps U+2212; it is the same sign as "-" ("−7" is "minus seven").
	s = strings.ReplaceAll(s, "\u2212", "-")
	// One apostrophe, so "Drʼs" is "Dr's" to the abbreviation pass, which reads U+02BC as a letter otherwise.
	s = strings.Map(func(r rune) rune {
		if isApostrophe(r) {
			return '\''
		}
		return r
	}, s)
	// A domain ending in a lowercase label ("example2.com", "Example2.com", "X.com", "ETC.com") is read
	// before the number pass spaces a digit off its dot and before an abbreviation swallows one; a
	// capital on the right waits, so "Mr.Smith" still expands.
	s = lowerDomainDots(s)
	// Numbers first: digits glued to letters ("Mr00") must not hide a word
	// boundary from the abbreviation pass, or a second pass would differ.
	s = chineseNumbers(englishNumbers(s))
	s = expandAbbreviations(s)
	s = domainDots(s)
	s = norm.NFKC.String(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case isApostrophe(r):
		case isHyphen(r), unicode.IsSpace(r):
			b.WriteByte(' ')
		case tokenRune(r):
			b.WriteRune(r)
		default:
			b.WriteString(" \x00 ") // punctuation: a separator, and a mark for the token after it
		}
	}
	// Dropping an apostrophe can bring a letter and a combining mark together ("A'̆"), which a
	// second pass would compose; compose them now.
	return strings.Fields(norm.NFKC.String(b.String()))
}

// tokenPasses are the passes that read tokens. Letters join before abbreviations expand, or a second pass would expand
// what the first joined ("M s" -> "ms" -> "miz"). Joining can make a number word ("T W O" -> "two"), so the number
// rewrites run again after it, or a second pass would read a following "oh" as zero or drop an "and" before it. The
// stages in lattice.go are these passes, one token at a time.
func tokenPasses(toks []tok) []tok {
	numberWords := func(t []tok) []tok { return spokenZeros(scaleArticle(dropNumberAnd(t))) }
	return expandTokens(numberWords(joinLetters(numberWords(toks))))
}

// marked drops the punctuation marks from fields, setting brk on the token after each.
func marked(fields []string) []tok {
	out := make([]tok, 0, len(fields))
	brk := false
	for _, f := range fields {
		if f == "\x00" {
			brk = true
			continue
		}
		out = append(out, tok{f, brk})
		brk = false
	}
	return out
}

// spokenZeros reads "oh" and "o" as zero inside a reading of digits: the spoken form says
// "three seven oh four" for 3704 and "four point oh" for 4.0, and STT writes "O" or "oh". A
// run of them counts when a number word stands right before or after it, or "point" right
// before it, so "oh no" and the letter O stay. It runs before joinLetters, which would join "O O" into "oo".
func spokenZeros(toks []tok) []tok {
	zero := func(t tok) bool { return t.text == "oh" || t.text == "o" }
	out := slices.Clone(toks)
	for i := 0; i < len(out); {
		if !zero(out[i]) {
			i++
			continue
		}
		j := i
		for j < len(out) && zero(out[j]) {
			j++
		}
		// Beside a single letter, O is a letter of a spelled code ("X O 7" = "XO 7"), not a zero.
		letter := func(k int) bool {
			return k >= 0 && k < len(out) && len(out[k].text) == 1 && out[k].text[0] >= 'a' && out[k].text[0] <= 'z'
		}
		if letter(i-1) || letter(j) {
			i = j
			continue
		}
		if i > 0 && (isNumberWord(out[i-1].text) || out[i-1].text == "point") || j < len(out) && isNumberWord(out[j].text) {
			for k := i; k < j; k++ {
				out[k].text = "zero"
			}
		}
		i = j
	}
	return out
}

// joinLetters joins each run of two or more single-letter Latin tokens into one token. A code
// read letter by letter is transcribed spaced ("X J 4471 B") or not ("XJ 4471B"); joining
// removes only that spacing, so different letters still compare different.
func joinLetters(toks []tok) []tok {
	single := func(t tok) bool { return len(t.text) == 1 && t.text[0] >= 'a' && t.text[0] <= 'z' }
	var out []tok
	for i := 0; i < len(toks); {
		j := i
		for j < len(toks) && single(toks[j]) {
			j++
		}
		if j-i >= 2 {
			out = append(out, tok{strings.Join(texts(toks[i:j]), ""), toks[i].brk})
			i = j
			continue
		}
		out = append(out, toks[i])
		i++
	}
	return out
}

// expandTokens expands abbreviations that only became whole tokens after
// punctuation removal ("M's" -> "ms"). Without it a second Canon pass would
// expand them, breaking idempotence. Case is gone here, so "st" is "street".
func expandTokens(toks []tok) []tok {
	var out []tok
	for _, t := range toks {
		// Case is gone here, so a title ("ms", "st", "dr") cannot be told from a unit, a suffix or joined spelled
		// letters; only the case-free abbreviations ("vs", "etc") expand.
		switch exp := abbreviations[t.text]; {
		case exp != "" && !titleAbbreviation[t.text]:
			for i, w := range strings.Fields(exp) {
				out = append(out, tok{w, t.brk && i == 0})
			}
		default:
			out = append(out, t)
		}
	}
	return out
}

func isApostrophe(r rune) bool { return r == '\'' || r == '’' || r == 'ʼ' || r == '‘' }

func isHyphen(r rune) bool {
	return r == '-' || r == '‐' || r == '‑' || r == '‒' || r == '–' || r == '—' || r == '−'
}

// --- domains ---

// domainDot is a dot between two letter or digit runs whose right side is letters, as in
// "example.com": the spoken form reads it "dot", the recogniser writes it. Decimals ("3.5")
// and initials ("U.S.") do not match.
var domainDot = regexp.MustCompile(`([\p{L}\p{N}]{2})\.(\p{L}{2})`)

// lowerDomainDots reads "dot" for a dot between a run of letters or digits (one letter is a label:
// "x.com", "X.com") and a run of two or more lowercase letters. A capital on the right ("Mr.Smith",
// "end.Next") waits for domainDots, after the abbreviations; a run of digits on the right is a decimal.
func lowerDomainDots(s string) string {
	rs := []rune(s)
	run := func(i, step int) (n int, lower bool) {
		lower = true
		for j := i; j >= 0 && j < len(rs) && isWordRune(rs[j]); j += step {
			lower = lower && !unicode.IsUpper(rs[j])
			n++
		}
		return n, lower
	}
	var b strings.Builder
	for i, r := range rs {
		if r == '.' && i > 0 {
			left, _ := run(i-1, -1)
			right, rl := run(i+1, 1)
			letters := right >= 2
			for k := i + 1; k <= i+right && letters; k++ {
				letters = unicode.IsLetter(rs[k])
			}
			if left >= 1 && rl && letters {
				b.WriteString(" dot ")
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

func domainDots(s string) string {
	for {
		t := domainDot.ReplaceAllString(s, "$1 dot $2")
		if t == s {
			return s
		}
		s = t
	}
}

// --- abbreviations ---

var (
	latinAbbrev = regexp.MustCompile(`(?i)\b(e\.g|i\.e)\b\.?`)
	wordAbbrev  = regexp.MustCompile(`(?i)\b(mrs|mr|ms|dr|st|vs|etc|jr|sr)\b\.?`)
)

var titleAbbreviation = map[string]bool{"mrs": true, "mr": true, "ms": true, "dr": true, "st": true, "jr": true, "sr": true}

var abbreviations = map[string]string{
	"mrs": "missus", "mr": "mister", "ms": "miz", "dr": "doctor", "vs": "versus",
	"etc": "et cetera", "jr": "junior", "sr": "senior",
}

// expandAbbreviations runs before lower-casing because "st" needs case:
// "St" followed by a capitalised word is "saint" (St. Louis), otherwise
// "street" (Main St.). "dr" is always "doctor": the Drive reading is rare
// enough in spoken text that a context rule would misfire more than it helps.
func expandAbbreviations(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range latinAbbrev.FindAllStringSubmatchIndex(s, -1) {
		if !wordBounded(s, loc[2], loc[3]) {
			continue
		}
		b.WriteString(s[last:loc[0]])
		if strings.EqualFold(s[loc[2]:loc[2]+1], "e") {
			b.WriteString(" for example ")
		} else {
			b.WriteString(" that is ")
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	s = b.String()
	b.Reset()
	last = 0
	for _, loc := range wordAbbrev.FindAllStringSubmatchIndex(s, -1) {
		if !wordBounded(s, loc[2], loc[3]) {
			continue
		}
		word := strings.ToLower(s[loc[2]:loc[3]])
		if titleAbbreviation[word] && !unicode.IsUpper([]rune(s[loc[2]:loc[3]])[0]) {
			// "ms" is milliseconds and "st" a suffix; only "Ms" and "St" are titles. Expanding them case-blind made
			// "5 ms" = "five miz" and the spelled letters "M S" = "miz". Skipping writes nothing: the text up to
			// the next expansion is copied from last.
			continue
		}
		b.WriteString(s[last:loc[0]])
		exp := abbreviations[word]
		if word == "st" {
			exp = "street"
			if r, _ := utf8.DecodeRuneInString(strings.TrimLeftFunc(s[loc[1]:], unicode.IsSpace)); unicode.IsUpper(r) {
				exp = "saint"
			}
		}
		b.WriteString(exp)
		// "Mr.Smith": the consumed dot was the only boundary. An apostrophe keeps the possessive ("Dr's").
		if r, _ := utf8.DecodeRuneInString(s[loc[1]:]); loc[1] < len(s) && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteByte(' ')
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// --- English numbers ---

// The number words live in internal/numwords, shared with the spoken form.
var (
	cardinal     = numwords.Cardinal
	digitWords   = numwords.Digits
	integerWords = numwords.Integer
	ordinal      = numwords.Ordinal
	smallValue   = numwords.SmallValue
)

const maxCardinal = numwords.MaxCardinal

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordBounded reports whether s[start:end] is a whole word under Unicode: regexp's \b knows
// only ASCII letters, so "ms" in "msð" passes \b, and a second Canon pass would expand it.
func wordBounded(s string, start, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:start])
	after, _ := utf8.DecodeRuneInString(s[end:])
	return !tokenRune(before) && !tokenRune(after)
}

// tokenRune is a rune canon keeps inside a token: a letter, a combining mark or a digit.
func tokenRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) }

// englishNumbers rewrites every ASCII digit run as English words, so no ASCII
// digit survives (which keeps Canon idempotent). It reads thousands
// separators, a sign after a non-word character, decimals, a % suffix and an
// ordinal suffix.
func englishNumbers(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		neg := false
		start := i
		// ".5" is a decimal with its zero unwritten, also after a sign ("-.5"); after a digit or a
		// letter the dot belongs to what precedes it.
		leadDot := func(k int) bool {
			prev, _ := utf8.DecodeLastRuneInString(s[:k])
			return k+1 < len(s) && s[k] == '.' && isASCIIDigit(s[k+1]) && (k == 0 || !isWordRune(prev))
		}
		if c == '-' && i+1 < len(s) && (isASCIIDigit(s[i+1]) || i+2 < len(s) && s[i+1] == '.' && isASCIIDigit(s[i+2])) {
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if i == 0 || !isWordRune(prev) {
				neg, start = true, i+1
			}
		}
		if !neg && !isASCIIDigit(c) && !leadDot(i) {
			b.WriteByte(c)
			i++
			continue
		}
		j := start
		var digits strings.Builder
		for j < len(s) && isASCIIDigit(s[j]) {
			digits.WriteByte(s[j])
			j++
		}
		// Thousands groups: ",ddd" not followed by another digit, after 1-3 digits.
		if digits.Len() <= 3 {
			for j+3 < len(s) && s[j] == ',' && isASCIIDigit(s[j+1]) && isASCIIDigit(s[j+2]) && isASCIIDigit(s[j+3]) &&
				(j+4 >= len(s) || !isASCIIDigit(s[j+4])) {
				digits.WriteString(s[j+1 : j+4])
				j += 4
			}
		}
		words := integerWords(digits.String())
		if digits.Len() == 0 {
			words = cardinal(0) // ".5"
		}
		if j+1 < len(s) && s[j] == '.' && isASCIIDigit(s[j+1]) {
			k := j + 1
			for k < len(s) && isASCIIDigit(s[k]) {
				k++
			}
			words = append(append(words, "point"), digitWords(s[j+1:k])...)
			j = k
		} else if suf := ordinalSuffix(s[j:]); suf > 0 {
			words = ordinal(words)
			j += suf
		}
		if j < len(s) && s[j] == '%' {
			words = append(words, "percent")
			j++
		}
		if neg {
			words = append([]string{"minus"}, words...)
		}
		b.WriteByte(' ')
		b.WriteString(strings.Join(words, " "))
		b.WriteByte(' ')
		i = j
	}
	return b.String()
}

// ordinalSuffix returns the length of an ordinal suffix (st, nd, rd, th) at
// the start of s when no letter follows it, else 0.
func ordinalSuffix(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch strings.ToLower(s[:2]) {
	case "st", "nd", "rd", "th":
	default:
		return 0
	}
	if r, _ := utf8.DecodeRuneInString(s[2:]); len(s) > 2 && unicode.IsLetter(r) {
		return 0
	}
	return 2
}

// scaleArticle reads "a" before a scale word as one ("a hundred" = "one hundred"), the form the
// digits canonicalise to: STT writes "about a hundred and sixty" as "about 160".
func scaleArticle(toks []tok) []tok {
	out := slices.Clone(toks)
	for i := range out {
		if out[i].text == "a" && i+1 < len(out) && isScale(out[i+1].text) {
			out[i].text = "one"
		}
	}
	return out
}

// dropNumberAnd removes "and" inside a number reading ("one hundred and
// five" = "one hundred five"), the form numbers are canonicalised to.
// A dropped "and" passes its punctuation mark to the token after it.
func dropNumberAnd(toks []tok) []tok {
	out := toks[:0:0]
	brk := false
	for i, t := range toks {
		if t.text == "and" && i > 0 && i+1 < len(toks) && isScale(toks[i-1].text) && isNumberWord(toks[i+1].text) {
			brk = brk || t.brk
			continue
		}
		t.brk = t.brk || brk
		brk = false
		out = append(out, t)
	}
	return out
}

func isScale(w string) bool {
	return w == "hundred" || w == "thousand" || w == "million" || w == "billion" || w == "trillion"
}

func isNumberWord(w string) bool { _, ok := smallValue(w); return ok }

// twoDigit parses a number word reading of 0..99 at toks[i:], returning the
// value and the tokens consumed (0 when none).
func twoDigit(toks []string, i int) (int, int) {
	if i >= len(toks) {
		return 0, 0
	}
	v, ok := smallValue(toks[i])
	if !ok {
		return 0, 0
	}
	if v >= 20 && i+1 < len(toks) {
		if u, ok := smallValue(toks[i+1]); ok && u >= 1 && u <= 9 {
			return v + u, 2
		}
	}
	return v, 1
}

// yearPairAt reports the year read as a pair at toks[i] ("nineteen ninety-nine", "nineteen
// hundred", "nineteen oh five") and how many tokens it spans; n is 0 when there is none.
func yearPairAt(toks []string, i int) (v, n int) {
	hi, k := twoDigit(toks, i)
	if k != 1 || hi < 10 || hi > 20 || i+1 >= len(toks) {
		return 0, 0
	}
	j := i + 1
	switch {
	case toks[j] == "hundred":
		return hi * 100, 2
	case toks[j] == "zero" && j+1 < len(toks): // "oh" is zero by now (spokenZeros)
		if lo, ok := smallValue(toks[j+1]); ok && lo >= 1 && lo <= 9 {
			return hi*100 + lo, 3
		}
	default:
		if lo, m := twoDigit(toks, j); m > 0 && lo >= 10 {
			return hi*100 + lo, 1 + m
		}
	}
	return 0, 0
}

// --- Chinese numbers ---

var hanDigits = map[rune]uint64{'〇': 0, '零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

var hanUnits = map[rune]uint64{'十': 10, '百': 100, '千': 1000, '万': 1e4, '亿': 1e8}

func isHanNumeral(r rune) bool {
	_, d := hanDigits[r]
	_, u := hanUnits[r]
	return d || u
}

// chineseNumbers rewrites each run of 汉字 numerals as English words, so
// "四十二" and "42" share one canonical form.
func chineseNumbers(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if !isHanNumeral(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && isHanNumeral(rs[j]) {
			j++
		}
		b.WriteByte(' ')
		b.WriteString(strings.Join(hanRunWords(rs[i:j]), " "))
		b.WriteByte(' ')
		i = j
	}
	return b.String()
}

func hanRunWords(run []rune) []string {
	positional := false
	for _, r := range run {
		if _, ok := hanUnits[r]; ok {
			positional = true
		}
	}
	if !positional {
		// 一九九九: a digit string.
		var ds strings.Builder
		for _, r := range run {
			ds.WriteByte(byte('0' + hanDigits[r]))
		}
		return integerWords(ds.String())
	}
	// A run that is not a well-formed numeral ("一五一十", "亿万", "零十") is not a number and stays as written; the
	// old parser let a later digit overwrite an earlier one and read 一五一十 as 10, and read 一百一 as 101.
	n, ok := saidguard.ParseHanNumeral(string(run))
	if !ok {
		return []string{string(run)}
	}
	// Beyond the cardinal range, the value's digits are read as the digits written in Arabic numerals
	// are, so "一千万亿" and "1000000000000000" share one form.
	return integerWords(strconv.FormatUint(n, 10))
}
