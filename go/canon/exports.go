package canon

import "strings"

// The helpers below are the canonicaliser's own rules, exported so a scorer built on Canon reads text the same way.

// YearPairAt reports the year read as a pair at toks[i] ("nineteen ninety-nine" = 1999): its value and the number of
// tokens it spans, or n = 0 when toks[i] starts no year pair. toks are Canon tokens.
func YearPairAt(toks []string, i int) (v, n int) { return yearPairAt(toks, i) }

// Cardinal spells n as Canon spells a number: English cardinal words.
func Cardinal(n uint64) []string { return cardinal(n) }

// IsNumberWord reports whether w is an English number word below a hundred ("seven", "twenty").
func IsNumberWord(w string) bool { return isNumberWord(w) }

// IsScale reports whether w is a scale word ("hundred", "thousand", "million", ...).
func IsScale(w string) bool { return isScale(w) }

// IsHanNumeral reports whether r is a Chinese numeral character Canon reads as a number.
func IsHanNumeral(r rune) bool { return isHanNumeral(r) }

// IsHyphen reports whether r is a hyphen or dash Canon treats as a word break.
func IsHyphen(r rune) bool { return isHyphen(r) }

// IsApostrophe reports whether r is an apostrophe Canon removes inside a word.
func IsApostrophe(r rune) bool { return isApostrophe(r) }

// IsAbbreviation reports whether word, in any case, is an abbreviation Canon may expand ("Mr", "St", "vs").
func IsAbbreviation(word string) bool {
	lw := strings.ToLower(word)
	return abbreviations[lw] != "" || lw == "st"
}
