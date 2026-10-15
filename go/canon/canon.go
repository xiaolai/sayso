// Package canon is a symmetric canonicaliser for scoring speech-to-text against a script: both sides are reduced
// to comparison tokens and compared. It suits a tolerant score; the said guard (package saidguard) is the strict
// check that a reading said the script. It comes from recite (github.com/xiaolai/recite), whose STT scoring uses it.
package canon

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xiaolai/sayso/go/numwords"
	"github.com/xiaolai/sayso/go/saidguard"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// CanonVersion identifies the canonicaliser's behaviour. Bump it on any change
// to Canon's output: cache keys and verification provenance record it.
const CanonVersion = 9 // 9: strict Han numerals, case-sensitive titles

// Canon canonicalises s into comparison tokens : NFKC and width
// folding, abbreviation expansion, lower case, numbers to English words (one
// canonical form for digits and words), apostrophes removed, hyphens to a
// space, every other punctuation or symbol removed, and a run of single Latin letters joined
// into one token ("X J" = "XJ": a spelled-out code). Canon is idempotent:
// Canon(strings.Join(Canon(x), " ")) equals Canon(x).
func Canon(s string) []string { return canon(s, false) }

// Equivalent reports whether a and b canonicalise equal, also trying the
// alternatives Canon cannot decide alone: hyphenated compounds joined
// ("e-mail" = "email") and years read as pairs ("nineteen ninety-nine" =
// "1999", whose canonical form is the cardinal).
func Equivalent(a, b string) bool {
	for _, join := range []bool{false, true} {
		ta, tb := canon(a, join), canon(b, join)
		if equalTokens(ta, tb) || equalTokens(yearPairsToCardinal(ta), yearPairsToCardinal(tb)) {
			return true
		}
	}
	return false
}

func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func canon(s string, joinHyphens bool) []string {
	// An invalid byte ends up a separator anyway; making it one first keeps the number pass
	// from stalling on it ("\xf7¾" would keep "3 4" as digits, and a second pass read them).
	s = strings.ToValidUTF8(s, " ")
	s = width.Fold.String(norm.NFKC.String(s))
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
		case isHyphen(r):
			if !joinHyphens {
				b.WriteByte(' ')
			}
		case tokenRune(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	// Letters join before abbreviations expand, or a second pass would expand what the first
	// joined ("M s" -> "ms" -> "miz").
	// Dropping an apostrophe can bring a letter and a combining mark together ("A'̆"), which a
	// second pass would compose; compose them now.
	return expandTokens(joinLetters(spokenZeros(scaleArticle(dropNumberAnd(strings.Fields(norm.NFKC.String(b.String())))))))
}

// spokenZeros reads "oh" and "o" as zero inside a reading of digits: the spoken form says
// "three seven oh four" for 3704 and "four point oh" for 4.0, and STT writes "O" or "oh". A
// run of them counts when a number word stands right before or after it, or "point" right
// before it, so "oh no" and the letter O stay. It runs before joinLetters, which would join "O O" into "oo".
func spokenZeros(toks []string) []string {
	zero := func(t string) bool { return t == "oh" || t == "o" }
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
		if i > 0 && (isNumberWord(out[i-1]) || out[i-1] == "point") || j < len(out) && isNumberWord(out[j]) {
			for k := i; k < j; k++ {
				out[k] = "zero"
			}
		}
		i = j
	}
	return out
}

// joinLetters joins each run of two or more single-letter Latin tokens into one token. A code
// read letter by letter is transcribed spaced ("X J 4471 B") or not ("XJ 4471B"); joining
// removes only that spacing, so different letters still compare different.
func joinLetters(toks []string) []string {
	single := func(t string) bool { return len(t) == 1 && t[0] >= 'a' && t[0] <= 'z' }
	var out []string
	for i := 0; i < len(toks); {
		j := i
		for j < len(toks) && single(toks[j]) {
			j++
		}
		if j-i >= 2 {
			out = append(out, strings.Join(toks[i:j], ""))
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
func expandTokens(toks []string) []string {
	var out []string
	for _, t := range toks {
		// Case is gone here, so a title ("ms", "st", "dr") cannot be told from a unit, a suffix or joined spelled
		// letters; only the case-free abbreviations ("vs", "etc") expand.
		switch exp := abbreviations[t]; {
		case exp != "" && !titleAbbreviation[t]:
			out = append(out, strings.Fields(exp)...)
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
			if r, _ := utf8.DecodeRuneInString(strings.TrimLeft(s[loc[1]:], " \t")); unicode.IsUpper(r) {
				exp = "saint"
			}
		}
		b.WriteString(exp)
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
		if c == '-' && i+1 < len(s) && isASCIIDigit(s[i+1]) {
			prev, _ := utf8.DecodeLastRuneInString(s[:i])
			if i == 0 || !isWordRune(prev) {
				neg, start = true, i+1
			}
		}
		if !neg && !isASCIIDigit(c) {
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
func scaleArticle(toks []string) []string {
	out := slices.Clone(toks)
	for i := range out {
		if out[i] == "a" && i+1 < len(out) && isScale(out[i+1]) {
			out[i] = "one"
		}
	}
	return out
}

// dropNumberAnd removes "and" inside a number reading ("one hundred and
// five" = "one hundred five"), the form numbers are canonicalised to.
func dropNumberAnd(toks []string) []string {
	out := toks[:0:0]
	for i, t := range toks {
		if t == "and" && i > 0 && i+1 < len(toks) && isScale(toks[i-1]) && isNumberWord(toks[i+1]) {
			continue
		}
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

// yearPairsToCardinal rewrites year-pair readings for 1000..2099 ("nineteen
// ninety nine", "nineteen oh five", "nineteen hundred", "twenty twenty")
// into the cardinal words Canon gives the digits.
func yearPairsToCardinal(toks []string) []string {
	var out []string
	for i := 0; i < len(toks); {
		if v, n := yearPairAt(toks, i); n > 0 {
			out = append(out, cardinal(uint64(v))...)
			i += n
			continue
		}
		out = append(out, toks[i])
		i++
	}
	return out
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
	if n > maxCardinal {
		return digitWordsHan(run)
	}
	return cardinal(n)
}

// digitWordsHan is the fallback for out-of-range runs: each rune read alone.
func digitWordsHan(run []rune) []string {
	var w []string
	for _, r := range run {
		if d, ok := hanDigits[r]; ok {
			w = append(w, cardinal(d)...)
		} else {
			w = append(w, cardinal(hanUnits[r])...)
		}
	}
	return w
}
