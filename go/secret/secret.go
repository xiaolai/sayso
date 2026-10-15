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
	ordered []string
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
}

// Scrub removes every registered secret and every secret-shaped pattern from s. Use it on any text that may reach a
// user, a log or an artifact, such as error messages. Every match is found in the original text and the union of
// the matched spans is replaced, overlapping or touching spans as one, so a registered secret inside a longer
// secret-shaped value never leaves the rest of that value behind.
func Scrub(s string) string {
	var spans [][2]int
	registry.mu.RLock()
	for _, sec := range registry.ordered {
		for i := 0; i+len(sec) <= len(s); {
			j := strings.Index(s[i:], sec)
			if j < 0 {
				break
			}
			spans = append(spans, [2]int{i + j, i + j + len(sec)})
			i += j + 1 // overlapping occurrences too
		}
	}
	registry.mu.RUnlock()
	for _, m := range patterns.FindAllStringIndex(s, -1) {
		spans = append(spans, [2]int{m[0], m[1]})
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		span := spans[i]
		for i++; i < len(spans) && spans[i][0] <= span[1]; i++ {
			span[1] = max(span[1], spans[i][1])
		}
		b.WriteString(s[at:span[0]])
		b.WriteString(Redacted)
		at = span[1]
	}
	b.WriteString(s[at:])
	return b.String()
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
