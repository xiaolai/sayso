package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaolai/sayso/go/codexapp"
	"github.com/xiaolai/sayso/go/secret"
)

// newToken registers bearer with the scrubber and wraps it with its expiry: the JWT exp claim, read without checking
// the signature. A token that is not a JWT, or has no exp, has unknown expiry (the zero time).
func newToken(bearer, source string) secret.Token {
	return secret.NewToken(bearer, jwtExpiry(bearer), "", source)
}

func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	// The base64url alphabet only: the decoder would skip a CR or LF, which the Swift implementation refuses.
	payload64 := strings.TrimRight(parts[1], "=")
	if strings.IndexFunc(payload64, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(payload64)
	if err != nil {
		return time.Time{}
	}
	// Exact key: a struct field would also match "EXP", which the Swift implementation does not read.
	var claims map[string]json.RawMessage
	var exp int64
	if !codexapp.ValidJSON(payload) || json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}
	raw, ok := claims["exp"]
	if !ok || string(raw) == "null" || json.Unmarshal(raw, &exp) != nil { // not an int64 (a fraction, an overflow)
		return time.Time{}
	}
	// time.Time counts seconds from year 1 in an int64, so a Unix time this large would wrap into the past; any
	// such expiry is far in the future, so it is clamped there.
	exp = min(exp, maxUnix)
	t := time.Unix(exp, 0)
	if t.IsZero() { // exp names year 1 exactly, which is the zero time, "unknown"; it is expired
		t = t.Add(time.Nanosecond)
	}
	return t
}

// maxUnix is a Unix time time.Time can hold without wrapping: about 146 billion years from now.
const maxUnix = 1 << 62
