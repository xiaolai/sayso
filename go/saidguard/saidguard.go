// Package saidguard answers one question about a text-to-speech take: did the model read exactly this script?
//
// It generates the correct readings of the script (numbers, symbols, abbreviations, spelled codes, Han numerals)
// and matches the model's own transcript against them, one reading per script unit, in order. The transcript is only
// normalised and tokenised, never parsed back into numbers or abbreviations, and it deletes no spoken character.
// The contract is spec/said-guard.md; the cases are fixtures/said-guard.json.
package saidguard

// Three invariants hold by construction rather than by fixture:
//   - A number's every reading carries all of its sign, fraction, ordinal suffix and percent (svNumberUnits).
//   - A Han numeral is a number only if it is one of the spellings of its own value (svParseHan round-trips).
//   - Matching is a bitset simulation over transcript positions, linear in the frontier it actually carries.

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/xiaolai/sayso/go/numwords"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

type svAlt []string // tokens acceptable at one position
type svSeq []svAlt  // one reading
type svUnit []svSeq // the alternative readings of one script unit

// Version is the said guard's revision (spec/said-guard.md), raised whenever a change makes the guard refuse a
// reading it accepted, as Swift's saidGuardVersion is. A cache of audio accepted by the guard keys on it.
const Version = 11

// MaxBytes bounds each side; a longer script or transcript never matches.
const MaxBytes = 64 << 10

// Matches reports whether the model's own transcript (said) is a reading of the script (spoken) in language, a
// BCP 47 tag ("en", "zh-Hans"). An empty script never matches.
func Matches(said, spoken, language string) bool {
	if len(said) > MaxBytes || len(spoken) > MaxBytes || svOverlongMarks(said) || svOverlongMarks(spoken) {
		return false
	}
	units := svScriptUnits(spoken, language)
	if len(units) == 0 {
		return false
	}
	return svMatch(units, svTranscriptTokens(said, language))
}

// --- normalisation shared by both sides ---

// svMaxMarkRun bounds a run of combining marks. Normalisers differ beyond 30 (Go's inserts U+034F to keep the text
// stream-safe, Foundation's does not), so a side with a longer run never matches; a lowercase mapping can add one
// mark (İ → i̇), hence 29.
const svMaxMarkRun = 29

// svOverlongMarks reports a run of more than svMaxMarkRun non-starters (canonical combining class above zero, or
// a Hangul medial or final jamo) in the compatibility decomposition of s, or of s after the deletions of
// preNormalize, which can join two runs.
func svOverlongMarks(s string) bool {
	return svMarkRun(s) || svMarkRun(preNormalize(norm.NFKC.String(svSuperscripts(strings.ToValidUTF8(s, " ")))))
}

func svMarkRun(s string) bool {
	run := 0
	for _, r := range s {
		for _, x := range norm.NFKD.String(string(r)) {
			if norm.NFD.PropertiesString(string(x)).CCC() != 0 || x >= 0x1160 && x <= 0x11FF || x >= 0xD7B0 && x <= 0xD7FF {
				if run++; run > svMaxMarkRun {
					return true
				}
			} else {
				run = 0
			}
		}
	}
	return false
}

func svPre(s string) string {
	s = svSuperscripts(strings.ToValidUTF8(s, " "))
	return width.Fold.String(norm.NFKC.String(preNormalize(norm.NFKC.String(s))))
}

// svSuperscripts writes a superscript digit as "^" and the digit before NFKC would fold it into the number before it
// ("2³" is two cubed, not twenty-three).
func svSuperscripts(s string) string {
	const sup = "⁰¹²³⁴⁵⁶⁷⁸⁹"
	if !strings.ContainsAny(s, sup) {
		return s
	}
	var b strings.Builder
	prev := false
	rs := []rune(s)
	for k, r := range rs {
		// A superscript sign before superscript digits belongs to the exponent ("2⁻³" is 2^-3); left in place it
		// would fold to a minus before the caret and could be dropped.
		if (r == '⁻' || r == '⁺') && !prev && k+1 < len(rs) && strings.ContainsRune(sup, rs[k+1]) {
			b.WriteString(" ^")
			b.WriteRune(map[rune]rune{'⁻': '−', '⁺': '+'}[r])
			prev = true
			continue
		}
		if i := strings.IndexRune(sup, r); i >= 0 {
			if !prev {
				b.WriteString(" ^")
			}
			b.WriteRune('0' + rune(strings.Count(sup[:i], "")-1))
			prev = true
			continue
		}
		prev = false
		b.WriteRune(r)
	}
	return b.String()
}

func svLower(s, language string) string {
	switch baseLanguage(language) {
	case "tr", "az":
		s = strings.ToLowerSpecial(unicode.TurkishCase, s)
	default:
		s = strings.ToLower(s)
	}
	return norm.NFKC.String(s)
}

// --- lexer ---

type svKind int

const (
	svWord svKind = iota
	svNumber
	svUnspacedChar
	svHanRun
	svSymbol
)

type svRaw struct {
	text       string
	kind       svKind
	start, end int    // rune offsets in the normalised text
	gap        string // the text between the previous raw and this one
	// A word whose first or last letter touches a mark svUnquote dropped as a possible quotation mark: the mark may
	// as well be an apostrophe, so the word may also be read with it ("‘the students’ books’").
	optLead, optTrail bool
	dash              svDash // set on a symbol that is a free dash (svFreeDash)
}

// svDash tells a free dash (svFreeDash) from any other raw: a sign ("-x", "-五"), or a dash the script side may read
// with no word outside an expression ("Monday - Friday").
type svDash int

const (
	svDashNone svDash = iota
	svDashSign
	svDashSilentable
)

// punctBefore: something other than space separates this raw from the one before ("Oh, one").
func (r svRaw) punctBefore() bool { return strings.TrimSpace(r.gap) != "" }

// svUnspokenPunct is the closed set of punctuation no one reads aloud; both sides drop it. Every other character
// that is not a letter, digit, mark or space is a spoken symbol and a token on both sides, so a transcript cannot
// gain or lose one. The inverse, a whitelist of spoken symbols, left − × ÷ < ≠ # * ′ deletable.
const svUnspokenPunct = ".,;:!?\"'`()[]{}…–—―/\\|_~·•¡¿、。，！？；：「」『』（）《》〈〉【】‹›«»“”„‘’‚"

func svSpokenSymbol(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r), unicode.IsSpace(r), unicode.IsControl(r), isUnspaced(r):
		return false
	case strings.ContainsRune(svUnspokenPunct, r), svJoiner(r):
		return false
	case unicode.Is(unicode.So, r) && !strings.ContainsRune("°©®™№", r):
		return false // emoji and pictographs are not read aloud
	}
	return true
}

// svBetweenNumbers: a '-' with a number on each side, spaced or not ("5 - 2", "3-4", "5 -2", "5% - 2%", "1st - 3rd",
// "五 - 二"), is spoken (minus, to). A number on the left may end in a percent sign or an ordinal suffix, as the lexer
// reads it; the one on the right may carry a sign ("5 - -2", "5 - +2") or start with a decimal point or comma
// ("5 - .2", "5 - ,2").
func svBetweenNumbers(rs []rune, i int) bool {
	p, n := i-1, i+1
	for p >= 0 && unicode.IsSpace(rs[p]) {
		p--
	}
	for n < len(rs) && unicode.IsSpace(rs[n]) {
		n++
	}
	if p >= 1 && rs[p] == '%' {
		p--
		for p >= 0 && unicode.IsSpace(rs[p]) { // "5 % - 2 %"
			p--
		}
	} else if p >= 2 && svOrdinalSuffixAt(rs, p-1) {
		p -= 2
	}
	if n < len(rs) && (svMinus(rs[n]) && svSignAt(rs, n) || rs[n] == '+' && svOperandAt(rs, n+1)) {
		n++ // a signed operand ("5 - -2", "5 - +2")
	}
	return p >= 0 && (svDigit(rs[p]) || isHanNumeral(rs[p])) && svOperandAt(rs, n)
}

// svOperandAt: a number starts at rs[i]: a digit, a Han numeral, or a decimal point or comma before a digit.
func svOperandAt(rs []rune, i int) bool {
	if i >= len(rs) {
		return false
	}
	return svDigit(rs[i]) || isHanNumeral(rs[i]) || (rs[i] == '.' || rs[i] == ',') && i+1 < len(rs) && svDigit(rs[i+1])
}

// svMinus: the hyphen-minus or the minus sign.
func svMinus(r rune) bool { return r == '-' || r == '−' }

// svOrdinalSuffixAt: rs[i:i+2] is "st", "nd", "rd" or "th" in any case.
func svOrdinalSuffixAt(rs []rune, i int) bool {
	if i < 0 || i+2 > len(rs) {
		return false
	}
	switch strings.ToLower(string(rs[i : i+2])) {
	case "st", "nd", "rd", "th":
		return true
	}
	return false
}

func svDigit(r rune) bool { return r >= '0' && r <= '9' }

func svLetter(r rune) bool { return unicode.IsLetter(r) && !isUnspaced(r) }

// svJoiner: the hyphens that join a word ("e-mail"). Dashes (– —) separate words and are not joiners.
func svJoiner(r rune) bool { return r == '-' || r == '‐' || r == '‑' }

// svSignAt: a '-' or '−' before a digit, or a decimal point or comma before one ("-.5", "-,5"), is a sign at the
// start, after a space or punctuation, or as an exponent sign ("1e-3"); after a letter it is a joiner ("JIRA-10442"),
// and after a number, spaced or not, a minus ("3-4", "5 -2").
func svSignAt(rs []rune, i int) bool {
	if !svMinus(rs[i]) || i+1 >= len(rs) {
		return false
	}
	if !svDigit(rs[i+1]) && !((rs[i+1] == '.' || rs[i+1] == ',') && i+2 < len(rs) && svDigit(rs[i+2])) {
		return false
	}
	if i == 0 {
		return true
	}
	p := rs[i-1]
	if (p == 'e' || p == 'E') && i >= 2 && svDigit(rs[i-2]) {
		return true
	}
	return !svLetter(p) && !svDigit(p) && !unicode.IsMark(p)
}

