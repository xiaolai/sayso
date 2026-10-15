// Package numwords spells numbers as English words, for the said guard's
// readings. It comes from recite (github.com/xiaolai/recite), where the
// canonicaliser and the spoken-form rules share it.
package numwords

import "strings"

var (
	ones = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen",
		"eighteen", "nineteen"}
	tens   = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	scales = []struct {
		v    uint64
		name string
	}{{1e12, "trillion"}, {1e9, "billion"}, {1e6, "million"}, {1e3, "thousand"}}
)

// MaxCardinal bounds numbers read as cardinals; longer digit strings are read
// digit by digit, as are strings with a leading zero ("007").
const MaxCardinal = 999_999_999_999_999

// Cardinal spells n as words, one word per token: 42 is "forty", "two".
func Cardinal(n uint64) []string {
	if n < 20 {
		return []string{ones[n]}
	}
	if n < 100 {
		w := []string{tens[n/10]}
		if n%10 != 0 {
			w = append(w, ones[n%10])
		}
		return w
	}
	if n < 1000 {
		w := []string{ones[n/100], "hundred"}
		if n%100 != 0 {
			w = append(w, Cardinal(n%100)...)
		}
		return w
	}
	for _, sc := range scales {
		if n >= sc.v {
			w := append(Cardinal(n/sc.v), sc.name)
			if n%sc.v != 0 {
				w = append(w, Cardinal(n%sc.v)...)
			}
			return w
		}
	}
	panic("unreachable")
}

// Digits reads an ASCII digit string one digit at a time.
func Digits(ds string) []string {
	w := make([]string, 0, len(ds))
	for _, d := range ds {
		w = append(w, ones[d-'0'])
	}
	return w
}

// Integer reads an ASCII digit string as a cardinal, or digit by digit when it
// has a leading zero or exceeds MaxCardinal.
func Integer(ds string) []string {
	if len(ds) > 1 && ds[0] == '0' || len(ds) > 15 {
		return Digits(ds)
	}
	var n uint64
	for _, d := range ds {
		n = n*10 + uint64(d-'0')
	}
	if n > MaxCardinal {
		return Digits(ds)
	}
	return Cardinal(n)
}

var ordinalIrregular = map[string]string{
	"one": "first", "two": "second", "three": "third", "five": "fifth", "eight": "eighth",
	"nine": "ninth", "twelve": "twelfth",
}

// Ordinal turns a cardinal reading into its ordinal: "forty", "two" becomes
// "forty", "second".
func Ordinal(words []string) []string {
	last := words[len(words)-1]
	switch {
	case ordinalIrregular[last] != "":
		last = ordinalIrregular[last]
	case strings.HasSuffix(last, "y"):
		last = last[:len(last)-1] + "ieth"
	default:
		last += "th"
	}
	out := append([]string(nil), words[:len(words)-1]...)
	return append(out, last)
}

// SmallValue returns the value of a word in zero..nineteen or a tens word.
func SmallValue(w string) (int, bool) {
	for i, o := range ones {
		if o == w {
			return i, true
		}
	}
	for i, t := range tens {
		if t != "" && t == w {
			return i * 10, true
		}
	}
	return 0, false
}

var unitOrdinals = map[string]bool{"first": true, "second": true, "third": true, "fourth": true,
	"fifth": true, "sixth": true, "seventh": true, "eighth": true, "ninth": true,
	"seconds": true, "thirds": true, "fourths": true, "fifths": true, "sixths": true, "sevenths": true,
	"eighths": true, "ninths": true}

// Join joins number words with spaces, hyphenating a tens word and the unit
// (or unit ordinal) after it, as written English does: "forty-two",
// "twenty-first", "thirty-seconds".
func Join(words []string) string {
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			if v, ok := SmallValue(words[i-1]); ok && v >= 20 && v%10 == 0 {
				if u, ok := SmallValue(w); ok && u >= 1 && u <= 9 || unitOrdinals[w] {
					b.WriteByte('-')
					b.WriteString(w)
					continue
				}
			}
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	return b.String()
}
