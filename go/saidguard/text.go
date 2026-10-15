package saidguard

import (
	"strings"
	"unicode"
)

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

func isHanNumeral(r rune) bool {
	_, d := svHanDigit[r]
	_, u := svHanBigUnit[r]
	_, s := svHanSmallUnit[r]
	return d || u || s
}

var svHanBigUnit = map[rune]uint64{'万': 1e4, '亿': 1e8}
