package codexauth

import (
	"context"
	"encoding/base64"
	"errors"
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

// Callers that refused a token while a refresh ran take that refresh's result, even when Codex returned the same
// bearer: one refresh, not one per caller.
func TestRefreshAfterSharesARefreshThatReturnedTheSameBearer(t *testing.T) {
	now := time.Unix(1800000000, 0)
	c, err := New(Config{Source: SourceCodex, Home: t.TempDir(), Env: []string{}, Margin: time.Minute, Timeout: time.Second,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	same := secret.NewToken(jwt(now.Unix()+3600), time.Unix(now.Unix()+3600, 0), "", "test")
	c.cur, c.refreshable = &same, true
	var refreshes atomic.Int32
	c.refresh = func(context.Context) (secret.Token, error) {
		refreshes.Add(1)
		time.Sleep(100 * time.Millisecond)
		return same, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.RefreshAfter(context.Background(), same); err != nil {
				t.Errorf("RefreshAfter: %v", err)
			}
		})
	}
	wg.Wait()
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

// The backoff runs from the failure, so a refresh that took longer than the backoff does not allow an immediate retry.
func TestBackoffRunsFromTheFailure(t *testing.T) {
	now := time.Unix(1800000000, 0)
	c, err := New(Config{Source: SourceCodex, Home: t.TempDir(), Env: []string{}, Margin: 10 * time.Minute, Timeout: time.Second,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stale := secret.NewToken(jwt(now.Unix()+60), time.Unix(now.Unix()+60, 0), "", "test")
	c.cur, c.refreshable = &stale, true
	calls := 0
	c.refresh = func(context.Context) (secret.Token, error) {
		calls++
		now = now.Add(90 * time.Second) // a slow failure
		return secret.Token{}, fmt.Errorf("offline")
	}
	_, _ = c.Token(context.Background())
	now = now.Add(time.Second)
	if _, err := c.Token(context.Background()); StateOf(err) != StateExpired || calls != 1 {
		t.Fatalf("a call 1 s after a slow failure refreshed again (calls %d, %v)", calls, err)
	}
}

// A refresh the caller cancelled is not a refresh failure: it spends no backoff.
func TestCancelledRefreshSpendsNoBackoff(t *testing.T) {
	now := time.Unix(1800000000, 0)
	c, err := New(Config{Source: SourceCodex, Home: t.TempDir(), Env: []string{}, Margin: 10 * time.Minute, Timeout: time.Second,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stale := secret.NewToken(jwt(now.Unix()+60), time.Unix(now.Unix()+60, 0), "", "test")
	c.cur, c.refreshable = &stale, true
	ctx, cancel := context.WithCancel(context.Background())
	c.refresh = func(ctx context.Context) (secret.Token, error) {
		cancel()
		return secret.Token{}, ctx.Err()
	}
	if _, err := c.Token(ctx); err == nil {
		t.Fatal("a cancelled refresh succeeded")
	}
	if c.backoff != 0 || !c.retryAt.IsZero() {
		t.Fatalf("a cancelled refresh set a backoff of %v", c.backoff)
	}
}

// Formatting the credentials, the value as well as the pointer, or their Config never prints the environment.
func TestFormattingNeverPrintsTheEnvironment(t *testing.T) {
	c, err := New(Config{Source: SourceFile, Home: t.TempDir(), Env: []string{"OPENAI_API_KEY=canary-env-2"},
		Margin: time.Minute, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", *c, *c, *c, *c), fmt.Sprint(c), fmt.Sprintf("%+v %#v", c.cfg, c.cfg)} {
		if strings.Contains(s, "canary") {
			t.Fatalf("leaked: %s", s)
		}
	}
}

// Error text is scrubbed whole, so a Mode set by a caller cannot carry a secret out.
func TestErrorScrubsItsMode(t *testing.T) {
	secret.Register("canary-mode-3")
	e := &Error{State: StateUnsupported, Mode: "canary-mode-3 ek_abc"}
	if s := e.Error(); strings.Contains(s, "canary") || strings.Contains(s, "ek_abc") {
		t.Fatalf("leaked: %s", s)
	}
	if got := cleanMode("e\x00k_canary4"); strings.Contains(got, "canary") {
		t.Fatalf("a secret joined by filtering survived: %q", got)
	}
}

// A CODEX_HOME swapped for a link after it was validated cannot redirect the read: the folder is read through the
// descriptor opened when it was checked.
func TestFileReadsThroughThePinnedFolder(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inside, outside := filepath.Join(home, "codex"), filepath.Join(t.TempDir(), "elsewhere")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "auth.json"), []byte(`{"tokens":{"access_token":"outside-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loc, e := authFilePath(inside, home, home)
	if e != nil {
		t.Fatal(e)
	}
	defer loc.close()
	// Swap the validated folder for a link outside the confinement.
	if err := os.Rename(inside, inside+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	tok, e := readAuthFile(loc, nil)
	if e == nil && tok.Bearer() == "outside-token" {
		t.Fatal("the read followed the swapped folder outside the confinement")
	}
}

// A late refusal of an older token does not unmark the current one (spec/credentials.md, after a refusal).
func TestLateRefusalKeepsTheCurrentRefusal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c, err := New(Config{Source: SourceCodex, Margin: time.Minute, Timeout: time.Second, Env: []string{}, Home: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	a := secret.NewToken("opaque-late-a", time.Time{}, "", "test")
	b := secret.NewToken("opaque-late-b", time.Time{}, "", "test")
	c.cur, c.refreshable = &a, true
	calls := 0
	c.refresh = func(context.Context) (secret.Token, error) {
		calls++
		if calls == 1 {
			return b, nil
		}
		return secret.Token{}, errors.New("offline")
	}
	if got, err := c.RefreshAfter(t.Context(), a); err != nil || got.Bearer() != b.Bearer() {
		t.Fatalf("first refresh: %v", err)
	}
	if _, err := c.RefreshAfter(t.Context(), b); err == nil {
		t.Fatal("B's refresh succeeded")
	}
	if _, err := c.RefreshAfter(t.Context(), a); err == nil {
		t.Fatal("a late refusal of A returned a token")
	}
	if tok, err := c.Token(t.Context()); err == nil {
		t.Fatalf("Token handed out %v after B was refused", tok.Bearer() == b.Bearer())
	}
	if calls != 2 {
		t.Errorf("refresh calls %d, want 2", calls)
	}
}

// A caller paused before taking the lock does not take a token refused after a refresh it waited for.
func TestPausedRefusalDoesNotTakeARefusedToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c, err := New(Config{Source: SourceCodex, Margin: time.Minute, Timeout: time.Second, Env: []string{}, Home: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	a := secret.NewToken("opaque-paused-a", time.Time{}, "", "test")
	b := secret.NewToken("opaque-paused-b", time.Time{}, "", "test")
	c.cur, c.refreshable = &a, true
	c.refresh = func(context.Context) (secret.Token, error) { return secret.Token{}, errors.New("offline") }
	if err := c.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.RefreshAfter(t.Context(), a) // reads the refresh count, then waits for the lock
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// Meanwhile: a refresh returned B, then B was refused and its refresh failed.
	c.cur = &b
	c.refreshes.Add(1)
	c.refused = b.Bearer()
	c.backoff, c.retryAt = backoffStart, now.Add(backoffStart)
	c.release()
	if err := <-done; err == nil {
		t.Fatal("the paused caller took the refused B")
	}
}

// Formatting the value while a fetch runs reads no mutable state (run under -race).
func TestFormattingDoesNotRaceAFetch(t *testing.T) {
	c, err := New(Config{Source: SourceCodex, Margin: time.Minute, Timeout: time.Second, Env: []string{}, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tok := secret.NewToken("opaque-race", time.Time{}, "", "test")
	c.cur, c.refreshable = &tok, true
	c.refresh = func(context.Context) (secret.Token, error) { return tok, nil }
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = fmt.Sprintf("%v %+v", *c, *c)
			}
		}
	}()
	for range 200 {
		_, _ = c.RefreshAfter(t.Context(), tok)
	}
	close(stop)
}
