package codexapp

import (
	"encoding/json"

	"github.com/xiaolai/sayso/go/internal/wire"
)

// MaxJSONDepth is how deeply Codex's JSON (a reply, auth.json, a JWT payload) may nest. Codex's own documents nest a
// few levels; the bound is one both implementations' decoders accept (Foundation's stops at 512), so a deeper
// document is malformed in both rather than in one.
const MaxJSONDepth = 64

// ValidJSON reports whether b is JSON as Codex's is read (spec/credentials.md, messages): one strict value (valid
// UTF-8, no duplicate key; Codex's serde refuses both) nested at most MaxJSONDepth deep, with no number too large for
// a float64 (1e999). Foundation refuses such a number wherever it is, even in a field nobody reads, and Codex's serde
// refuses it too, so it makes the document malformed in both implementations.
func ValidJSON(b []byte) bool { return withinBounds(b, MaxJSONDepth) && wire.Valid(b) }

// withinBounds reports whether no array or object in b nests deeper than max and every number outside strings fits a
// float64. It runs before the strict check and reads b in place, a number's digits included: nothing is copied.
func withinBounds(b []byte, max int) bool {
	depth, inString, escaped := 0, false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case !inString && (c == '-' || c >= '0' && c <= '9'):
			j := i
			for j < len(b) && (b[j] == '-' || b[j] == '+' || b[j] == '.' || b[j] == 'e' || b[j] == 'E' || b[j] >= '0' && b[j] <= '9') {
				j++
			}
			if !finiteAsFloat64(b[i:j]) {
				return false
			}
			i = j - 1
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			if depth++; depth > max {
				return false
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return true
}

// Replies are read with exact key names. encoding/json matches struct fields case-insensitively, so a reply with
// "AUTHTOKEN" would be read here and ignored by the Swift implementation; a map lookup is exact in both.

// object decodes raw, a JSON object or null (an empty object), with exact keys.
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// optional returns key of the object raw decoded as T; nil when the key is absent or null. A value of another type
// is an error.
func optional[T any](raw json.RawMessage, key string) (*T, error) {
	m, err := object(raw)
	if err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok || string(v) == "null" {
		return nil, nil
	}
	var out T
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// field is optional with the zero value for an absent or null key.
func field[T any](raw json.RawMessage, key string) (T, error) {
	var zero T
	p, err := optional[T](raw, key)
	if err != nil || p == nil {
		return zero, err
	}
	return *p, nil
}

// overflowDigits are the digits of 2^1024 - 2^970, halfway between the largest finite float64 and 2^1024: a number
// at least this large rounds, to even, up to 2^1024, which is infinite.
const overflowDigits = "179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792"

// finiteAsFloat64 reports whether the JSON number n is finite as a correctly rounded float64 (as ParseFloat and
// Foundation read it), decided from its digits without converting it, so nothing of it is copied. Text that is no
// JSON number counts as finite: it is the strict check's to refuse.
func finiteAsFloat64(n []byte) bool {
	i := 0
	if i < len(n) && n[i] == '-' {
		i++
	}
	digits := func() (int, int) {
		s := i
		for i < len(n) && n[i] >= '0' && n[i] <= '9' {
			i++
		}
		return s, i
	}
	ws, we := digits()
	if ws == we {
		return true
	}
	fs, fe := i, i
	if i < len(n) && n[i] == '.' {
		i++
		if fs, fe = digits(); fs == fe {
			return true
		}
	}
	exponent := 0
	if i < len(n) && (n[i] == 'e' || n[i] == 'E') {
		i++
		neg := false
		if i < len(n) && (n[i] == '+' || n[i] == '-') {
			neg = n[i] == '-'
			i++
		}
		es, ee := digits()
		if es == ee {
			return true
		}
		for _, c := range n[es:ee] {
			exponent = min(exponent*10+int(c-'0'), 1_000_000_000) // saturated
		}
		if neg {
			exponent = -exponent
		}
	}
	if i != len(n) {
		return true
	}
	// The significant digits run through the whole part then the fraction; the value is 0.d1d2... × 10^point.
	whole, all := we-ws, (we-ws)+(fe-fs)
	digit := func(k int) byte {
		if k < whole {
			return n[ws+k]
		}
		return n[fs+k-whole]
	}
	first := 0
	for first < all && digit(first) == '0' {
		first++
	}
	if first == all {
		return true // zero
	}
	if point := whole - first + exponent; point != len(overflowDigits) {
		return point < len(overflowDigits)
	}
	for k := 0; k < len(overflowDigits); k++ {
		d := byte('0')
		if first+k < all {
			d = digit(first + k)
		}
		if d != overflowDigits[k] {
			return d < overflowDigits[k]
		}
	}
	return false // at least the halfway point
}