func svLex(s string, hanNumbers bool) []svRaw {
	rs, dropped := svUnquote([]rune(s))
	var out []svRaw
	prevEnd := 0
	emit := func(kind svKind, i, j int, dash svDash) {
		text := string(rs[i:j])
		switch {
		case kind == svNumber && rs[i] == '−':
			text = "-" + string(rs[i+1:j]) // one sign, however written: "−5" reads as "-5"
		case kind == svSymbol && isApostrophe(rs[i]):
			text = "'" // one apostrophe, however written
		}
		raw := svRaw{text: text, kind: kind, start: i, end: j, gap: string(rs[prevEnd:i]), dash: dash}
		if kind == svWord {
			raw.optLead = i > 0 && dropped[i-1]
			raw.optTrail = j < len(rs) && dropped[j]
		}
		out = append(out, raw)
		prevEnd = j
	}
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case svDigit(r) || svSignAt(rs, i) && !svBetweenNumbers(rs, i) || r == '.' && i+1 < len(rs) && svDigit(rs[i+1]) && (i == 0 || !svDigit(rs[i-1])) ||
			r == ',' && i+1 < len(rs) && svDigit(rs[i+1]) && (i == 0 || unicode.IsSpace(rs[i-1])):
			// A leading "," is a number only after a space (",5" is a French decimal); "Yes,3" is a word and a number.
			j := i + 1
			for j < len(rs) && svDigit(rs[j]) {
				j++
			}
			for j+1 < len(rs) && strings.ContainsRune(".,/:", rs[j]) && svDigit(rs[j+1]) {
				j += 2
				for j < len(rs) && svDigit(rs[j]) {
					j++
				}
			}
			if j+1 < len(rs) && svDigit(rs[j-1]) {
				suf := strings.ToLower(string(rs[j : j+2]))
				if (suf == "st" || suf == "nd" || suf == "rd" || suf == "th") && (j+2 >= len(rs) || !svLetter(rs[j+2])) {
					j += 2
				}
			}
			if j < len(rs) && rs[j] == '%' {
				j++
			}
			emit(svNumber, i, j, svDashNone)
			i = j
		case svSpokenSymbol(r) || svMinus(r) && svBetweenNumbers(rs, i):
			emit(svSymbol, i, i+1, svDashNone)
			i++
		case svFreeDash(rs, i):
			dash := svDashSilentable
			if svFreeSign(rs, i) {
				dash = svDashSign
			}
			emit(svSymbol, i, i+1, dash)
			i++
		case isApostrophe(r) && !(i+1 < len(rs) && svLetter(rs[i+1])) && (prevEnd == i && svAfterToken(out, 1) || i+1 < len(rs) && isUnspaced(rs[i+1])):
			// A mark right after a number or an unspaced character that svUnquote kept, or after one such mark, is
			// no quotation mark: feet or minutes ("5'", "5’10", "21st’"), with the second inches or seconds ("5''"),
			// or a mark of unspaced text on either side ("五’", "好’", "’好"), kept as a word keeps its own ("’tis").
			// A spoken symbol on both sides, so a transcript cannot drop it.
			emit(svSymbol, i, i+1, svDashNone)
			i++
		case r == '"' && !dropped[i] && prevEnd == i && svFeetAndNumber(out):
			// The inches after feet ("5'10\""): a symbol on both sides. Any other '"' is a quotation mark, unspoken.
			emit(svSymbol, i, i+1, svDashNone)
			i++
		case hanNumbers && isHanNumeral(r) && !(i+1 < len(rs) && unicode.IsMark(rs[i+1])):
			// A numeral carrying a mark is a character like any other ("一́"), as the transcript side reads it.
			j := i
			for j < len(rs) && isHanNumeral(rs[j]) && !(j+1 < len(rs) && unicode.IsMark(rs[j+1])) {
				j++
			}
			emit(svHanRun, i, j, svDashNone)
			i = j
		case isUnspaced(r):
			j := i + 1
			for j < len(rs) && unicode.IsMark(rs[j]) {
				j++
			}
			emit(svUnspacedChar, i, j, svDashNone)
			i = j
		case svLetter(r) || isApostrophe(r) && i+1 < len(rs) && svLetter(rs[i+1]):
			// "e.g." and "i.e." are one word, also at a sentence start ("E.g."). With a capital second letter they are
			// initials ("E.G. White"), lexed letter by letter like any other.
			if m := strings.ToLower(string(rs[i:min(i+4, len(rs))])); (strings.HasPrefix(m, "e.g") || strings.HasPrefix(m, "i.e")) && !unicode.IsUpper(rs[i+2]) && (i == 0 || !svLetter(rs[i-1]) && rs[i-1] != '.') && (i+3 >= len(rs) || !svLetter(rs[i+3])) && !(i+4 < len(rs) && rs[i+3] == '.' && svLetter(rs[i+4])) {
				// Not inside a domain ("www.e.g.com"), where its letters are labels.
				emit(svWord, i, i+3, svDashNone)
				i += 3
				if i < len(rs) && rs[i] == '.' {
					i++
					out[len(out)-1].optTrail = i < len(rs) && dropped[i] // the dot is the abbreviation's: "‘e.g.’"
				}
				continue
			}
			j := i
			for j < len(rs) {
				c := rs[j]
				switch {
				case svLetter(c) || unicode.IsMark(c):
					j++
					continue
				case isApostrophe(c) && (j > i || j+1 < len(rs) && svLetter(rs[j+1])):
					j++
					continue
				case svJoiner(c) && j > i && j+1 < len(rs) && svLetter(rs[j+1]) && (svLetter(rs[j-1]) || unicode.IsMark(rs[j-1])):
					j++
					continue
				}
				break
			}
			emit(svWord, i, j, svDashNone)
			i = j
		default:
			i++
		}
	}
	return out
}

// svUnquote finds the single marks that may be quotation marks around words ("Say ‘hello’.", "'yes'") and replaces
// each with an unspoken '"', reporting which. It does not decide which mark closes which: in "‘the students’ books’"
// the middle mark may be a possessive or a closing quotation mark. A dropped mark leaves its word flagged (svRaw
// optLead, optTrail), and each side then reads the word with and without the mark, so the transcript selects. An
// opener stands at a word start after a space, an opening bracket or the start, and never before a leading elision
// (svElisions: "’Tis the students’ books" holds two apostrophes); a closer at a word end, after any punctuation that
// ends the quoted words ("‘hello!’", "‘yes,’"), before a space, punctuation or the end. Within one sentence, which ends
// at its final punctuation (svSentenceEndAt) or, where closers follow that, right after them ("‘Wait.’" is one
// quotation, and "‘Wait!’ The students’ books." two sentences), an opener with a closer after it, and a closer with an
// opener before it, may be quotation marks; every other mark is an apostrophe ("’tis", "the students’ books", "rock
// ’n’ roll"). The candidates of a sentence are kept in a list, and the next scalar that is no inner punctuation is
// found in one reverse pass, so the work is linear.
func svUnquote(rs []rune) ([]rune, []bool) {
	dropped := make([]bool, len(rs))
	var openers, closers []int
	flush := func() {
		if len(openers) > 0 && len(closers) > 0 {
			first, last := openers[0], closers[len(closers)-1]
			for _, o := range openers {
				dropped[o] = dropped[o] || o < last
			}
			for _, c := range closers {
				dropped[c] = dropped[c] || c > first
			}
		}
		openers, closers = openers[:0], closers[:0]
	}
	// next[i]: the first index from i on that holds no svInnerPunct, len(rs) at the end.
	next := make([]int, len(rs)+1)
	next[len(rs)] = len(rs)
	for i := len(rs) - 1; i >= 0; i-- {
		next[i] = i
		if svInnerPunct(rs[i]) {
			next[i] = next[i+1]
		}
	}
	// markFrom is the index of the first scalar from i on that is no inner punctuation, if it is an apostrophe.
	markFrom := func(i int) int {
		if next[i] < len(rs) && isApostrophe(rs[next[i]]) {
			return next[i]
		}
		return -1
	}
	// pending: the mark after a sentence's final punctuation. The sentence ends after it, or before it when it closes
	// nothing.
	pending := -1
	for i, r := range rs {
		if !isApostrophe(r) {
			if svSentenceEndAt(rs, i) {
				if pending = markFrom(i + 1); pending < 0 {
					flush()
				}
			}
			continue
		}
		k := i - 1
		for k >= 0 && svInnerPunct(rs[k]) {
			k--
		}
		// An unspaced character is a word of its own, so it is a word boundary on either side, as a space is.
		closer := k >= 0 && (svLetter(rs[k]) || unicode.IsMark(rs[k]) || svDigit(rs[k]) || isUnspaced(rs[k])) &&
			(i+1 == len(rs) || unicode.IsSpace(rs[i+1]) || isUnspaced(rs[i+1]) || strings.ContainsRune(svUnspokenPunct, rs[i+1]) && !isApostrophe(rs[i+1]))
		opener := (i == 0 || unicode.IsSpace(rs[i-1]) || isUnspaced(rs[i-1]) || strings.ContainsRune("([{\"“«", rs[i-1])) && i+1 < len(rs) &&
			(svLetter(rs[i+1]) || svDigit(rs[i+1]) || isUnspaced(rs[i+1])) && !svElisionAt(rs, i+1)
		// Between two unspaced characters a mark may open or close ("说‘好’"): it is a candidate for both.
		if pending == i {
			pending = -1
			if closer {
				closers = append(closers, i)
				if opener {
					openers = append(openers, i)
				}
				if pending = markFrom(i + 1); pending < 0 {
					flush()
				}
				continue
			}
			flush()
		}
		if closer {
			closers = append(closers, i)
		}
		if opener {
			openers = append(openers, i)
		}
	}
	flush()
	for i, d := range dropped {
		if d {
			rs[i] = '"'
		}
	}
	return rs, dropped
}

// svInnerPunct reports unspoken punctuation that may stand between quoted words and their closing mark ("hello!’"):
// anything unspoken but a space or another apostrophe.
func svInnerPunct(r rune) bool {
	return strings.ContainsRune(svUnspokenPunct, r) && !isApostrophe(r)
}

