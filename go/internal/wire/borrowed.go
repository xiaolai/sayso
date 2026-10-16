package wire

import (
	"math/bits"
	"math/rand/v2"
)

// This file reads a validated document where it lies (AUTH-02): lookup and comparison read the caller's bytes in
// place and copy none of them, so a value the caller does not keep never leaves the caller's buffer, which the caller
// clears. AppendString makes the one copy, of a value the caller keeps, into a buffer the caller provides.

// Span is a value's place in a document: its bytes are doc[Start:End].
type Span struct{ Start, End int }

// Root is the root value of the validated document doc.
func Root(doc []byte) Span {
	s := skipSpace(doc, 0)
	return Span{s, skipValue(doc, s)}
}

// KindOf is the kind of the value at v: '{', '[', '"', '0' (a number), 't', 'f' or 'n'.
func KindOf(doc []byte, v Span) byte {
	if c := doc[v.Start]; c == '-' || c >= '0' && c <= '9' {
		return '0'
	}
	return doc[v.Start]
}

// Member is the value of the member name of the object at obj; false when it has none, or is no object. Names match
// as decoded bytes.
func Member(doc []byte, obj Span, name string) (Span, bool) {
	if KindOf(doc, obj) != '{' {
		return Span{}, false
	}
	i := skipSpace(doc, obj.Start+1)
	for doc[i] != '}' {
		at := i
		i = skipSpace(doc, skipString(doc, i)) + 1 // the colon
		start := skipSpace(doc, i)
		end := skipValue(doc, start)
		if unescapedEquals(doc, at, name) {
			return Span{start, end}, true
		}
		if i = skipSpace(doc, end); doc[i] == ',' {
			i = skipSpace(doc, i+1)
		}
	}
	return Span{}, false
}

// StringEquals reports whether the value at v is a string that decodes to exactly s.
func StringEquals(doc []byte, v Span, s string) bool {
	return KindOf(doc, v) == '"' && unescapedEquals(doc, v.Start, s)
}

// AppendString appends the decoded string at v to dst; false, with dst unchanged, for any other value. A dst with
// capacity v.End-v.Start never grows, so no copy of the string is left behind in a dropped array.
func AppendString(dst, doc []byte, v Span) ([]byte, bool) {
	if KindOf(doc, v) != '"' {
		return dst, false
	}
	u := unescaper{b: doc, i: v.Start + 1}
	for c, ok := u.next(); ok; c, ok = u.next() {
		dst = append(dst, c)
	}
	return dst, true
}

// Int64Of is the number at v written as an integer, without fraction or exponent, within int64, as encoding/json
// decodes one into an int64; false for any other value. Read digit by digit, in place.
func Int64Of(doc []byte, v Span) (int64, bool) {
	if KindOf(doc, v) != '0' {
		return 0, false
	}
	i := v.Start
	neg := doc[i] == '-'
	if neg {
		i++
	}
	var n int64 // accumulated negative, so the minimum fits
	for ; i < v.End; i++ {
		c := doc[i]
		if c < '0' || c > '9' {
			return 0, false // a fraction or an exponent
		}
		d := int64(c - '0')
		if n < (minInt64+d)/10 {
			return 0, false
		}
		n = n*10 - d
	}
	if neg {
		return n, true
	}
	if n == minInt64 {
		return 0, false
	}
	return -n, true
}

const minInt64 = -1 << 63

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// skipString is the end of the string whose opening quote is at i, after its closing quote. It copies nothing.
func skipString(b []byte, i int) int {
	j := i + 1
	for b[j] != '"' {
		if b[j] == '\\' {
			j += 2 // an escape's second byte is never a quote that ends it
		} else {
			j++
		}
	}
	return j + 1
}

// skipValue is the end of the validated value starting at i.
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; ; {
			switch b[j] {
			case '"':
				j = skipString(b, j)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
			j++
		}
	default:
		j := i
		for j < len(b) && !isSpace(b[j]) && b[j] != ',' && b[j] != '}' && b[j] != ']' {
			j++
		}
		return j
	}
}

