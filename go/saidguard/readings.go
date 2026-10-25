package saidguard

import (
	"strconv"
	"strings"

	"github.com/xiaolai/sayso/go/numwords"
)

type svAlt []string // tokens acceptable at one position
type svSeq []svAlt  // one reading
type svUnit []svSeq // the alternative readings of one script unit

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
		if s := lit(sign + d + suffix); !svUnitHas(unit, s) { // "42" and "1,000" are already the reading as written
			unit = append(unit, s)
		}
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
var svHanBigUnit = map[rune]uint64{'万': 1e4, '亿': 1e8}

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
	if digits, number := svHanDigitString(rs); digits {
		// A digit string (二〇二四). One that is no number (svHanDigitString) has no reading in digits.
		if !number {
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