// svAfterToken reports whether the raws so far end in a number or an unspaced character (a Han run on the script
// side, a character on the transcript side), or in one and up to marks measurement marks (svMeasureMark), all
// touching.
func svAfterToken(out []svRaw, marks int) bool {
	k, seen := len(out)-1, 0
	for k >= 0 && seen < marks && out[k].kind == svSymbol && out[k].text == "'" && out[k].gap == "" {
		k--
		seen++
	}
	return k >= 0 && (out[k].kind == svNumber || out[k].kind == svHanRun || out[k].kind == svUnspacedChar)
}

// svFeetAndNumber reports whether the raws so far are a number, a feet mark and a number, all touching ("5'10",
// "5′10").
func svFeetAndNumber(out []svRaw) bool {
	n := len(out)
	return n >= 3 && out[n-1].kind == svNumber && out[n-1].gap == "" && out[n-2].kind == svSymbol &&
		svMeasureMark(out[n-2].text) && out[n-2].gap == "" && out[n-3].kind == svNumber
}

// svMeasureMark: a mark that measures after a number, the apostrophe the lexer keeps there or the prime.
func svMeasureMark(s string) bool { return s == "'" || s == "′" }

// svFreeDash: a '-' the lexer read as no sign, no minus between numbers and no joiner inside a word, unless it is
// glued between a letter or digit and a letter or digit ("JIRA-10442"), which stays silent: a dash between words
// ("Monday - Friday", "x - y = z"), a suspended hyphen ("pre- and post-war") or a sign before a word or Han numeral
// ("-x", "-五"). It is a spoken symbol on both sides, so a transcript cannot drop the one an expression or a sign
// needs.
func svFreeDash(rs []rune, i int) bool {
	if rs[i] != '-' {
		return false
	}
	glued := i > 0 && i+1 < len(rs) && (svLetter(rs[i-1]) || svDigit(rs[i-1]) || unicode.IsMark(rs[i-1])) &&
		(svLetter(rs[i+1]) || svDigit(rs[i+1]))
	return !glued
}

// svFreeSign: a free dash that is a sign, read only as written or "minus", never silent. Before a number, a Han numeral
// as a digit, it is one unless a letter, digit or mark comes before it, as svSignAt has it ("-五", "气温-五度"). Before
// a word or any other unspaced character it is one only at a word start, after a space, punctuation or the start
// ("-x"): an unspaced character is a word of its own, so a dash after one joins two words ("北京-上海"), as a hyphen
// joins "x-y".
func svFreeSign(rs []rune, i int) bool {
	if i+1 >= len(rs) {
		return false
	}
	n := rs[i+1]
	afterWord := i > 0 && (svLetter(rs[i-1]) || svDigit(rs[i-1]) || unicode.IsMark(rs[i-1]))
	if svDigit(n) || isHanNumeral(n) {
		return !afterWord
	}
	return !afterWord && !(i > 0 && isUnspaced(rs[i-1])) && (svLetter(n) || isUnspaced(n))
}

var svDotAbbreviations = map[string]bool{"mr": true, "mrs": true, "ms": true, "dr": true, "st": true, "jr": true, "sr": true, "vs": true}

// svSentenceEndAt reports whether rs[i] ends a sentence: "!", "?" and "。" always, and "." unless a letter or digit
// follows it ("e.g", "3.5", "x.com") or it closes an abbreviation that is mostly followed by more of the sentence
// (Mr, Mrs, Ms, Dr, St, Jr, Sr, vs, e.g., i.e.; "etc." mostly ends one). It looks at most four letters back, so the
// work is linear.
func svSentenceEndAt(rs []rune, i int) bool {
	r := rs[i]
	if !strings.ContainsRune(svSentenceEnds, r) {
		return false
	}
	if r != '.' {
		return true
	}
	if i+1 < len(rs) && (svLetter(rs[i+1]) || svDigit(rs[i+1])) {
		return false
	}
	b := i
	for b > 0 && i-b < 4 && svLetter(rs[b-1]) {
		b--
	}
	if b == i || b > 0 && (svLetter(rs[b-1]) || svDigit(rs[b-1])) { // "1st.", a longer word
		return true
	}
	w := svASCIILower(rs[b:i])
	if svDotAbbreviations[w] && (b == 0 || rs[b-1] != '.') {
		return false
	}
	// The dot after "e.g" and "i.e", not inside a domain ("x.e.g.").
	if len(w) == 1 && b >= 2 && rs[b-1] == '.' && svLetter(rs[b-2]) {
		pair := svASCIILower(rs[b-2:b-1]) + "." + w
		return !((pair == "e.g" || pair == "i.e") && (b == 2 || !svLetter(rs[b-3]) && rs[b-3] != '.'))
	}
	return true
}

// svASCIILower is rs with its ASCII letters in lower case; any other rune is kept as it is.
func svASCIILower(rs []rune) string {
	out := make([]rune, len(rs))
	for i, r := range rs {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out[i] = r
	}
	return string(out)
}

// svElisions are the English words written with a leading apostrophe for dropped letters ("’tis", "’em", "’n’"). A
// mark before one is an apostrophe, never an opening quotation mark: a closed list, as the guard's other word lists.
var svElisions = map[string]bool{
	"tis": true, "twas": true, "twere": true, "twill": true, "twould": true, "em": true, "n": true, "cause": true,
	"cos": true, "til": true, "bout": true, "cept": true, "neath": true, "gainst": true, "tween": true, "twixt": true,
	"scuse": true, "nuff": true,
}

// svElisionAt reports whether the letters starting at rs[i], in any case, are a whole word of svElisions: not a
// word's prefix before a mark, or before a joiner or an apostrophe and a letter ("n-gram", "n'est").
func svElisionAt(rs []rune, i int) bool {
	var b []byte
	for ; i < len(rs) && svLetter(rs[i]); i++ {
		r := rs[i]
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r < 'a' || r > 'z' || len(b) == 6 {
			return false
		}
		b = append(b, byte(r))
	}
	if i < len(rs) && (unicode.IsMark(rs[i]) || (svJoiner(rs[i]) || isApostrophe(rs[i])) && i+1 < len(rs) && svLetter(rs[i+1])) {
		return false
	}
	return svElisions[string(b)]
}

func svNormApostrophes(s string) string {
	return strings.Map(func(r rune) rune {
		if isApostrophe(r) {
			return '\''
		}
		return r
	}, s)
}

// --- transcript side: normalisation only ---

// svTok is a transcript token. link marks a token joined to the next by a hyphen ("e-mail"): the matcher lets a
// script word equal to the joined letters consume the whole chain. lead and trail mark a word whose dropped mark
// (svUnquote) may have been an apostrophe: the token may also be read with "'" before or after it.
type svTok struct {
	text        string
	link        bool
	lead, trail bool
}

func svTranscriptTokens(s, language string) []svTok {
	var out []svTok
	for _, t := range svLex(svPre(s), false) {
		text := svNormApostrophes(svLower(t.text, language))
		switch {
		case t.kind == svWord:
			parts := strings.FieldsFunc(text, svJoiner)
			for i, part := range parts {
				out = append(out, svTok{text: part, link: i < len(parts)-1, lead: i == 0 && t.optLead, trail: i == len(parts)-1 && t.optTrail})
			}
		case t.kind == svNumber:
			body, percent := strings.CutSuffix(text, "%")
			if svCommaList(body, language) {
				// "1,2,3" is three numbers, cut as the script side cuts it; commas are not spoken
				for _, p := range strings.Split(body, ",") {
					out = append(out, svTok{text: p})
				}
			} else {
				out = append(out, svTok{text: body})
			}
			if percent {
				out = append(out, svTok{text: "%"})
			}
		default:
			out = append(out, svTok{text: text})
		}
	}
	return out
}

// --- script side: units and their readings ---

func lit(tokens ...string) svSeq {
	seq := make(svSeq, len(tokens))
	for i, t := range tokens {
		seq[i] = svAlt{t}
	}
	return seq
}

func cat(seqs ...svSeq) svSeq {
	var out svSeq
	for _, s := range seqs {
		out = append(out, s...)
	}
	return out
}

var svZeroAlt = svAlt{"zero", "oh", "o"}

// svWordsWithZero turns a reading of digits into a sequence where every "zero" also accepts "oh" and "o".
func svWordsWithZero(words []string) svSeq {
	seq := make(svSeq, len(words))
	for i, w := range words {
		if w == "zero" {
			seq[i] = svZeroAlt
		} else {
			seq[i] = svAlt{w}
		}
	}
	return seq
}

func svAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !svDigit(rune(s[i])) {
			return false
		}
	}
	return true
}

var svScales = map[string]uint64{"thousand": 1e3, "million": 1e6, "billion": 1e9, "trillion": 1e12}

func svIsScaleWord(w string) bool { _, ok := svScales[w]; return ok || w == "hundred" }

// svWithAnd inserts the British "and" before the final part under a hundred ("one hundred and five").
func svWithAnd(words []string) []string {
	last := -1
	for i, w := range words {
		if svIsScaleWord(w) {
			last = i
		}
	}
	if last < 0 || last == len(words)-1 {
		return nil
	}
	return append(append(append([]string{}, words[:last+1]...), "and"), words[last+1:]...)
}

// svWithAllAnds is the British reading with an "and" in every group: "one million two hundred and thirty four
// thousand five hundred and sixty seven", "a million and one".
func svWithAllAnds(words []string) []string {
	var out []string
	for i, w := range words {
		out = append(out, w)
		if w == "hundred" && i+1 < len(words) && !svIsScaleWord(words[i+1]) {
			out = append(out, "and")
		}
	}
	last := -1
	for i, w := range words {
		if svIsScaleWord(w) {
			last = i
		}
	}
	if last >= 0 && words[last] != "hundred" && last < len(words)-1 {
		at := len(out) - (len(words) - 1 - last) // the tail under a hundred follows the last scale word
		out = append(append(append([]string{}, out[:at]...), "and"), out[at:]...)
	}
	return out
}

func svCommaDigits(n uint64) string { return svGroupDigits(strconv.FormatUint(n, 10)) }

