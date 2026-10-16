package codexauth

import (
	"math"
	"strings"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// newToken registers bearer with the scrubber and wraps it with its expiry: the JWT exp claim, read without checking
// the signature. A token that is not a JWT, or has no exp, has unknown expiry (the zero time).
func newToken(bearer, source string) secret.Token {
	return secret.NewToken(bearer, jwtExpiry(bearer), "", source)
}

// jwtExpiry reads the exp claim in place from the payload, decoded into a buffer cleared before it is dropped: the
// token is kept, but no other copy of what it holds is.
func jwtExpiry(tok string) time.Time {
	first := strings.IndexByte(tok, '.')
	if first < 0 {
		return time.Time{}
	}
	rest := tok[first+1:]
	second := strings.IndexByte(rest, '.')
	if second < 0 || strings.IndexByte(rest[second+1:], '.') >= 0 {
		return time.Time{}
	}
	// The base64url alphabet only, unpadded (RFC 7515, and Codex's own decoder): the decoder would skip a CR or LF,
	// and trimming "=" would read "eyJleHAiOjF9====" as exp 1.
	payload64 := rest[:second]
	for i := 0; i < len(payload64); i++ {
		if base64URL(payload64[i]) < 0 {
			return time.Time{}
		}
	}
	if len(payload64)%4 == 1 {
		return time.Time{}
	}
	payload := make([]byte, len(payload64)*3/4)
	defer clear(payload)
	var acc uint32
	bits, k := 0, 0
	for i := 0; i < len(payload64); i++ {
		acc = acc<<6 | uint32(base64URL(payload64[i]))
		if bits += 6; bits >= 8 {
			bits -= 8
			payload[k] = byte(acc >> bits)
			k++
		}
		acc &= 1<<bits - 1
	}
	// The bits left over are ignored, as base64.RawURLEncoding and Foundation ignore them.
	if !wire.ValidCodex(payload) {
		return time.Time{}
	}
	// Exact key: a struct field would also match "EXP", which the Swift implementation does not read.
	raw, ok := wire.Member(payload, wire.Root(payload), "exp")
	if !ok {
		return time.Time{}
	}
	exp, ok := wire.Int64Of(payload, raw) // not an int64 (null, a fraction, an overflow)
	if !ok {
		return time.Time{}
	}
	// time.Time counts seconds from year 1 in an int64, so a Unix time beyond maxUnix would wrap into the past; such
	// an expiry is clamped to the last instant it can hold. Every smaller exp is kept as given, as in Swift.
	exp = min(exp, maxUnix)
	t := time.Unix(exp, 0)
	if t.IsZero() { // exp names year 1 exactly, which is the zero time, "unknown"; it is expired
		t = t.Add(time.Nanosecond)
	}
	return t
}

// maxUnix is the largest Unix time time.Time holds: its seconds count from year 1, 62,135,596,800 s before 1970.
const maxUnix = math.MaxInt64 - 62_135_596_800

// base64URL is a base64url digit's value, or -1.
func base64URL(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '-':
		return 62
	case c == '_':
		return 63
	}
	return -1
}
