package codexauth

import (
	"math"
	"strings"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"
	"github.com/xiaolai/sayso/go/secret"
)

// newToken registers bearer with the scrubber and wraps it with its expiry and account: the JWT exp claim, read without
// checking the signature, and the account the token names (accountClaim). A token that is not a JWT, or has no exp,
// has unknown expiry (the zero time).
func newToken(bearer, source string) secret.Token {
	t := secret.NewToken(bearer, jwtExpiry(bearer), "", source)
	t.AccountID, _ = accountClaim(bearer)
	return t
}

// jwtExpiry reads the exp claim in place from the payload, decoded into a buffer cleared before it is dropped: the
// token is kept, but no other copy of what it holds is.
func jwtExpiry(tok string) time.Time {
	var t time.Time
	readJWTPayload(tok, func(payload []byte) {
		// Exact key: a struct field would also match "EXP", which the Swift implementation does not read.
		raw, ok := wire.Member(payload, wire.Root(payload), "exp")
		if !ok {
			return
		}
		exp, ok := wire.Int64Of(payload, raw) // not an int64 (null, a fraction, an overflow)
		if !ok {
			return
		}
		// time.Time counts seconds from year 1 in an int64, so a Unix time beyond maxUnix would wrap into the past;
		// such an expiry is clamped to the last instant it can hold. Every smaller exp is kept as given, as in Swift.
		t = time.Unix(min(exp, maxUnix), 0)
		if t.IsZero() { // exp names year 1 exactly, which is the zero time, "unknown"; it is expired
			t = t.Add(time.Nanosecond)
		}
	})
	return t
}

// readJWTPayload decodes tok's payload into a buffer cleared before it is dropped and, when the payload is JSON as
// Codex's is read, passes it to read, which must not keep it; false when tok is not a JWT or the payload is not such
// JSON.
func readJWTPayload(tok string, read func(payload []byte)) bool {
	first := strings.IndexByte(tok, '.')
	if first < 0 {
		return false
	}
	rest := tok[first+1:]
	second := strings.IndexByte(rest, '.')
	if second < 0 || strings.IndexByte(rest[second+1:], '.') >= 0 {
		return false
	}
	// The base64url alphabet only, unpadded (RFC 7515, and Codex's own decoder): the decoder would skip a CR or LF,
	// and trimming "=" would read "eyJleHAiOjF9====" as exp 1.
	payload64 := rest[:second]
	for i := 0; i < len(payload64); i++ {
		if base64URL(payload64[i]) < 0 {
			return false
		}
	}
	if len(payload64)%4 == 1 {
		return false
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
		return false
	}
	read(payload)
	return true
}

// maxAccountID is the longest account ID a private sign-in names.
const maxAccountID = 256

// accountClaim is the ChatGPT account tok names: chatgpt_account_id in its "https://api.openai.com/auth" claim
// (spec/credentials.md, AUTH-08). False when tok is not a JWT, or the claim is absent or not a string of 1 to 256
// ASCII letters, digits, '-', '_' and '.'. Names are exact, as for exp.
func accountClaim(tok string) (string, bool) {
	var id string
	readJWTPayload(tok, func(payload []byte) {
		auth, ok := wire.Member(payload, wire.Root(payload), "https://api.openai.com/auth")
		if !ok {
			return
		}
		v, ok := wire.Member(payload, auth, "chatgpt_account_id")
		if !ok || wire.KindOf(payload, v) != '"' {
			return
		}
		// A decoded string is never longer than its JSON text, so the buffer never grows.
		buf := make([]byte, 0, v.End-v.Start)
		buf, _ = wire.AppendString(buf, payload, v)
		if len(buf) == 0 || len(buf) > maxAccountID {
			return
		}
		for _, c := range buf {
			if base64URL(c) < 0 && c != '.' {
				return
			}
		}
		id = string(buf)
	})
	return id, id != ""
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