// svGroupDigits writes a string of digits with thousands commas ("1000000000000000" → "1,000,000,000,000,000").
func svGroupDigits(s string) string {
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// svCardinalForms: the word readings of n, with no "and", one, or one per group, and with "a" for a leading one
// before a scale word ("a hundred and sixty").
func svCardinalForms(n uint64) [][]string {
	words := numwords.Cardinal(n)
	forms := [][]string{words}
	seen := map[string]bool{strings.Join(words, " "): true}
	add := func(f []string) {
		if f != nil && !seen[strings.Join(f, " ")] {
			seen[strings.Join(f, " ")] = true
			forms = append(forms, f)
		}
	}
	add(svWithAnd(words))
	add(svWithAllAnds(words))
	for _, f := range append([][]string{}, forms...) {
		if len(f) > 1 && f[0] == "one" && svIsScaleWord(f[1]) {
			add(append([]string{"a"}, f[1:]...))
		}
	}
	return forms
}

func svYearReadings(n uint64) []svSeq {
	if n < 1000 || n > 2099 {
		return nil
	}
	hi, lo := n/100, n%100
	switch {
	case lo == 0 && hi%10 != 0:
		return []svSeq{lit(append(numwords.Cardinal(hi), "hundred")...)}
	case lo == 0:
		return nil
	case lo < 10:
		return []svSeq{cat(lit(numwords.Cardinal(hi)...), svSeq{svZeroAlt}, lit(numwords.Cardinal(lo)...))}
	default:
		return []svSeq{lit(append(numwords.Cardinal(hi), numwords.Cardinal(lo)...)...)}
	}
}

func svOrdinalSuffix(n uint64) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return "th"
	case n%10 == 1:
		return "st"
	case n%10 == 2:
		return "nd"
	case n%10 == 3:
		return "rd"
	}
	return "th"
}

// svCommaList: a comma-separated list of plain numbers in a language whose comma groups digits ("1,2,3").
func svCommaList(text, language string) bool {
	if b := baseLanguage(language); b != "en" && b != "zh" || !strings.Contains(text, ",") || svIsGrouped(text) {
		return false
	}
	for _, p := range strings.Split(text, ",") {
		if p == "" || !svAllDigits(p) {
			return false
		}
	}
	return true
}

// svIsGrouped: "1,000" and "12,345,678", digits only, groups of three after a first group of one to three.
func svIsGrouped(s string) bool {
	parts := strings.Split(s, ",")
	if len(parts) < 2 || len(parts[0]) < 1 || len(parts[0]) > 3 || parts[0][0] == '0' {
		return false
	}
	for i, p := range parts {
		if i > 0 && len(p) != 3 || !svAllDigits(p) {
			return false
		}
	}
	return true
}

// svNumberUnits lists the readings of a number written in the script. Every reading of it says its whole value:
// sign, magnitude, fraction, ordinal suffix and percent, so no alternative can drop one of them. A comma list
// ("1,2,3") is several numbers.
func svNumberUnits(text, language string) []svUnit {
	low := svLower(text, language)
	base := baseLanguage(language)
	body, sign := low, ""
	if strings.HasPrefix(body, "-") {
		body, sign = body[1:], "-"
	}
	percent := strings.HasSuffix(body, "%")
	body = strings.TrimSuffix(body, "%")
	suffix := ""
	if n := len(body); n > 2 && svDigit(rune(body[n-3])) {
		if s := body[n-2:]; s == "st" || s == "nd" || s == "rd" || s == "th" {
			body, suffix = body[:n-2], s
		}
	}
	percentAlt := svAlt{"%"}
	if base == "en" {
		percentAlt = svAlt{"%", "percent"}
	}
	finish := func(u svUnit) []svUnit {
		if percent {
			for i := range u {
				u[i] = cat(u[i], svSeq{percentAlt})
			}
		}
		return []svUnit{u}
	}
	literal := func() []svUnit { return finish(svUnit{lit(sign + body + suffix)}) }
	if base != "en" && base != "zh" || strings.ContainsAny(body, "/:") {
		return literal() // digits match digits; other languages' separators and spell-out are Swift's, via ICU
	}
	intPart, frac := body, ""
	if i := strings.LastIndexByte(body, '.'); i >= 0 {
		intPart, frac = body[:i], body[i+1:]
		if frac == "" || !svAllDigits(frac) {
			return literal()
		}
	}
	grouped := false
	if strings.Contains(intPart, ",") {
		switch {
		case svIsGrouped(intPart):
			intPart, grouped = strings.ReplaceAll(intPart, ",", ""), true
		case frac == "" && sign == "" && suffix == "":
			// A list ("1,2,3"; "1,2,3%" ends in a percentage, as the transcript side cuts it).
			parts := strings.Split(intPart, ",")
			var out []svUnit
			for i, p := range parts {
				if p == "" || !svAllDigits(p) {
					return literal()
				}
				if i == len(parts)-1 && percent {
					p += "%"
				}
				out = append(out, svNumberUnits(p, language)...)
			}
			return out
		default:
			return literal()
		}
	}
	if !svAllDigits(intPart) || intPart == "" && (frac == "" || suffix != "") || frac != "" && suffix != "" {
		return literal()
	}
	fracText := ""
	if frac != "" {
		fracText = "." + frac
	}
	leadingZero := len(intPart) > 1 && intPart[0] == '0'
	n, err := strconv.ParseUint(intPart, 10, 64)
	cardinal := intPart != "" && !leadingZero && err == nil && n <= numwords.MaxCardinal
	if suffix != "" && (!cardinal || svOrdinalSuffix(n) != suffix) {
		return literal() // "2st", "007th": written as given, no spoken alternative
	}

	unit := svUnit{lit(sign + body + suffix)} // as written ("1,000,000,000,000,000")
	digitForms := []string{intPart + fracText}
	if intPart == "" {
		digitForms = append(digitForms, "0"+fracText)
	} else if !leadingZero && len(intPart) >= 4 {
		digitForms = append(digitForms, svGroupDigits(intPart)+fracText) // also beyond the cardinal range
	}
	for _, d := range digitForms {
		unit = append(unit, lit(sign+d+suffix))
		if sign != "" && base == "en" {
			unit = append(unit, lit("minus", d+suffix), lit("negative", d+suffix))
		}
	}
	if base == "zh" {
		// The Han spellings reach further than the English cardinals: up to svHanMax (一千万亿 and beyond).
		if intPart != "" && !leadingZero && err == nil && n <= svHanMax && sign == "" && suffix == "" && frac == "" {
			unit = append(unit, svHanSeqs(n)...)
		}
		return finish(unit)
	}

	var words []svSeq
	fracWords := svWordsWithZero(numwords.Digits(frac))
	switch {
	case intPart == "": // ".5"
		words = append(words, cat(lit("point"), fracWords), cat(svSeq{svZeroAlt}, lit("point"), fracWords))
	case !cardinal && grouped: // beyond the cardinal range a grouped number is as written, never digit by digit
	case !cardinal: // "007", "01.5", more than fifteen digits: digit by digit
		w := svWordsWithZero(numwords.Digits(intPart))
		if frac != "" {
			w = cat(w, lit("point"), fracWords)
		}
		words = append(words, w)
	case frac != "":
		for _, f := range svCardinalForms(n) {
			words = append(words, cat(lit(f...), lit("point"), fracWords))
		}
		if len(intPart) >= 4 && !grouped {
			words = append(words, cat(svWordsWithZero(numwords.Digits(intPart)), lit("point"), fracWords)) // "1234.5"
		}
	case suffix != "":
		for _, f := range svCardinalForms(n) {
			words = append(words, lit(numwords.Ordinal(f)...))
		}
	default:
		for _, f := range svCardinalForms(n) {
			words = append(words, lit(f...))
		}
		if len(intPart) == 4 && !grouped {
			words = append(words, svYearReadings(n)...)
		}
		if len(intPart) >= 4 && !grouped {
			words = append(words, svWordsWithZero(numwords.Digits(intPart))) // a code or phone number
		}
	}
	for _, w := range words {
		if sign != "" {
			unit = append(unit, cat(lit("minus"), w), cat(lit("negative"), w))
		} else {
			unit = append(unit, w)
		}
	}
	return finish(unit)
}

func svSplitRunes(s string) []string {
	var out []string
	for _, r := range s {
		out = append(out, string(r))
	}
	return out
}

// --- Han numerals ---

