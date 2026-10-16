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
	"strings"
)

// Version is the said guard's revision (spec/said-guard.md), raised whenever a change makes the guard refuse a
// reading it accepted, as Swift's saidGuardVersion is. A cache of audio accepted by the guard keys on it.
const Version = 12

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
