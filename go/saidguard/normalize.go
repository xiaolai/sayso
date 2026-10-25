package saidguard

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// --- normalisation shared by both sides ---

// svMaxMarkRun bounds a run of combining marks. Normalisers differ beyond 30 (Go's inserts U+034F to keep the text
// stream-safe, Foundation's does not), so a side with a longer run never matches; Foundation's full lowercase mapping
// can add one mark (İ → i̇; svLower's simple mapping gives i), hence 29 in both implementations.
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
	return norm.NFKC.String(preNormalize(norm.NFKC.String(s)))
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

// preNormalize: format characters, enclosing marks and variation selectors go; a zero width space is a word break;
// every Unicode decimal digit becomes ASCII at scalar level; ⁄ becomes /. The minus sign − stays: it is always
// spoken (a sign or an operator), where "-" may also join words, so folding it into "-" let "x−y" read as "xy".
func preNormalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\u200B': // zero width space marks a word break (UAX #14 ZW); deleting it glued "now"+"here"
			b.WriteByte(' ')
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Variation_Selector, r):
		case r == '⁄':
			b.WriteByte('/')
		case unicode.Is(unicode.Nd, r) && (r < '0' || r > '9'):
			b.WriteRune('0' + rune(decimalValue(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decimalValue: Unicode lays out decimal digits in runs of ten starting at zero; adjacent runs (the mathematical
// digits) repeat, hence the modulo.
func decimalValue(r rune) int {
	z := r
	for unicode.Is(unicode.Nd, z-1) {
		z--
	}
	return int(r-z) % 10
}

var unspacedScripts = []*unicode.RangeTable{unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar}

// isUnspaced: a script written without spaces between words, so each character is its own token.
func isUnspaced(r rune) bool { return unicode.In(r, unspacedScripts...) }

// baseLanguage is the primary subtag of a BCP 47 tag, lower case ("zh-Hans" → "zh").
func baseLanguage(tag string) string {
	tag = strings.ToLower(tag)
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func isApostrophe(r rune) bool { return r == '\'' || r == '’' || r == 'ʼ' || r == '‘' }