var svHanDigit = map[rune]uint64{'〇': 0, '零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
var svHanSmallUnit = map[rune]uint64{'十': 10, '百': 100, '千': 1000}

// svParseHan reads a Han numeral strictly: it computes a value, then accepts the run only if the run is one of the
// spellings of that value (svHanSeqs: standard, 两 before a unit, 一十, 〇 for 零, and the colloquial dropped final
// unit 一百一 = 110, 两万五 = 25000). Repeated or out-of-order units, a leading 零, a 零 that skips no place, and
// anything that overflows fail the round trip by construction.
func svParseHan(run []rune) (uint64, bool) {
	n, ok := svHanValue(run)
	if !ok {
		return 0, false
	}
	for _, seq := range svHanSeqs(n) {
		if len(seq) != len(run) {
			continue
		}
		match := true
		for i, alt := range seq {
			if !svAltHas(alt, string(run[i])) {
				match = false
				break
			}
		}
		if match {
			return n, true
		}
	}
	return 0, false
}

// svHanValue computes the value a well-formed run would have. It does not validate; svParseHan does.
func svHanValue(run []rune) (uint64, bool) {
	var total, section uint64
	pending := -1
	zero, any, zeroInSection := false, false, false
	lastUnit := uint64(0)
	for i, r := range run {
		if d, ok := svHanDigit[r]; ok {
			if d == 0 {
				zero = true
				continue
			}
			if pending >= 0 {
				return 0, false
			}
			if zero {
				zero, zeroInSection = false, true
			}
			pending, any = int(d), true
			continue
		}
		if u, ok := svHanSmallUnit[r]; ok {
			switch {
			case pending >= 0:
				section += uint64(pending) * u
			case u == 10 && i == 0:
				section += 10
			default:
				return 0, false
			}
			// A zero before this unit skipped a higher place (一万零一百五); only a zero after the last unit makes the
			// final digit a units digit (一百零五).
			pending, lastUnit, any, zeroInSection = -1, u, true, false
			continue
		}
		group := section
		if pending >= 0 {
			group += uint64(pending)
		}
		switch r {
		case '万':
			if group == 0 || group >= 1e4 || total >= 1e17 {
				return 0, false
			}
			total += group * 1e4
			lastUnit = 1e4
		case '亿':
			if total+group == 0 || total+group >= 1e10 {
				return 0, false
			}
			total = (total + group) * 1e8
			lastUnit = 1e8
		default:
			return 0, false
		}
		section, pending, zeroInSection = 0, -1, false
	}
	if pending >= 0 {
		if lastUnit >= 100 && section%10 == 0 && !zeroInSection { // 一百一 = 110, 两万五 = 25000, but 一百零一 = 101
			section += uint64(pending) * (lastUnit / 10)
		} else {
			section += uint64(pending)
		}
	}
	return total + section, any
}

var svHanNumerals = []rune{'零', '一', '二', '三', '四', '五', '六', '七', '八', '九'}

// svHanMax is the largest value svRenderHan writes: its 亿 group stays below 一万万.
const svHanMax = 1e16 - 1

func svRenderSection(n uint64, leadingTen bool) string {
	var b strings.Builder
	units := []struct {
		v uint64
		s string
	}{{1000, "千"}, {100, "百"}, {10, "十"}, {1, ""}}
	started, zeroPending := false, false
	for _, u := range units {
		d := n / u.v % 10
		if d == 0 {
			if started {
				zeroPending = true
			}
			continue
		}
		if zeroPending {
			b.WriteRune('零')
			zeroPending = false
		}
		if !(u.v == 10 && d == 1 && !started && leadingTen) {
			b.WriteRune(svHanNumerals[d])
		}
		b.WriteString(u.s)
		started = true
	}
	return b.String()
}

// svRenderHan writes n in standard Han numerals (110 = 一百一十, 101 = 一百零一, 10 = 十, 1e12 = 一万亿). With short
// false a leading ten is written 一十.
func svRenderHan(n uint64, short bool) string {
	if n == 0 {
		return "零"
	}
	yi, wan, rest := n/1e8, n/1e4%1e4, n%1e4
	var b strings.Builder
	if yi > 0 {
		if yi >= 1e4 {
			b.WriteString(svRenderSection(yi/1e4, short) + "万")
			if yi%1e4 > 0 {
				if yi%1e4 < 1000 {
					b.WriteRune('零')
				}
				b.WriteString(svRenderSection(yi%1e4, false))
			}
		} else {
			b.WriteString(svRenderSection(yi, short))
		}
		b.WriteString("亿")
	}
	if wan > 0 {
		if yi > 0 && wan < 1000 {
			b.WriteRune('零')
		}
		b.WriteString(svRenderSection(wan, short && yi == 0) + "万")
	}
	if rest > 0 {
		if (yi > 0 || wan > 0) && rest < 1000 || yi > 0 && wan == 0 { // 一亿零一千: the empty 万 group is a gap
			b.WriteRune('零')
		}
		b.WriteString(svRenderSection(rest, short && yi == 0 && wan == 0))
	}
	return b.String()
}

var svNextHigher = map[rune]rune{'十': '百', '百': '千', '千': '万'}

// svHanColloquial drops a final unit that is one place below the unit before its digit: 一百一十 → 一百一,
// 两万五千 → 两万五. It returns "" when the form has none.
func svHanColloquial(f string) string {
	rs := []rune(f)
	k := len(rs)
	if k < 4 {
		return ""
	}
	u, d, v := rs[k-1], rs[k-2], rs[k-3]
	if _, ok := svHanSmallUnit[u]; !ok || svHanDigit[d] == 0 || svNextHigher[u] != v {
		return ""
	}
	return string(rs[:k-1])
}

// svHanSeqs: every accepted spelling of n, as sequences whose positions allow 两 for 二 before a unit and 〇 for 零.
func svHanSeqs(n uint64) []svSeq {
	forms := []string{svRenderHan(n, true)}
	if long := svRenderHan(n, false); long != forms[0] {
		forms = append(forms, long)
	}
	for _, f := range append([]string{}, forms...) {
		if c := svHanColloquial(f); c != "" {
			forms = append(forms, c)
		}
	}
	var out []svSeq
	for _, f := range forms {
		rs := []rune(f)
		seq := make(svSeq, len(rs))
		for i, r := range rs {
			switch {
			// 两 is a whole coefficient: before 百 or 千, or before 万 or 亿 when nothing of that group precedes it
			// (两万, 一亿两千万; not 十两万 for 十二万).
			case r == '二' && i+1 < len(rs) && (strings.ContainsRune("百千", rs[i+1]) || strings.ContainsRune("万亿", rs[i+1]) && (i == 0 || strings.ContainsRune("万亿零", rs[i-1]))):
				seq[i] = svAlt{"二", "两"}
			case r == '零':
				seq[i] = svAlt{"零", "〇"}
			default:
				seq[i] = svAlt{string(r)}
			}
		}
		out = append(out, seq)
	}
	if n == 2 {
		out = append(out, lit("两"))
	}
	return out
}

func svAltHas(alt svAlt, s string) bool {
	for _, a := range alt {
		if a == s {
			return true
		}
	}
	return false
}

func svHanUnit(run string) svUnit {
	unit := svUnit{lit(svSplitRunes(run)...)}
	rs := []rune(run)
	hasUnit := false
	for _, r := range rs {
		if _, ok := svHanDigit[r]; !ok {
			hasUnit = true
		}
	}
	if !hasUnit {
		// A digit string (二〇二四). Two digits are an approximation (两三天, 三五天: two or three days), and 两 is
		// never a digit of a string, so neither has a reading in digits.
		if len(rs) == 2 || len(rs) > 2 && strings.ContainsRune(run, '两') {
			return unit
		}
		var ds strings.Builder
		for _, r := range rs {
			ds.WriteByte(byte('0' + svHanDigit[r]))
		}
		unit = append(unit, lit(ds.String()))
		if len(rs) == 1 { // a lone digit is a number: its other spellings (二 and 两, 零 and 〇)
			unit = append(unit, svHanSeqs(uint64(svHanDigit[rs[0]]))...)
		}
		return unit
	}
	n, ok := svParseHan(rs)
	if !ok {
		return unit
	}
	unit = append(unit, lit(strconv.FormatUint(n, 10)))
	if n >= 1000 {
		unit = append(unit, lit(svCommaDigits(n)))
	}
	return append(unit, svHanSeqs(n)...)
}

// --- English number words in the script ---

func svNumberWordValue(w string) (uint64, bool) {
	if v, ok := numwords.SmallValue(w); ok {
		return uint64(v), true
	}
	return 0, false
}

// svParseEnglish reads a cardinal written in words, strictly: "one hundred and five", "a hundred and sixty",
// "two thousand", "a million and one". "zero" is a cardinal only alone ("one thousand zero" is not one number).
// Digit-by-digit readings ("three seven oh four") are not cardinals.
func svParseEnglish(words []string) (uint64, bool) {
	var total, group uint64
	lastScale := uint64(1e15)
	prevSmall, prevTens, sawNumber := false, false, false
	for i, w := range words {
		switch {
		case w == "and": // between a scale word and a number under it ("hundred and five", not "hundred and thousand")
			if i == 0 || i == len(words)-1 || !svIsScaleWord(words[i-1]) || svIsScaleWord(words[i+1]) || words[i+1] == "and" {
				return 0, false
			}
		case w == "a":
			if i+1 >= len(words) || !svIsScaleWord(words[i+1]) || prevSmall || group != 0 {
				return 0, false
			}
			group, prevSmall, prevTens, sawNumber = 1, true, false, true
		case w == "zero":
			if len(words) != 1 {
				return 0, false
			}
			sawNumber = true
		case w == "hundred":
			if group == 0 || group > 9 || !prevSmall {
				return 0, false
			}
			group *= 100
			prevSmall, prevTens = false, false
		case svScales[w] != 0:
			s := svScales[w]
			if group == 0 || s >= lastScale {
				return 0, false
			}
			total += group * s
			group, lastScale, prevSmall, prevTens = 0, s, false, false
		default:
			v, ok := svNumberWordValue(w)
			if !ok {
				return 0, false
			}
			if prevSmall && !(prevTens && v > 0 && v < 10) {
				return 0, false
			}
			if !prevSmall && group%100 != 0 {
				return 0, false
			}
			group += v
			prevSmall, prevTens, sawNumber = true, v >= 20 && v%10 == 0, true
		}
	}
	return total + group, sawNumber
}

func svIsEnglishNumberWord(w string) bool {
	_, ok := svNumberWordValue(w)
	return ok || svIsScaleWord(w)
}

// svOrdinalWords maps an ordinal word to the cardinal word it ends ("first" → "one", "twentieth" → "twenty").
var svOrdinalWords = func() map[string]string {
	m := map[string]string{}
	for v := uint64(1); v < 100; v++ {
		if v < 20 || v%10 == 0 {
			c := numwords.Cardinal(v)
			m[numwords.Ordinal(c)[0]] = c[0]
		}
	}
	for _, s := range []string{"hundred", "thousand", "million", "billion", "trillion"} {
		m[numwords.Ordinal([]string{s})[0]] = s
	}
	m["zeroth"] = "zero"
	return m
}()

const svMaxPhraseWords = 48 // longer than any cardinal up to numwords.MaxCardinal, so parsing stays linear

func svPhraseWord(w string) bool {
	return svIsEnglishNumberWord(w) || svOrdinalWords[w] != "" || w == "and" || w == "a" || w == "point" || w == "percent" || w == "minus" || w == "negative"
}

// svParseEnglishPhrase reads a number written in words, [minus] cardinal [point digits] [percent], or a cardinal
// ending in an ordinal word, and returns it written in digits ("-3.5%", "21st").
func svParseEnglishPhrase(words []string) (string, bool) {
	i, sign := 0, ""
	if len(words) > 0 && (words[0] == "minus" || words[0] == "negative") {
		i, sign = 1, "-"
	}
	end, percent := len(words), false
	if end > i && words[end-1] == "percent" {
		end, percent = end-1, true
	}
	card := words[i:end]
	var frac strings.Builder
	for p, w := range card {
		if w != "point" {
			continue
		}
		digits := card[p+1:]
		if len(digits) == 0 {
			return "", false
		}
		for _, d := range digits {
			v, ok := numwords.SmallValue(d)
			if !ok || v > 9 {
				return "", false
			}
			frac.WriteByte(byte('0' + v))
		}
		card = card[:p]
		break
	}
	if len(card) == 0 {
		return "", false
	}
	ordinal := false
	if c, ok := svOrdinalWords[card[len(card)-1]]; ok {
		if frac.Len() > 0 || percent || sign != "" {
			return "", false
		}
		card = append(append([]string{}, card[:len(card)-1]...), c)
		ordinal = true
	}
	n, ok := svParseEnglish(card)
	if !ok {
		return "", false
	}
	out := sign + strconv.FormatUint(n, 10)
	if frac.Len() > 0 {
		out += "." + frac.String()
	}
	if ordinal {
		out += svOrdinalSuffix(n)
	}
	if percent {
		out += "%"
	}
	return out, true
}

// --- units ---

var svTitles = map[string]string{"Mr": "mister", "Mrs": "missus", "Ms": "miz", "Dr": "doctor"}
var svSuffixTitles = map[string]string{"Jr": "junior", "Sr": "senior"}
var svTitleAbbrev = map[string]string{"mister": "mr", "missus": "mrs", "doctor": "dr", "saint": "st"}
var svCaseFree = map[string][]string{"vs": {"versus"}, "etc": {"et", "cetera"}, "e.g": {"for", "example"}, "i.e": {"that", "is"}}

// svSpelledGap: letters of a spelled code are separated by spaces or initials' dots ("U.S.A."), nothing else.
// svSentenceEnds end a sentence. NFKC folds the full-width ！ and ？ but not the ideographic full stop.
const svSentenceEnds = ".!?。"

func svSpelledGap(gap string) bool { return strings.TrimSpace(strings.ReplaceAll(gap, ".", "")) == "" }

func word(raws []svRaw, k int) bool { return k >= 0 && k < len(raws) && raws[k].kind == svWord }

func svIsCapitalised(s string) bool { rs := []rune(s); return len(rs) > 0 && unicode.IsUpper(rs[0]) }

func svAllCaps(s string) bool { return strings.ToUpper(s) == s && strings.ToLower(s) != s }

func svScriptUnits(s, language string) []svUnit {
	raws := svLex(svPre(s), baseLanguage(language) == "zh")
	english := baseLanguage(language) == "en"
	var units []svUnit
	word := func(k int) bool { return k >= 0 && k < len(raws) && raws[k].kind == svWord }
	lower := func(k int) string { return svNormApostrophes(svLower(raws[k].text, language)) }
	// A capitalised word inside a sentence ("Main" in "on Main St."), not the first word of one.
	innerCapital := func(k int) bool {
		return word(k) && k > 0 && svIsCapitalised(raws[k].text) && !strings.ContainsAny(raws[k].gap, svSentenceEnds+":")
	}
	nextCapital := func(k int) bool { return word(k+1) && svIsCapitalised(raws[k+1].text) }
	inExpression, inWordExpression := svExpressions(raws, false), svExpressions(raws, true)

	// Spoken zeros: "oh" or "o" in a run of digits and number words with no punctuation inside ("oh oh seven",
	// "X O 4"), English only.
	zero := make([]bool, len(raws))
	if english {
		digitish := func(k int) bool {
			if raws[k].kind == svNumber {
				return true
			}
			_, ok := svNumberWordValue(lower(k))
			return word(k) && ok
		}
		oh := func(k int) bool { return word(k) && (lower(k) == "oh" || lower(k) == "o") }
		for k := 0; k < len(raws); {
			j, hasDigit := k, false
			for j < len(raws) && (digitish(j) || oh(j)) && (j == k || !raws[j].punctBefore()) {
				hasDigit = hasDigit || digitish(j)
				j++
			}
			for m := k; m < j; m++ {
				zero[m] = hasDigit && oh(m)
			}
			if j == k {
				j++
			}
			k = j
		}
	}
	singleLetter := func(k int, _ bool) bool {
		if !word(k) || zero[k] {
			return false
		}
		t := []rune(raws[k].text)
		if len(t) != 1 || t[0] >= 128 || !unicode.IsLetter(t[0]) {
			return false
		}
		return true
	}

	// Each pass of the loop reads raws[spanStart:k] into units[unitStart:]; the next pass first gives those units the
	// readings in which a dropped mark beside a word is an apostrophe.
	spanStart, unitStart := 0, 0
	for k := 0; k < len(raws); k++ {
		svKeepMarks(units[unitStart:], raws[spanStart:k], language)
		spanStart, unitStart = k, len(units)
		r := raws[k]
		switch r.kind {
		case svNumber:
			units = append(units, svNumberUnits(r.text, language)...)
			continue
		case svHanRun:
			units = append(units, svHanUnit(r.text))
			continue
		case svUnspacedChar:
			units = append(units, svUnit{lit(svLower(r.text, language))})
			continue
		case svSymbol:
			if english && r.text == "^" && k+1 < len(raws) && raws[k+1].gap == "" && (raws[k+1].text == "2" || raws[k+1].text == "3") {
				power := map[string]string{"2": "squared", "3": "cubed"}[raws[k+1].text]
				w := numwords.Cardinal(uint64(raws[k+1].text[0] - '0'))[0]
				units = append(units, svUnit{lit("^", raws[k+1].text), lit("^", w), lit(power), lit("to", "the", "power", "of", w), lit("to", "the", "power", "of", raws[k+1].text)})
				k++
				continue
			}
			// A sign before a Han numeral is the number's sign: "-五" = "-5" = "- 5".
			if r.dash == svDashSign && k+1 < len(raws) && raws[k+1].kind == svHanRun && raws[k+1].gap == "" {
				units = append(units, svSignedHanUnit(raws[k+1].text))
				k++
				continue
			}
			// A measurement mark after a number ("5'", "5′10″", whose ″ NFKC folds to ′′): feet or minutes, and a
			// pair, or the '"' after feet, inches or seconds. Never silent, so "5'" still refuses "5".
			if english && k > 0 && r.gap == "" && (raws[k-1].kind == svNumber || raws[k-1].kind == svSymbol) {
				if svMeasureMark(r.text) && raws[k-1].kind == svNumber {
					if k+1 < len(raws) && raws[k+1].kind == svSymbol && raws[k+1].text == r.text && raws[k+1].gap == "" {
						units = append(units, append(svUnit{lit(r.text, r.text)}, svNameReadings(svInchNames)...))
						k++
					} else {
						units = append(units, append(svSymbolUnit(r.text, english), svNameReadings(svFootNames)...))
					}
					continue
				}
				if r.text == `"` {
					units = append(units, append(svUnit{lit(r.text)}, svNameReadings(svInchNames)...))
					continue
				}
			}
			unit := svSymbolUnit(r.text, english)
			if r.text == "-" && r.dash == svDashNone && r.gap == "" && k+1 < len(raws) && raws[k+1].gap == "" && !inExpression[k] {
				unit = append(unit, svSeq{}) // a range or score ("3-4", "1-0") is often read with no word for the dash
			} else if r.dash == svDashSilentable && !inWordExpression[k] {
				unit = append(unit, svSeq{}) // a dash between words ("Monday - Friday", "pre- and post-war") may be unspoken
			}
			units = append(units, unit)
			continue
		}
		low := lower(k)
		if zero[k] {
			units = append(units, svUnit{svSeq{svAlt{"oh", "o", "zero", "0"}}})
			continue
		}
		// A run of single letters is a spelled code, transcribed spaced or joined ("X J" = "XJ").
		if singleLetter(k, false) && singleLetter(k+1, true) && svSpelledGap(raws[k+1].gap) {
			j := k
			var letters []string
			for j < len(raws) && singleLetter(j, j > k) && (j == k || svSpelledGap(raws[j].gap)) {
				letters = append(letters, lower(j))
				j++
			}
			// Spaced, the reading is the letters as written. Joined, two letters with an article or pronoun among them
			// would make a word ("a T" = "at"), so a joined reading needs three letters or none of a, I.
			unit := svUnit{lit(letters...)}
			if len(letters) >= 3 || !svAltHas(letters, "a") && !svAltHas(letters, "i") {
				unit = append(unit, lit(strings.Join(letters, "")))
			}
			units = append(units, unit)
			k = j - 1
			continue
		}
		// A domain: "www.example.com" may be read "www dot example dot com". Lowercase only, so "Dr.Smith" and a
		// sentence run together ("end.Next") are not domains. Recognised before number words, so "one.com" is a
		// domain; a label may be one letter ("x.com"), and a hyphenated label is cut as the transcript cuts it.
		labels, j, lowercase := []string{low}, k+1, low == r.text
		for lowercase && j < len(raws) && word(j) && raws[j].gap == "." && lower(j) == raws[j].text {
			labels = append(labels, lower(j))
			j++
		}
		if len(labels) >= 2 { // "one.Next" has no second lowercase label: a number word and a word
			// One unit a label and one a dot, so each choice is independent and the readings stay linear in the
			// labels: a hyphenated label split or joined on its own ("e-mail.long-term.com" = "email dot long term
			// dot com"), and "dot" said or not at each dot. A dot written by the transcript is unspoken, so
			// "www.example dot com" already reads as "www example dot com": a per-dot choice accepts no reading the
			// written dots did not.
			for i, l := range labels {
				if i > 0 {
					units = append(units, svUnit{lit("dot"), lit()})
				}
				ws := strings.FieldsFunc(l, svJoiner)
				unit := svUnit{lit(ws...)}
				if len(ws) > 1 { // "e-mail.com" = "email dot com", as a hyphenated word may be joined
					unit = append(unit, lit(strings.Join(ws, "")))
				}
				units = append(units, unit)
			}
			k = j - 1
			continue
		}
		// A number in words ("a hundred and sixty", "forty-two", "minus seven", "three point five percent",
		// "twenty-first"): the longest prefix of at most svMaxPhraseWords words that reads as one number.
		if english && svPhraseWord(strings.FieldsFunc(low, svJoiner)[0]) {
			var words []string
			ends := []int{} // ends[l-1]: the words in the first l raws
			for j := k; j < len(raws) && j < k+svMaxPhraseWords && word(j) && (j == k || !raws[j].punctBefore()); j++ {
				parts := strings.FieldsFunc(lower(j), svJoiner)
				ok := len(parts) > 0
				for _, p := range parts {
					ok = ok && svPhraseWord(p)
				}
				if !ok || len(words)+len(parts) > svMaxPhraseWords { // the bound counts words, hyphenated ones too
					break
				}
				words = append(words, parts...)
				ends = append(ends, len(words))
			}
			found := false
			for l := len(ends); l > 0 && !found; l-- {
				if digits, ok := svParseEnglishPhrase(words[:ends[l-1]]); ok {
					// The words keep their ordinary readings ("TEN" = "T E N", "forty-two" = "fortytwo") beside the
					// number's, up to svMaxProduct of them; a longer phrase is read as written and as a number.
					plain := make([]svUnit, l)
					for m := range plain {
						plain[m] = svWordUnit(raws, k+m, lower(k+m), english, innerCapital, nextCapital)
					}
					unit, ok := svProduct(plain)
					if !ok {
						unit = svUnit{lit(words[:ends[l-1]]...)}
					}
					for _, reading := range svNumberUnits(digits, language)[0] {
						if !svUnitHas(unit, reading) {
							unit = append(unit, reading)
						}
					}
					units = append(units, unit)
					k += l - 1
					found = true
				}
			}
			if found {
				continue
			}
		}
		// "for example" and "et cetera" may be transcribed abbreviated.
		if english && word(k+1) && !raws[k+1].punctBefore() {
			pair := low + " " + lower(k+1)
			if abbr := map[string]string{"for example": "e.g", "et cetera": "etc", "that is": "i.e"}[pair]; abbr != "" {
				units = append(units, svUnit{lit(low, lower(k+1)), lit(abbr)})
				k++
				continue
			}
		}
		units = append(units, svWordUnit(raws, k, low, english, innerCapital, nextCapital))
	}
	svKeepMarks(units[unitStart:], raws[spanStart:], language)
	return units
}

// svKeepMarks gives the units read from the raws of span the readings in which a mark svUnquote dropped beside one of
// their words is an apostrophe ("‘the students’ books’" may say "the students’ books"). A reading that spells words as
// written (their parts, in order, from the first word not yet placed) takes each word's marks on its own parts; the
// unit's other readings of those words ("email" for "e-mail", "100" for "a hundred") take the first word's leading
// mark and the last word's trailing one. A unit with no reading of the words as written ("dot" in a domain) is left
// as it is. Each reading gains at most three spellings per position, so the readings stay linear in the marks.
func svKeepMarks(units []svUnit, span []svRaw, language string) {
	type part struct {
		text        string
		lead, trail bool
	}
	var words []part
	marked := false
	for _, r := range span {
		if r.kind != svWord {
			continue
		}
		parts := strings.FieldsFunc(svNormApostrophes(svLower(r.text, language)), svJoiner)
		for i, p := range parts {
			words = append(words, part{p, i == 0 && r.optLead, i == len(parts)-1 && r.optTrail})
		}
		marked = marked || r.optLead || r.optTrail
	}
	if !marked {
		return
	}
	spells := func(seq svSeq, at int) bool {
		if len(seq) == 0 || at+len(seq) > len(words) {
			return false
		}
		for p, alt := range seq {
			if !svAltHas(alt, words[at+p].text) {
				return false
			}
		}
		return true
	}
	at := 0
	for u, unit := range units {
		literal := slices.IndexFunc(unit, func(seq svSeq) bool { return spells(seq, at) })
		if literal < 0 {
			continue
		}
		n := len(unit[literal])
		first, last := words[at], words[at+n-1]
		out := make(svUnit, len(unit))
		for i, seq := range unit {
			ns := slices.Clone(seq)
			switch {
			case i == literal:
				for p := range ns {
					ns[p] = svWithMarks(ns[p], words[at+p].lead, words[at+p].trail)
				}
			case len(ns) == 1:
				ns[0] = svWithMarks(ns[0], first.lead, last.trail)
			case len(ns) > 1:
				ns[0] = svWithMarks(ns[0], first.lead, false)
				ns[len(ns)-1] = svWithMarks(ns[len(ns)-1], false, last.trail)
			}
			out[i] = ns
		}
		units[u] = out
		at += n
	}
}

// svWithMarks is a new alt holding each token of alt in all its svMarkReadings.
func svWithMarks(alt svAlt, lead, trail bool) svAlt {
	if !lead && !trail {
		return alt
	}
	var out svAlt
	for _, s := range alt {
		out = append(out, svMarkReadings(s, lead, trail)...)
	}
	return out
}

func svWordUnit(raws []svRaw, k int, low string, english bool, innerCapital, nextCapital func(int) bool) svUnit {
	r := raws[k]
	parts := strings.FieldsFunc(low, svJoiner)
	unit := svUnit{lit(parts...)}
	if len(parts) > 1 {
		unit = append(unit, lit(strings.Join(parts, ""))) // "e-mail" = "email"
	}
	if !english {
		return svWithCodeReadings(unit, raws, k, low)
	}
	bare := r.text // the lexer keeps no dot in a word ("e.g." is "e.g")
	// A title only with its capital and before a name: "ms" is milliseconds, "5 Ms." is no one. "Dr" and "St" after
	// a capitalised word inside a sentence are a road ("Take Oak Dr.", "Main St. Next").
	road := !strings.ContainsAny(r.gap, svSentenceEnds) && (innerCapital(k-1) || word(raws, k-1) && svIsCapitalised(raws[k-1].text) && !nextCapital(k))
	if exp, ok := svTitles[bare]; ok && nextCapital(k) && !(road && bare == "Dr") {
		unit = append(unit, lit(exp))
	}
	if exp, ok := svSuffixTitles[bare]; ok && word(raws, k-1) && svIsCapitalised(raws[k-1].text) {
		unit = append(unit, lit(exp)) // after a name: "John Smith Jr."
	}
	switch {
	case bare == "Dr" && road:
		unit = append(unit, lit("drive"))
	case bare == "St" && road:
		unit = append(unit, lit("street"))
	case bare == "St" && nextCapital(k) && !road:
		unit = append(unit, lit("saint"))
	}
	if abbr, ok := svTitleAbbrev[low]; ok && nextCapital(k) {
		unit = append(unit, lit(abbr)) // "Doctor Smith" may be transcribed "Dr. Smith"
	}
	// "vs", "etc", "e.g", "i.e" expand only in lower case (a capital first letter is a sentence start): "VS Code"
	// is a code and "E.G. White" initials.
	if exp, ok := svCaseFree[strings.ToLower(bare)]; ok && strings.ToLower(bare[1:]) == bare[1:] {
		unit = append(unit, lit(exp...))
		if strings.Contains(bare, ".") {
			unit = append(unit, lit(strings.Split(strings.ToLower(bare), ".")...), lit(strings.ReplaceAll(strings.ToLower(bare), ".", "")))
		}
	}
	return svWithCodeReadings(unit, raws, k, low)
}

// svCapsWords are common short English words written in capitals for emphasis ("Turn ON", "He said NO"); they are
// never spelled. Short codes that are also words (US, IT) are ambiguous; they stay codes, and OK is spelled.
var svCapsWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`AM AN AS AT BE BY DO GO HE HI IF IN IS ME MY NO OF OH ON OR SO TO UP WE YO
		ALL AND ANY ARE ASK BAD BIG BOY BUT CAN DAY DID END FAR FEW FOR GET GOT HAD HAS HER HEY HIM HIS HOT HOW ITS LET
		LOT MAN MAY NEW NOT NOW OFF OLD ONE OUR OUT OWN PUT RUN SAY SEE SET SHE SIT TOO TOP TRY TWO USE WAS WAY WHO WHY
		WIN YES YET YOU`) {
		m[w] = true
	}
	return m
}()