// unescaper yields the decoded bytes of a validated string one at a time, read where the string lies: an escape's
// bytes are made in a register, so decoding or comparing a value copies none of it into memory.
type unescaper struct {
	b       []byte
	i       int
	pending uint32 // the rest of an escaped scalar's UTF-8 bytes, the next one highest
	n       int
}

func hex4(b []byte, i int) uint32 {
	var v uint32
	for _, c := range b[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		default:
			c -= 'A' - 10
		}
		v = v<<4 | uint32(c)
	}
	return v
}

func (u *unescaper) next() (byte, bool) {
	if u.n > 0 {
		u.n--
		return byte(u.pending >> (8 * u.n)), true
	}
	c := u.b[u.i]
	if c == '"' {
		return 0, false
	}
	if c != '\\' {
		u.i++
		return c, true
	}
	e := u.b[u.i+1]
	u.i += 2
	switch e {
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	case 'u':
		r := hex4(u.b, u.i)
		u.i += 4
		if r >= 0xD800 && r <= 0xDBFF { // validated: a low surrogate escape follows
			low := hex4(u.b, u.i+2)
			u.i += 6
			r = 0x10000 + (r-0xD800)<<10 + (low - 0xDC00)
		}
		switch {
		case r < 0x80:
			return byte(r), true
		case r < 0x800:
			u.pending, u.n = 0x80|r&0x3F, 1
			return byte(0xC0 | r>>6), true
		case r < 0x10000:
			u.pending, u.n = (0x80|r>>6&0x3F)<<8|(0x80|r&0x3F), 2
			return byte(0xE0 | r>>12), true
		default:
			u.pending, u.n = (0x80|r>>12&0x3F)<<16|(0x80|r>>6&0x3F)<<8|(0x80|r&0x3F), 3
			return byte(0xF0 | r>>18), true
		}
	default:
		return e, true // " \ /
	}
}

// unescapedEquals reports whether the string whose opening quote is at i decodes to exactly s.
func unescapedEquals(b []byte, i int, s string) bool {
	u := unescaper{b: b, i: i + 1}
	for k := 0; ; k++ {
		c, ok := u.next()
		if !ok {
			return k == len(s)
		}
		if k >= len(s) || s[k] != c {
			return false
		}
	}
}

// unescapedEqual reports whether the strings whose opening quotes are at x and y decode to the same bytes.
func unescapedEqual(b []byte, x, y int) bool {
	p, q := unescaper{b: b, i: x + 1}, unescaper{b: b, i: y + 1}
	for {
		c, ok := p.next()
		d, ok2 := q.next()
		if ok != ok2 || c != d {
			return false
		}
		if !ok {
			return true
		}
	}
}

// uniqueNames reports whether no object in the syntactically valid document b has two members whose names decode
// to the same bytes. Each object's names are kept as their places, by a seeded hash of their decoded bytes, and
// compared where they lie: nothing of a name is copied. The hash mixes one byte at a time, as maphash, which buffers
// what it is given, would not.
func uniqueNames(b []byte) bool {
	seed := rand.Uint64()
	type object map[uint64][]int
	var stack []object // nil for an array
	expectName := false
	for i := skipSpace(b, 0); i < len(b); {
		switch c := b[i]; {
		case c == '{':
			stack = append(stack, object{})
			expectName = true
			i++
		case c == '[':
			stack = append(stack, nil)
			i++
		case c == '}' || c == ']':
			stack = stack[:len(stack)-1]
			i++
		case c == ',':
			expectName = len(stack) > 0 && stack[len(stack)-1] != nil
			i++
		case c == '"':
			end := skipString(b, i)
			if expectName {
				key := seed
				u := unescaper{b: b, i: i + 1}
				for c, ok := u.next(); ok; c, ok = u.next() {
					key = bits.RotateLeft64((key^uint64(c))*0x9E3779B97F4A7C15, 29) * 0xBF58476D1CE4E5B9
				}
				names := stack[len(stack)-1]
				for _, at := range names[key] {
					if unescapedEqual(b, at, i) {
						return false
					}
				}
				names[key] = append(names[key], i)
				expectName = false
			}
			i = end
		default: // a colon, a literal or a number
			i++
		}
		i = skipSpace(b, i)
	}
	return true
}
