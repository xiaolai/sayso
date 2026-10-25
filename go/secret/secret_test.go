package secret

import (
	"encoding/json"
	"fmt"
	"math/bits"
	mrand "math/rand/v2"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
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

// Reflection finds no plaintext either: a reflect.Value of the field, and a dumper walking unexported fields, see only
// an opaque handle.
func TestSecretsNeverReflect(t *testing.T) {
	tok := NewToken("opaque-reflect-token-canary", time.Time{}, "", "")
	cs := NewClientSecret("opaque-reflect-secret-canary", time.Time{})
	for _, v := range []reflect.Value{reflect.ValueOf(tok).Field(0), reflect.ValueOf(cs).Field(0)} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if got := fmt.Sprintf(verb, v); strings.Contains(got, "canary") {
				t.Fatalf("%s of the reflected field printed the secret", verb)
			}
			if v.Kind() == reflect.Pointer && !v.IsNil() {
				if got := fmt.Sprintf(verb, v.Elem()); strings.Contains(got, "canary") {
					t.Fatalf("%s of the dereferenced field printed the secret", verb)
				}
			}
		}
	}
	if tok.Bearer() != "opaque-reflect-token-canary" || cs.Bearer() != "opaque-reflect-secret-canary" {
		t.Fatal("Bearer lost its value")
	}
}

// The rolling match finds exactly the windows equal to a registered secret: every occurrence, overlapping ones
// included, and nothing else, against a direct comparison, on texts and secrets over a two-letter alphabet where
// near misses are everywhere.
func TestRollingMatchIsExact(t *testing.T) {
	r := mrand.New(mrand.NewPCG(1, 2))
	b1, b2 := randomBase(), randomBase()
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = "ab"[r.IntN(2)]
		}
		return string(b)
	}
	for range 2000 {
		text := word(r.IntN(64))
		n := 1 + r.IntN(8)
		set := newLengthSet(n, b1, b2)
		secrets := map[string]bool{}
		for range 1 + r.IntN(4) {
			sec := word(n)
			if r.IntN(2) == 0 && len(text) >= n { // often one that occurs
				at := r.IntN(len(text) - n + 1)
				sec = text[at : at+n]
			}
			secrets[sec] = true
			set.hashes[hashPair(sec, b1, b2)] = struct{}{}
		}
		var want [][2]int
		for i := 0; i+n <= len(text); i++ {
			if secrets[text[i:i+n]] {
				want = append(want, [2]int{i, i + n})
			}
		}
		if got := occurrences(text, n, set, b1, b2); !reflect.DeepEqual(got, want) {
			t.Fatalf("text %q, secrets %v: windows %v, want %v", text, secrets, got, want)
		}
	}
}

// Thue-Morse strings collide under any odd base modulo 2^64, the modulus a rolling hash is most often written with;
// modulo 2^61-1 the complementary pair stays apart.
func TestThueMorseStringsDoNotCollide(t *testing.T) {
	tm := func(n int, flip bool) string {
		b := make([]byte, n)
		for i := range b {
			if bits.OnesCount(uint(i))%2 == 1 != flip {
				b[i] = 'b'
			} else {
				b[i] = 'a'
			}
		}
		return string(b)
	}
	x, y := tm(2048, false), tm(2048, true)
	for range 20 {
		b1, b2 := randomBase(), randomBase()
		if hashPair(x, b1, b2) == hashPair(y, b1, b2) {
			t.Fatal("a Thue-Morse pair collides")
		}
	}
}

// Register keeps nothing of the secret: once the caller drops it, its memory is collected.
func TestRegisterKeepsNothingOfTheSecret(t *testing.T) {
	s := strings.Clone("opaque-reachability-canary-" + strconv.FormatUint(mrand.Uint64(), 36))
	collected := make(chan struct{})
	runtime.AddCleanup(unsafe.StringData(s), func(ch chan struct{}) { close(ch) }, collected)
	Register(s)
	if got := Scrub("x " + s + " y"); got != "x "+Redacted+" y" {
		t.Fatalf("Scrub = %q", got)
	}
	s = ""
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		select {
		case <-collected:
			return
		case <-deadline:
			t.Fatal("the registry keeps the registered string alive")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// tinySink keeps small pointer-free values alive beside the handles allocated between them.
var tinySink []*uint64

// A dropped token's vault entry is deleted even when small values allocated beside its handle stay reachable: a
// handle the tiny allocator batched with one of them would never be collected. The race detector turns the tiny
// allocator off, so this case is decided by a run without -race.
func TestVaultEntryIsDroppedBesideLiveTinyValues(t *testing.T) {
	ids := make([]uint64, 0, 64)
	for i := range 64 {
		tok := NewToken("vault-canary-"+strconv.Itoa(i), time.Time{}, "", "")
		ids = append(ids, tok.bearer.id)
		tinySink = append(tinySink, new(uint64))
	}
	defer func() { tinySink = nil }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		left := 0
		for _, id := range ids {
			if _, ok := vault.Load(id); ok {
				left++
			}
		}
		if left == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d vault entries outlived their tokens", left, len(ids))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One conditional subtraction reduces mulMod's fold: for a, b below the modulus it is below twice the modulus.
func TestMulModMatchesBigReduction(t *testing.T) {
	for _, a := range []uint64{0, 1, 2, mersenne - 2, mersenne - 1, 1 << 60, 12345678901234567} {
		for _, b := range []uint64{0, 1, 3, mersenne - 2, mersenne - 1, 1 << 60, 98765432109876543} {
			if a >= mersenne || b >= mersenne {
				continue
			}
			hi, lo := bits.Mul64(a, b)
			_, want := bits.Div64(hi, lo, mersenne)
			if got := mulMod(a, b); got != want {
				t.Errorf("mulMod(%d, %d) = %d, want %d", a, b, got, want)
			}
		}
	}
}