// svWithCodeReadings adds a letter-by-letter reading to a code: an all-capital word of two or three letters, or of
// up to six with no vowel ("API", "HTML"), unless a neighbour is capitalised too (a shouted "STOP NOW"); and two or
// three letters written against a number ("mp3", "Nf3"), or a capital code of up to six against one ("JIRA-10442").
func svWithCodeReadings(unit svUnit, raws []svRaw, k int, low string) svUnit {
	text := raws[k].text
	rs := []rune(text)
	// A capital neighbour that is a word, not a code, makes this shouting ("STOP NOW"); two codes stay codes ("API URL").
	shout := func(j int) bool {
		if j < 0 || j >= len(raws) || raws[j].kind != svWord || !svAllCaps(raws[j].text) {
			return false
		}
		n := len([]rune(raws[j].text))
		return n >= 4 && strings.ContainsAny(raws[j].text, "AEIOUY") || svCapsWords[raws[j].text]
	}
	attached := func(gap string) bool { return gap == "" || gap == "-" }
	glued := k+1 < len(raws) && raws[k+1].kind == svNumber && attached(raws[k+1].gap) || k > 0 && raws[k-1].kind == svNumber && attached(raws[k].gap)
	plain := !strings.ContainsAny(text, "'-‐‑")
	code := svAllCaps(text) && !svCapsWords[text] && !shout(k-1) && !shout(k+1) && (len(rs) >= 2 && len(rs) <= 3 || len(rs) <= 6 && (glued || !strings.ContainsAny(strings.ToUpper(text), "AEIOUY")))
	if plain && (code || glued && len(rs) >= 2 && len(rs) <= 3) {
		unit = append(unit, lit(svSplitRunes(low)...))
	}
	return unit
}

