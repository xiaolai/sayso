// Package secret holds the process-wide scrubber and the two secret types, a sign-in bearer and a minted realtime
// client secret, whose every formatting path prints Redacted (spec/credentials.md, hygiene).
package secret

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Redacted replaces every secret Scrub removes.
const Redacted = "<redacted>"

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
// user, a log or an artifact, such as error messages. Every match is found in the original text, at every starting
// position, and the union of the matched spans is replaced, overlapping or touching spans as one: a registered secret
// inside a longer secret-shaped value never leaves the rest of that value behind, and a JWT starting inside an ek_
// match ("ek_eyJa.b.c") is removed whole. The scan is linear in the text and in each secret.
func Scrub(s string) string {
	var spans [][2]int
	registry.mu.RLock()
	for _, sec := range registry.ordered {
		spans = append(spans, occurrences(sec, s)...)
	}
	registry.mu.RUnlock()
	spans = append(spans, patternSpans(s)...)
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

// occurrences returns every occurrence of needle in s, overlapping ones included, in O(len(s) + len(needle))
// (Knuth-Morris-Pratt): repeating strings.Index after each match is quadratic for a long, repetitive secret.
func occurrences(needle, s string) [][2]int {
	m := len(needle)
	if m == 0 || m > len(s) {
		return nil
	}
	fail := make([]int, m) // fail[q]: the longest proper border of needle[:q+1]
	for q, k := 1, 0; q < m; q++ {
		for k > 0 && needle[q] != needle[k] {
			k = fail[k-1]
		}
		if needle[q] == needle[k] {
			k++
		}
		fail[q] = k
	}
	var found [][2]int
	for i, k := 0, 0; i < len(s); i++ {
		for k > 0 && s[i] != needle[k] {
			k = fail[k-1]
		}
		if s[i] == needle[k] {
			k++
		}
		if k == m {
			found = append(found, [2]int{i + 1 - m, i + 1})
			k = fail[k-1]
		}
	}
	return found
}

// patternSpans returns the matches of `eyJ[\w-]+\.[\w-]+\.[\w-]+|ek_[\w-]+` starting at every position. [\w-]
// excludes ".", so each run is maximal and a match is decided in constant time from the precomputed run ends. The
// Swift implementation runs the same scan.
func patternSpans(s string) [][2]int {
	isWord := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-'
	}
	n := len(s)
	runEnd := make([]int, n+1) // runEnd[i]: the end of the word run starting at i
	runEnd[n] = n
	for i := n - 1; i >= 0; i-- {
		if isWord(s[i]) {
			runEnd[i] = runEnd[i+1]
		} else {
			runEnd[i] = i
		}
	}
	var spans [][2]int
	for i := 0; i+3 <= n; i++ {
		switch s[i : i+3] {
		case "eyJ": // then three dot-separated non-empty runs
			a := runEnd[i+3]
			if a > i+3 && a < n && s[a] == '.' {
				if b := runEnd[a+1]; b > a+1 && b < n && s[b] == '.' {
					if c := runEnd[b+1]; c > b+1 {
						spans = append(spans, [2]int{i, c})
					}
				}
			}
		case "ek_":
			if e := runEnd[i+3]; e > i+3 {
				spans = append(spans, [2]int{i, e})
			}
		}
	}
	return spans
}

// sealed is an opaque handle to a secret held in the vault. Nothing reachable from a Token or ClientSecret holds the
// secret itself, so no formatting or reflection path through one can print it: fmt, a reflect.Value of the field,
// or a dumper that walks unexported fields finds only a number. The vault entry is dropped once the last copy of
// the handle is unreachable.
type sealed struct{ id uint64 }

var (
	vault   sync.Map // uint64 -> string
	vaultID atomic.Uint64
)

func seal(s string) *sealed {
	p := &sealed{id: vaultID.Add(1)}
	vault.Store(p.id, s)
	runtime.AddCleanup(p, func(id uint64) { vault.Delete(id) }, p.id)
	return p
}

func (p *sealed) get() string {
	if p == nil {
		return ""
	}
	v, _ := vault.Load(p.id)
	runtime.KeepAlive(p) // p must outlive the load, or its cleanup could empty the entry first
	s, _ := v.(string)
	return s
}

// Token is a sign-in bearer with its metadata. The bearer is sealed and reachable only through Bearer, so no
// formatting or encoding path can print it. Compare tokens by Bearer, not with ==.
type Token struct {
	bearer *sealed
	// Expires is the JWT expiry; the zero time means unknown (a token that is not a JWT, or has no exp).
	Expires time.Time
	Plan    string
	Source  string
}

// NewToken returns a Token and registers its bearer with the scrubber.
func NewToken(bearer string, expires time.Time, plan, source string) Token {
	Register(bearer)
	return Token{bearer: seal(bearer), Expires: expires, Plan: plan, Source: source}
}

// Bearer returns the secret. Call it only to build an Authorization header.
func (t Token) Bearer() string { return t.bearer.get() }

// IsZero reports whether t holds no bearer.
func (t Token) IsZero() bool { return t.bearer.get() == "" }

func (Token) String() string               { return Redacted }
func (Token) GoString() string             { return Redacted }
func (Token) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (Token) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (Token) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// ClientSecret is a minted ephemeral `ek_` realtime secret.
type ClientSecret struct {
	value *sealed
	// ExpiresAt is the expiry as a time.Time, which holds a Unix time only within about ±2⁶² s: beyond that it is
	// clamped, keeping its side of now. ExpiresAtUnix keeps the exact value.
	ExpiresAt time.Time
	// ExpiresAtUnix is the expiry in Unix seconds exactly as minted (expires_at), as Swift keeps it.
	ExpiresAtUnix int64
}

// NewClientSecret returns a ClientSecret and registers it with the scrubber. ExpiresAtUnix is expiresAt.Unix().
func NewClientSecret(value string, expiresAt time.Time) ClientSecret {
	Register(value)
	return ClientSecret{value: seal(value), ExpiresAt: expiresAt, ExpiresAtUnix: expiresAt.Unix()}
}

// NewClientSecretUnix returns a ClientSecret that keeps expiresAtUnix exactly, and registers it with the scrubber.
// ExpiresAt is expiresAtUnix clamped to ±2⁶² s, which a time.Time holds without wrapping.
func NewClientSecretUnix(value string, expiresAtUnix int64) ClientSecret {
	Register(value)
	return ClientSecret{value: seal(value), ExpiresAt: time.Unix(max(min(expiresAtUnix, maxUnix), -maxUnix), 0),
		ExpiresAtUnix: expiresAtUnix}
}

// maxUnix is a Unix time time.Time holds without wrapping: about 146 billion years either side of 1970.
const maxUnix = 1 << 62

// Bearer returns the secret. Call it only to build an Authorization header.
func (s ClientSecret) Bearer() string { return s.value.get() }

func (ClientSecret) String() string               { return Redacted }
func (ClientSecret) GoString() string             { return Redacted }
func (ClientSecret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(Redacted)) }
func (ClientSecret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (ClientSecret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
