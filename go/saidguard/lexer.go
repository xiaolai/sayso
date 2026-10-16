package saidguard

import (
	"strings"
	"unicode"
)

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
	// For a word whose first or last letter touches marks svUnquote dropped as possible quotation marks, how many: they
	// may as well be apostrophes, so the word may also be read with them ("‘the students’ books’", "''yes''").
	optLead, optTrail int
	dash              svDash // set on a symbol that is a free dash (svFreeDash) or a '−' (svDashRole)
}

// svDash is the role of a free dash, or of a '−' that is no minus between numbers (svDashRole): a sign ("-x", "-五"), a
// dash before a number that is no sign ("A-五"), both never silent, or a dash the script side may read with no word
// outside an expression ("Monday - Friday").
type svDash int

const (
	svDashNone svDash = iota
	svDashSign
	svDashSpoken
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

// svSignOperand: a '-' or '−' before a digit, or a decimal point or comma before one ("-.5", "-,5"), with or without
// spaces between ("- 5"), is a sign at the start, after a space, punctuation or an unspaced character, or as an
// exponent sign ("1e-3"); after a letter or digit it is none: a joiner when glued ("JIRA-10442"), and after a number,
// spaced or not, a minus ("3-4", "5 -2", which svBetweenNumbers decides first). The operand decides, so the spaces
// before it never do. It returns the index of the operand, or -1 when rs[i] is no such sign.
func svSignOperand(rs []rune, i int) int {
	if !svMinus(rs[i]) {
		return -1
	}
	o := svPastSpaces(rs, i+1)
	if o >= len(rs) || !svDigit(rs[o]) && !((rs[o] == '.' || rs[o] == ',') && o+1 < len(rs) && svDigit(rs[o+1])) {
		return -1
	}
	if i == 0 {
		return o
	}
	p := rs[i-1]
	if o == i+1 && (p == 'e' || p == 'E') && i >= 2 && svDigit(rs[i-2]) {
		return o
	}
	if svLetter(p) || svDigit(p) || unicode.IsMark(p) {
		return -1
	}
	return o
}

func svSignAt(rs []rune, i int) bool { return svSignOperand(rs, i) >= 0 }

// svPastSpaces is the first index from i on that holds no space, len(rs) at the end.
func svPastSpaces(rs []rune, i int) int {
	for i < len(rs) && unicode.IsSpace(rs[i]) {
		i++
	}
	return i
}

// svMarkRunEnds holds, for each index, the first index from it on that holds no apostrophe, so a run of marks of any
// length is read as one ("”yes”", "五’’’"); len(rs) at the end. One reverse pass.
func svMarkRunEnds(rs []rune) []int {
	end := make([]int, len(rs)+1)
	end[len(rs)] = len(rs)
	for i := len(rs) - 1; i >= 0; i-- {
		end[i] = i
		if isApostrophe(rs[i]) {
			end[i] = end[i+1]
		}
	}
	return end
}

func isHanNumeral(r rune) bool {
	_, d := svHanDigit[r]
	_, u := svHanBigUnit[r]
	_, s := svHanSmallUnit[r]
	return d || u || s
}

func svLex(s string, hanNumbers bool) []svRaw {
	rs, dropped := svUnquote([]rune(s))
	markEnd := svMarkRunEnds(rs)
	// marksBeforeLetter reports whether the marks from rs[i] on, however many, are followed by a letter ("’tis",
	// "''yes").
	marksBeforeLetter := func(i int) bool { return markEnd[i] < len(rs) && svLetter(rs[markEnd[i]]) }
	// droppedBefore and droppedFrom count the dropped marks touching rs[i] on its left, and from rs[j] on: each is
	// walked from at most the two words beside its run, so the work stays linear.
	droppedBefore := func(i int) int {
		n := 0
		for i-n > 0 && dropped[i-n-1] {
			n++
		}
		return n
	}
	droppedFrom := func(j int) int {
		n := 0
		for j+n < len(rs) && dropped[j+n] {
			n++
		}
		return n
	}
	var out []svRaw
	prevEnd := 0
	// tokenEnd is the end of the last number or unspaced character, or of the run of kept marks touching one, carried
	// forward so a run of any length costs its length.
	tokenEnd := -1
	body := -1 // the operand of the sign the next number starts with
	emit := func(kind svKind, i, j int, dash svDash) {
		text := string(rs[i:j])
		switch {
		case kind == svNumber && svMinus(rs[i]):
			// One sign, however written and however far from its digits: "−5" and "- 5" read as "-5".
			if body < 0 {
				body = i + 1
			}
			text = "-" + string(rs[body:j])
		case kind == svSymbol && isApostrophe(rs[i]):
			text = "'" // one apostrophe, however written
		}
		body = -1
		raw := svRaw{text: text, kind: kind, start: i, end: j, gap: string(rs[prevEnd:i]), dash: dash}
		if kind == svWord {
			raw.optLead = droppedBefore(i)
			raw.optTrail = droppedFrom(j)
		}
		out = append(out, raw)
		switch {
		case kind == svNumber || kind == svHanRun || kind == svUnspacedChar:
			tokenEnd = j
		case kind == svSymbol && text == "'" && i == tokenEnd:
			tokenEnd = j
		default:
			tokenEnd = -1
		}
		prevEnd = j
	}
	for i := 0; i < len(rs); {
		r := rs[i]
		sign := -1
		if !svBetweenNumbers(rs, i) {
			sign = svSignOperand(rs, i)
		}
		switch {
		case svDigit(r) || sign >= 0 || r == '.' && i+1 < len(rs) && svDigit(rs[i+1]) && (i == 0 || !svDigit(rs[i-1])) ||
			r == ',' && i+1 < len(rs) && svDigit(rs[i+1]) && (i == 0 || unicode.IsSpace(rs[i-1])):
			// A leading "," is a number only after a space (",5" is a French decimal); "Yes,3" is a word and a number.
			j := i + 1
			if sign >= 0 {
				j, body = sign, sign
			}
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
		case svMinus(r) && svBetweenNumbers(rs, i):
			emit(svSymbol, i, i+1, svDashNone)
			i++
		case r == '−':
			// A minus sign is spoken wherever it stands; before a Han numeral or a word it may be a sign.
			dash := svDashSpoken
			if svDashRole(rs, i) == svDashSign {
				dash = svDashSign
			}
			emit(svSymbol, i, i+1, dash)
			i++
		case svSpokenSymbol(r):
			emit(svSymbol, i, i+1, svDashNone)
			i++
		case svFreeDash(rs, i):
			emit(svSymbol, i, i+1, svDashRole(rs, i))
			i++
		case isApostrophe(r) && !marksBeforeLetter(i) && (tokenEnd == i || markEnd[i] < len(rs) && isUnspaced(rs[markEnd[i]])):
			// Marks right after a number or an unspaced character that svUnquote kept, however many, are no quotation
			// marks: feet or minutes ("5'", "5’10", "21st’"), with a second one inches or seconds ("5''"), or marks of
			// unspaced text on either side ("五’’’", "好’", "’’好"), kept as a word keeps its own ("’tis"). Each is a
			// spoken symbol on both sides, so a transcript cannot drop one.
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
		case svLetter(r) || isApostrophe(r) && marksBeforeLetter(i):
			// "e.g." and "i.e." are one word, also at a sentence start ("E.g."). With a capital second letter they are
			// initials ("E.G. White"), lexed letter by letter like any other; with a mark on it ("e.g̣") another word.
			if m := strings.ToLower(string(rs[i:min(i+4, len(rs))])); (strings.HasPrefix(m, "e.g") || strings.HasPrefix(m, "i.e")) && !unicode.IsUpper(rs[i+2]) && (i == 0 || !svLetter(rs[i-1]) && rs[i-1] != '.') && (i+3 >= len(rs) || !svLetter(rs[i+3]) && !unicode.IsMark(rs[i+3])) && !(i+4 < len(rs) && rs[i+3] == '.' && svLetter(rs[i+4])) {
				// Not inside a domain ("www.e.g.com"), where its letters are labels.
				emit(svWord, i, i+3, svDashNone)
				i += 3
				if i < len(rs) && rs[i] == '.' {
					i++
					out[len(out)-1].optTrail = droppedFrom(i) // the dot is the abbreviation's: "‘e.g.’"
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
				case isApostrophe(c) && (j > i || marksBeforeLetter(j)):
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

// svUnquote finds the marks that may be quotation marks around words ("Say ‘hello’.", "'yes'", "”yes”") and
// replaces each with an unspoken '"', reporting which. A run of marks of any length is read as one: it opens or closes
// as a whole. It does not decide which mark closes which: in "‘the students’ books’"
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
	// The runs of marks that may open or close, as [start, end) pairs; a run between two unspaced characters may be
	// both.
	type run struct{ start, end int }
	var openers, closers []run
	drop := func(r run) {
		for m := r.start; m < r.end; m++ {
			dropped[m] = true
		}
	}
	flush := func() {
		if len(openers) > 0 && len(closers) > 0 {
			first, last := openers[0].start, closers[len(closers)-1].start
			for _, o := range openers {
				if o.start < last {
					drop(o)
				}
			}
			for _, c := range closers {
				if c.start > first {
					drop(c)
				}
			}
		}
		openers, closers = openers[:0], closers[:0]
	}
	markEnd := svMarkRunEnds(rs)
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
	for i := 0; i < len(rs); {
		if !isApostrophe(rs[i]) {
			if svSentenceEndAt(rs, i) {
				if pending = markFrom(i + 1); pending < 0 {
					flush()
				}
			}
			i++
			continue
		}
		e := markEnd[i] // the run rs[i:e], tested by what stands before and after it
		k := i - 1
		for k >= 0 && svInnerPunct(rs[k]) {
			k--
		}
		// An unspaced character is a word of its own, so it is a word boundary on either side, as a space is.
		closer := k >= 0 && (svLetter(rs[k]) || unicode.IsMark(rs[k]) || svDigit(rs[k]) || isUnspaced(rs[k])) &&
			(e == len(rs) || unicode.IsSpace(rs[e]) || isUnspaced(rs[e]) || strings.ContainsRune(svUnspokenPunct, rs[e]))
		opener := (i == 0 || unicode.IsSpace(rs[i-1]) || isUnspaced(rs[i-1]) || strings.ContainsRune("([{\"“«", rs[i-1])) && e < len(rs) &&
			(svLetter(rs[e]) || svDigit(rs[e]) || isUnspaced(rs[e])) && !svElisionAt(rs, e)
		// Between two unspaced characters a run may open or close ("说‘好’"): it is a candidate for both.
		if pending == i {
			pending = -1
			if closer {
				closers = append(closers, run{i, e})
				if opener {
					openers = append(openers, run{i, e})
				}
				if pending = markFrom(e); pending < 0 {
					flush()
				}
				i = e
				continue
			}
			flush()
		}
		if closer {
			closers = append(closers, run{i, e})
		}
		if opener {
			openers = append(openers, run{i, e})
		}
		i = e
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

// svDashRole is the role of a free dash (spec/said-guard.md, the dash table). Before a number, a Han numeral as a
// digit, past any spaces, the operand decides: a sign unless a letter, digit or mark comes before it, as svSignOperand
// has it ("-五", "- 五", "气温-五度"), and otherwise a spoken dash ("A-五", "x- 5"); never silent either way. Before a word
// or any other unspaced character it is a sign only touching it at a word start, after a space, punctuation or the
// start ("-x"): a spaced dash before a word separates ("Monday - Friday", "- item"), and a dash after an unspaced
// character joins two words ("北京-上海"), as a hyphen joins "x-y". Any other free dash may be silent.
func svDashRole(rs []rune, i int) svDash {
	n := svPastSpaces(rs, i+1)
	if n >= len(rs) {
		return svDashSilentable
	}
	afterWord := i > 0 && (svLetter(rs[i-1]) || svDigit(rs[i-1]) || unicode.IsMark(rs[i-1]))
	if svOperandAt(rs, n) {
		if afterWord {
			return svDashSpoken
		}
		return svDashSign
	}
	if n == i+1 && !afterWord && !(i > 0 && isUnspaced(rs[i-1])) && (svLetter(rs[n]) || isUnspaced(rs[n])) {
		return svDashSign
	}
	return svDashSilentable
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
// script word equal to the joined letters consume the whole chain. lead and trail count the dropped marks (svUnquote)
// beside a word that may have been apostrophes: the token may also be read with them.
type svTok struct {
	text        string
	link        bool
	lead, trail int
}

func svTranscriptTokens(s, language string) []svTok {
	var out []svTok
	for _, t := range svLex(svPre(s), false) {
		text := svNormApostrophes(svLower(t.text, language))
		switch {
		case t.kind == svWord:
			parts := strings.FieldsFunc(text, svJoiner)
			for i, part := range parts {
				tok := svTok{text: part, link: i < len(parts)-1}
				if i == 0 {
					tok.lead = t.optLead
				}
				if i == len(parts)-1 {
					tok.trail = t.optTrail
				}
				out = append(out, tok)
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
