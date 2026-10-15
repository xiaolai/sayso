// Package secret holds the process-wide scrubber and the two secret types, a sign-in bearer and a minted realtime
// client secret, whose every formatting path prints Redacted (spec/credentials.md, hygiene).
package secret

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Redacted replaces every secret Scrub removes.
const Redacted = "<redacted>"

// patterns match bearer JWTs and ephemeral client secrets whatever their source.
var patterns = regexp.MustCompile(`eyJ[\w-]+\.[\w-]+\.[\w-]+|ek_[\w-]+`)

// registry holds every secret the process has seen, so opaque formats that no pattern matches are removed too. It
// only grows: a secret seen once stays secret.
var registry struct {
	mu      sync.RWMutex
	secrets map[string]struct{}
	ordered []string // longest first, so a secret containing another is removed whole
}

// Register adds s to the process-wide scrub list. Empty strings are ignored.
func Register(s string) {
	if s == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.secrets == nil {
		registry.secrets = map[string]struct{}{}
	}
	if _, ok := registry.secrets[s]; ok {
		return
	}
	registry.secrets[s] = struct{}{}
	registry.ordered = append(registry.ordered, s)
	sort.SliceStable(registry.ordered, func(i, j int) bool {
		return len(registry.ordered[i]) > len(registry.ordered[j])
	})
}

// Scrub removes every registered secret and every secret-shaped pattern from s. Use it on any text that may reach a
// user, a log or an artifact, such as error messages.
func Scrub(s string) string {
	registry.mu.RLock()
	for _, sec := range registry.ordered {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	registry.mu.RUnlock()
	return patterns.ReplaceAllString(s, Redacted)
}

// Token is a sign-in bearer with its metadata. The bearer is unexported and reachable only through Bearer, so no
// formatting or encoding path can print it.
type Token struct {
	bearer string
	// Expires is the JWT expiry; the zero time means unknown (a token that is not a JWT, or has no exp).
	Expires time.Time
	Plan    string
	Source  string
}

// NewToken returns a Token and registers its bearer with the scrubber.
func NewToken(bearer string, expires time.Time, plan, source string) Token {
	Register(bearer)
	return Token{bearer: bearer, Expires: expires, Plan: plan, Source: source}
}

// Bearer returns the secret. Call it only to build an Authorization header.
func (t Token) Bearer() string { return t.bearer }

// IsZero reports whether t holds no bearer.
func (t Token) IsZero() bool { return t.bearer == "" }

func (Token) String() string               { return Redacted }
func (Token) GoString() string             { return Redacted }
func (Token) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// ClientSecret is a minted ephemeral `ek_` realtime secret.
type ClientSecret struct {
	value     string
	ExpiresAt time.Time
}

// NewClientSecret returns a ClientSecret and registers it with the scrubber.
func NewClientSecret(value string, expiresAt time.Time) ClientSecret {
	Register(value)
	return ClientSecret{value: value, ExpiresAt: expiresAt}
}

// Bearer returns the secret. Call it only to build an Authorization header.
func (s ClientSecret) Bearer() string { return s.value }

func (ClientSecret) String() string               { return Redacted }
func (ClientSecret) GoString() string             { return Redacted }
func (ClientSecret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (ClientSecret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (ClientSecret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
