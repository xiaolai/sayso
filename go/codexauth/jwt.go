package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

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
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}
	}
	return time.Unix(*claims.Exp, 0)
}
