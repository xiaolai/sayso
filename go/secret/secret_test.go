package secret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSecretsNeverFormat(t *testing.T) {
	tok := NewToken("eyJcanary.canary.canary", time.Unix(0, 0), "plus", "codex")
	cs := NewClientSecret("ek_canary", time.Unix(0, 0))
	for _, v := range []any{tok, &tok, cs, &cs, []any{tok, cs}, struct{ T Token }{tok}} {
		out := fmt.Sprintf("%v %+v %#v %s %q %x %d", v, v, v, v, v, v, v)
		js, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		for _, s := range []string{out, string(js)} {
			if strings.Contains(s, "canary") {
				t.Errorf("%T leaked: %s", v, s)
			}
			if !strings.Contains(s, "redacted") { // json.Marshal escapes < and >
				t.Errorf("%T: no redaction marker in %s", v, s)
			}
		}
	}
	if tok.Bearer() != "eyJcanary.canary.canary" || cs.Bearer() != "ek_canary" {
		t.Fatal("Bearer() must return the secret")
	}
}

func TestConstructorsRegisterOpaqueSecrets(t *testing.T) {
	_ = NewToken("opaque-canary-token-77", time.Time{}, "", "")
	_ = NewClientSecret("opaque-canary-secret-88", time.Time{})
	if got := Scrub("a opaque-canary-token-77 b opaque-canary-secret-88"); got != "a <redacted> b <redacted>" {
		t.Fatalf("Scrub = %q", got)
	}
}

func TestLongestSecretFirst(t *testing.T) {
	Register("abc-secret")
	Register("abc-secret-longer")
	if got := Scrub("x abc-secret-longer y"); got != "x <redacted> y" {
		t.Fatalf("Scrub = %q", got)
	}
}

func TestPatterns(t *testing.T) {
	if got := Scrub("Bearer eyJa.eyJb.c and ek_live_1"); got != "Bearer <redacted> and <redacted>" {
		t.Fatalf("Scrub = %q", got)
	}
}

// A registered secret inside a longer secret-shaped value removes the whole value, not just the registered part.
func TestRegisteredInsidePattern(t *testing.T) {
	Register("ek_demo")
	if got := Scrub("x ek_demo_sensitive y"); got != "x <redacted> y" {
		t.Fatalf("Scrub = %q", got)
	}
}

// Matching is on bytes: a combining mark after a secret does not hide it.
func TestCombiningMarkAfterSecret(t *testing.T) {
	Register("opaque-value-combining")
	if got := Scrub("a opaque-value-combining\u0301 b"); strings.Contains(got, "opaque-value") {
		t.Fatalf("Scrub = %q", got)
	}
}

// Overlapping occurrences are all removed.
func TestOverlappingOccurrences(t *testing.T) {
	Register("ovlap-ovlap")
	if got := Scrub("ovlap-ovlap-ovlap"); got != "<redacted>" {
		t.Fatalf("Scrub = %q", got)
	}
}

// A long run of pattern prefixes without dots scrubs in linear time.
func TestRepeatedPrefixIsLinear(t *testing.T) {
	s := strings.Repeat("eyJ", 60_000)
	start := time.Now()
	if got := Scrub(s); got != s {
		t.Fatal("a dotless run must not match")
	}
	if d := time.Since(start); d > 30*time.Second { // generous for -race on a loaded machine; a quadratic scan takes minutes
		t.Fatalf("Scrub took %v", d)
	}
}

// A secret inside an unexported field of another struct, which fmt formats by reflection without calling the
// secret's methods, still prints no bearer.
func TestSecretsInUnexportedFieldsNeverFormat(t *testing.T) {
	type holder struct {
		t Token
		c ClientSecret
	}
	h := holder{NewToken("opaque-held-token-canary", time.Time{}, "", ""), NewClientSecret("opaque-held-secret-canary", time.Time{})}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(verb, h); strings.Contains(got, "canary") {
			t.Fatalf("%s printed the bearer", verb)
		}
	}
	if h.t.Bearer() != "opaque-held-token-canary" || h.c.Bearer() != "opaque-held-secret-canary" || !(Token{}).IsZero() {
		t.Fatal("Bearer lost its value")
	}
}

// A JWT that starts inside an ek_ match is removed whole.
func TestJWTInsideAnEKMatch(t *testing.T) {
	if got := Scrub("x ek_eyJa.b.c y"); got != "x "+Redacted+" y" {
		t.Fatalf("Scrub = %q", got)
	}
}

// A long, repetitive registered secret against a long, repetitive text scrubs in linear time.
func TestRepetitiveSecretIsLinear(t *testing.T) {
	sec := strings.Repeat("a", 32_768) + "b"
	Register(sec)
	s := strings.Repeat("a", 65_536)
	start := time.Now()
	if got := Scrub(s); got != s {
		t.Fatal("no occurrence, nothing to scrub")
	}
	if got := Scrub(s + "b"); got != strings.Repeat("a", 32_768)+Redacted {
		t.Fatalf("Scrub found the wrong span (%d bytes)", len(got))
	}
	if d := time.Since(start); d > 30*time.Second { // a quadratic scan takes about 10^9 comparisons
		t.Fatalf("Scrub took %v", d)
	}
}