// svMaxProduct is the most readings a phrase's words keep beside the number they spell (svProduct).
const svMaxProduct = 64

// svProduct is every concatenation of one reading per unit, or false beyond svMaxProduct readings.
func svProduct(units []svUnit) (svUnit, bool) {
	out := svUnit{svSeq{}}
	for _, u := range units {
		var next svUnit
		for _, a := range out {
			for _, b := range u {
				next = append(next, append(append(svSeq{}, a...), b...))
				if len(next) > svMaxProduct {
					return nil, false
				}
			}
		}
		out = next
	}
	return out, true
}

// svUnitHas reports whether unit already holds the reading seq.
func svUnitHas(unit svUnit, seq svSeq) bool {
	for _, r := range unit {
		if slices.EqualFunc(r, seq, slices.Equal[svAlt]) {
			return true
		}
	}
	return false
}

// svMathOperators make a run holding words an expression (svExpressions).
var svMathOperators = map[string]bool{"=": true, "+": true, "−": true, "×": true, "÷": true, "<": true, ">": true, "≠": true, "*": true, "^": true}

// svExpressions marks each raw that sits in an expression: a run of numbers and symbols joined by nothing but space
// that holds an operator other than "-" or a number's "'" or '"' ("5-2=3"). A dash there is a spoken minus. Punctuation ends
// a run, so in "1-0. #1" the score's dash is not in one. withWords lets words into the run but counts only the
// arithmetic operators (svMathOperators): in "x - y = z" the free dash is a minus, in "Tom & Jerry - live" it may be
// silent. One pass over the raws, so a long chain costs its length.
func svExpressions(raws []svRaw, withWords bool) []bool {
	in := make([]bool, len(raws))
	part := func(k int) bool {
		switch raws[k].kind {
		case svNumber, svHanRun, svSymbol:
			return true
		case svWord:
			return withWords
		case svUnspacedChar: // a word of its own, and a Han numeral outside zh still a number ("五-二=三" in ja)
			return withWords || isHanNumeral([]rune(raws[k].text)[0])
		}
		return false
	}
	op := func(k int) bool {
		if raws[k].kind != svSymbol {
			return false
		}
		if withWords {
			return svMathOperators[raws[k].text]
		}
		return raws[k].text != "-" && raws[k].text != "'" && raws[k].text != `"`
	}
	for i := 0; i < len(raws); {
		if !part(i) {
			i++
			continue
		}
		j, holds := i, false
		for j < len(raws) && part(j) && (j == i || !raws[j].punctBefore()) {
			holds = holds || op(j)
			j++
		}
		for m := i; m < j; m++ {
			in[m] = holds
		}
		i = j
	}
	return in
}

