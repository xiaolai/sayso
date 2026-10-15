package codexauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/openaihttp"
	"github.com/xiaolai/sayso/go/secret"
)

func jwt(exp int64) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".c2ln"
}

func TestJWTExpiry(t *testing.T) {
	if got := jwtExpiry(jwt(1800000000)); got.Unix() != 1800000000 {
		t.Fatalf("exp %v", got)
	}
	for _, tok := range []string{"opaque", "a.b", "a.%%%.c", "eyJ.e30.c"} {
		if !jwtExpiry(tok).IsZero() {
			t.Errorf("%q has an expiry", tok)
		}
	}
}

func TestCleanMode(t *testing.T) {
	if got := cleanMode(strings.Repeat("é", 70) + "\a"); got != strings.Repeat("é", 64) {
		t.Fatalf("mode %q", got)
	}
	if got := cleanMode("x eyJa.eyJb.c"); strings.Contains(got, "eyJ") {
		t.Fatalf("mode not scrubbed: %q", got)
	}
}

func TestStateOfAndClasses(t *testing.T) {
	for state, class := range map[State]openaihttp.Class{
		StateExpired: openaihttp.ClassAuthExpired, StateCodexError: openaihttp.ClassCodexError,
		StateNoCodex: openaihttp.ClassCodexError, StateAPIKey: openaihttp.ClassAuthMode,
		StateNotSignedIn: openaihttp.ClassAuthMode, StateAppServerAuthUnavailable: openaihttp.ClassCodexError,
	} {
		err := fmt.Errorf("wrapped: %w", fail(state, "m", nil))
		if StateOf(err) != state || openaihttp.Classify(err) != class {
			t.Errorf("%s: state %s class %s", state, StateOf(err), openaihttp.Classify(err))
		}
	}
	if StateOf(nil) != StateReady || StateOf(fmt.Errorf("x")) != StateCodexError {
		t.Fatal("StateOf")
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{{Timeout: time.Second}, {Margin: time.Second}, {Margin: 1, Timeout: 1, Source: "keychain"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

func TestCredentialsNeverFormatTheToken(t *testing.T) {
	c := fileCreds(t, `{"tokens":{"access_token":"opaque-canary-77"}}`)
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprint(*c)} {
		if strings.Contains(s, "canary") {
			t.Fatalf("leaked: %s", s)
		}
	}
}

// fileCreds returns credentials reading content from a temporary home's ~/.codex/auth.json.
func fileCreds(t *testing.T, content string) *Credentials {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Source: SourceFile, Home: home, Env: []string{}, Margin: time.Minute, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Concurrent refusals of one token cause one refresh: the others get its replacement.
func TestRefreshAfterIsSingleFlight(t *testing.T) {
	now := time.Unix(1800000000, 0)
	c, err := New(Config{Source: SourceCodex, Home: t.TempDir(), Env: []string{}, Margin: time.Minute, Timeout: time.Second,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stale := secret.NewToken(jwt(now.Unix()+3600), time.Unix(now.Unix()+3600, 0), "", "test")
	c.cur, c.refreshable = &stale, true
	var refreshes atomic.Int32
	c.refresh = func(context.Context) (secret.Token, error) { // the seam that would start Codex
		refreshes.Add(1)
		time.Sleep(50 * time.Millisecond)
		return secret.NewToken(jwt(now.Unix()+7200), time.Unix(now.Unix()+7200, 0), "", "test"), nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if got, err := c.RefreshAfter(context.Background(), stale); err != nil || got.Bearer() == stale.Bearer() {
				t.Errorf("RefreshAfter: %v", err)
			}
		})
	}
	wg.Wait()
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

// A refresh failure backs off from 60 s, doubling to 15 min, and a success resets it.
func TestRefreshBackoff(t *testing.T) {
	now := time.Unix(1800000000, 0)
	c, err := New(Config{Source: SourceCodex, Home: t.TempDir(), Env: []string{}, Margin: 10 * time.Minute, Timeout: time.Second,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stale := secret.NewToken(jwt(now.Unix()+60), time.Unix(now.Unix()+60, 0), "", "test")
	c.cur, c.refreshable = &stale, true
	calls, ok := 0, false
	c.refresh = func(context.Context) (secret.Token, error) {
		calls++
		if ok {
			return secret.NewToken(jwt(now.Unix()+7200), now.Add(2*time.Hour), "", "test"), nil
		}
		return secret.Token{}, fmt.Errorf("offline")
	}
	var waits []time.Duration
	for range 6 {
		if _, err := c.Token(context.Background()); StateOf(err) != StateExpired {
			t.Fatalf("state %s", StateOf(err))
		}
		waits = append(waits, c.retryAt.Sub(now))
		if _, err := c.Token(context.Background()); StateOf(err) != StateExpired {
			t.Fatal("a call inside the backoff did not fail as expired")
		}
		now = c.retryAt
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	if fmt.Sprint(waits) != fmt.Sprint(want) || calls != 6 {
		t.Fatalf("backoffs %v (calls %d), want %v", waits, calls, want)
	}
	ok = true
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.backoff != 0 || !c.retryAt.IsZero() {
		t.Fatal("a success did not reset the backoff")
	}
}

// A cancelled context unblocks a caller waiting for the lock.
func TestTokenHonoursContextWhileWaiting(t *testing.T) {
	c := fileCreds(t, `{"tokens":{"access_token":"opaque"}}`)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Token(ctx); err == nil {
		t.Fatal("Token returned while the lock was held")
	}
}
