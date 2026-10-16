package saidguard

import (
	"slices"
	"strings"
	"unicode"

	"github.com/xiaolai/sayso/go/numwords"
)

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
			// A sign before a Han numeral, touching it or not, is the number's sign: "-五" = "- 五" = "-5" = "- 5".
			if r.dash == svDashSign && k+1 < len(raws) && raws[k+1].kind == svHanRun && !raws[k+1].punctBefore() {
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
			// A sign reads as written or, in English, "minus" or "negative", never "to"; a range dash reads "to".
			unit := svSymbolUnit(r.text, english)
			if r.dash == svDashSign {
				unit = svSignUnit(english)
			}
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
		lead, trail int
	}
	var words []part
	marked := false
	for _, r := range span {
		if r.kind != svWord {
			continue
		}
		parts := strings.FieldsFunc(svNormApostrophes(svLower(r.text, language)), svJoiner)
		for i, p := range parts {
			lead, trail := 0, 0
			if i == 0 {
				lead = r.optLead
			}
			if i == len(parts)-1 {
				trail = r.optTrail
			}
			words = append(words, part{p, lead, trail})
		}
		marked = marked || r.optLead > 0 || r.optTrail > 0
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
				ns[0] = svWithMarks(ns[0], first.lead, 0)
				ns[len(ns)-1] = svWithMarks(ns[len(ns)-1], 0, last.trail)
			}
			out[i] = ns
		}
		units[u] = out
		at += n
	}
}

// svWithMarks is a new alt holding each token of alt in all its svMarkReadings.
func svWithMarks(alt svAlt, lead, trail int) svAlt {
	if lead == 0 && trail == 0 {
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

// svSignUnit reads a sign standing alone ("-x", "-五"): either minus, written, or its English names.
func svSignUnit(english bool) svUnit {
	unit := svUnit{lit("-"), lit("−")}
	if english {
		unit = append(unit, lit("minus"), lit("negative"))
	}
	return unit
}

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