// svFootNames and svInchNames are the English readings of a measurement mark after a number (svMeasureMark), and of
// two of them or the '"' after feet.
var (
	svFootNames = []string{"feet", "foot", "minutes", "minute"}
	svInchNames = []string{"inches", "inch", "seconds", "second"}
)

// svNameReadings is one single-word reading per name.
func svNameReadings(names []string) svUnit {
	unit := make(svUnit, len(names))
	for i, n := range names {
		unit[i] = lit(n)
	}
	return unit
}

// svSignedHanUnit reads a Han numeral after its sign: each reading of the numeral after the sign as a token of its
// own ("-" or "−"), and a reading in digits also with the sign joined to it, as the transcript lexes "-5".
func svSignedHanUnit(run string) svUnit {
	var unit svUnit
	for _, seq := range svHanUnit(run) {
		unit = append(unit, append(svSeq{svAlt{"-", "−"}}, seq...))
		if len(seq) == 0 {
			continue
		}
		var signed svAlt
		for _, t := range seq[0] {
			if t != "" && svDigit(rune(t[0])) {
				signed = append(signed, "-"+t)
			}
		}
		if len(signed) > 0 {
			unit = append(unit, append(svSeq{signed}, seq[1:]...))
		}
	}
	return unit
}

// svSymbolNames: the English names of spoken symbols. A symbol without a name matches only itself.
var svSymbolNames = map[string][]string{"%": {"percent"}, "+": {"plus"}, "=": {"equals", "equal", "is"}, "&": {"and"},
	"@": {"at"}, "°": {"degrees", "degree"}, "-": {"minus", "to"}, "−": {"minus"}, "×": {"times", "multiplied by"}, "÷": {"divided by"},
	"<": {"less than"}, ">": {"greater than"}, "≠": {"not equal to", "does not equal"}, "#": {"number", "hash", "sharp"},
	"*": {"star", "asterisk", "times"}, "′": {"prime"}, "^": {"to the power of"}}

func svSymbolUnit(sym string, english bool) svUnit {
	unit := svUnit{lit(sym)}
	switch sym { // the two minuses are written either way
	case "-":
		unit = append(unit, lit("−"))
	case "−":
		unit = append(unit, lit("-"))
	}
	if english {
		for _, n := range svSymbolNames[sym] {
			unit = append(unit, lit(strings.Fields(n)...))
		}
	}
	return unit
}

// --- matching ---

// svMatch simulates the units over the transcript positions as bitsets: bit j of the frontier says the units so far
// can end at token j. A step ANDs the frontier with the positions where an accepted token (or a hyphen chain that
// joins to it) starts and shifts by its length. Work follows the frontier's actual span, so a short script costs
// little and an adversarial one costs its span, not a memo of every (unit, position) pair.
func svMatch(units []svUnit, toks []svTok) bool {
	m := newSvMasks(toks)
	words := len(toks)/64 + 1
	frontier, next := newSvSet(words), newSvSet(words)
	a, b := newSvSet(words), newSvSet(words)
	frontier.set(0)
	for _, unit := range units {
		next.reset()
		for _, seq := range unit {
			cur := frontier
			for p, alt := range seq {
				dst := a
				if p%2 == 1 {
					dst = b
				}
				dst.reset()
				for _, s := range alt {
					for _, e := range m[s] {
						dst.orShifted(cur, e)
					}
				}
				cur = dst
				if cur.empty() {
					break
				}
			}
			next.or(cur)
		}
		frontier, next = next, frontier
		if frontier.empty() {
			return false
		}
	}
	return frontier.has(len(toks))
}

const svMaxChain = 8 // hyphen chains joined for matching ("e-mail" = "email"); longer chains match part by part

// svMask: the transcript positions where a token string (k = 1) or a hyphen chain of k tokens joining to it starts,
// stored sparsely by 64-bit word.
type svMask struct {
	k    int
	idx  []int
	bits []uint64
}

func newSvMasks(toks []svTok) map[string][]*svMask {
	m := map[string][]*svMask{}
	add := func(s string, k, pos int) {
		var e *svMask
		for _, x := range m[s] {
			if x.k == k {
				e = x
			}
		}
		if e == nil {
			e = &svMask{k: k}
			m[s] = append(m[s], e)
		}
		w := pos / 64
		if n := len(e.idx); n == 0 || e.idx[n-1] != w {
			e.idx, e.bits = append(e.idx, w), append(e.bits, 0)
		}
		e.bits[len(e.bits)-1] |= 1 << (pos % 64)
	}
	for j, t := range toks {
		for _, v := range svMarkReadings(t.text, t.lead, t.trail) {
			add(v, 1, j)
		}
		joined := t.text
		for k := 2; k <= svMaxChain && j+k-1 < len(toks) && toks[j+k-2].link; k++ {
			joined += toks[j+k-1].text
			// Normalised again: joined jamo or a base and its mark compose ("ᄀ-ᅡ" is 가), as script tokens are.
			for _, v := range svMarkReadings(norm.NFKC.String(joined), t.lead, toks[j+k-1].trail) {
				add(v, k, j)
			}
		}
	}
	return m
}

// svMarkReadings is s, then s with each mark a dropped quotation mark may have been, as an apostrophe: before it
// (lead), after it (trail), or both. At most four readings, so the work stays linear in the marks.
func svMarkReadings(s string, lead, trail bool) []string {
	out := []string{s}
	if lead {
		out = append(out, "'"+s)
	}
	if trail {
		out = append(out, s+"'")
	}
	if lead && trail {
		out = append(out, "'"+s+"'")
	}
	return out
}

type svSet struct {
	w      []uint64
	lo, hi int // words outside [lo, hi] are zero
}

func newSvSet(words int) *svSet { return &svSet{w: make([]uint64, words), lo: words, hi: -1} }

func (s *svSet) reset() {
	for i := s.lo; i <= s.hi; i++ {
		s.w[i] = 0
	}
	s.lo, s.hi = len(s.w), -1
}

func (s *svSet) put(i int, v uint64) {
	if v == 0 {
		return
	}
	s.w[i] |= v
	s.lo, s.hi = min(s.lo, i), max(s.hi, i)
}

func (s *svSet) set(pos int)      { s.put(pos/64, 1<<(pos%64)) }
func (s *svSet) has(pos int) bool { return s.w[pos/64]&(1<<(pos%64)) != 0 }
func (s *svSet) empty() bool      { return s.lo > s.hi }
func (s *svSet) or(src *svSet) {
	for i := src.lo; i <= src.hi; i++ {
		s.put(i, src.w[i])
	}
}

// orShifted: s |= (src AND the mask's starts) shifted by the mask's length.
func (s *svSet) orShifted(src *svSet, e *svMask) {
	if src.empty() {
		return
	}
	for n := sort.SearchInts(e.idx, src.lo); n < len(e.idx) && e.idx[n] <= src.hi; n++ {
		i := e.idx[n]
		x := src.w[i] & e.bits[n]
		if x == 0 {
			continue
		}
		s.put(i, x<<e.k)
		if i+1 < len(s.w) {
			s.put(i+1, x>>(64-e.k))
		}
	}
}

// ParseHanNumeral reads s as a Chinese numeral written in characters (一百一十, 两万五, 零), or as a digit string of
// one digit or three or more (一〇八, 二〇二四), and reports false for a run that is not a well-formed numeral
// (一五一十, 亿万, 零十), a two-digit string (三五 is "three or five", an approximation), a digit string with 两, or
// a value that overflows. Units are read by the strict parser the said guard uses; recite's canonicaliser shares it.
func ParseHanNumeral(s string) (uint64, bool) {
	rs := []rune(s)
	digits := len(rs) > 0
	for _, r := range rs {
		if _, ok := svHanDigit[r]; !ok {
			digits = false
		}
	}
	if !digits {
		return svParseHan(rs)
	}
	if len(rs) == 2 || len(rs) > 2 && strings.ContainsRune(s, '两') {
		return 0, false
	}
	var n uint64
	for _, r := range rs {
		d := svHanDigit[r]
		if n > (math.MaxUint64-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}
