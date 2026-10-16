package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

// A padded payload is no JWT; an expiry beyond 2^62 that time.Time can hold is kept as given; one beyond is clamped,
// never wrapped.
func TestJWTPaddingAndLargeExpiries(t *testing.T) {
	if !jwtExpiry("a.eyJleHAiOjF9====.c").IsZero() {
		t.Error("a padded payload was read")
	}
	const big = 4611686018427392000
	if got := jwtExpiry(jwt(big)); got.Unix() != big {
		t.Errorf("exp %d read as %d", int64(big), got.Unix())
	}
	if got := jwtExpiry(jwt(9223372036854775807)); got.Unix() != maxUnix || got.Before(time.Now()) {
		t.Errorf("the largest exp read as %d", got.Unix())
	}
}

// An Error formatted by value, with %#v or as JSON shows no secret, including one registered after it was built.
func TestErrorFormattingRedacts(t *testing.T) {
	e := fail(StateUnsupported, "m", errors.New("cause opaque-canary-err-3"))
	e.Mode = "opaque-canary-err-3"
	secret.Register("opaque-canary-err-3")
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	vb, _ := json.Marshal(*e)
	for _, s := range []string{fmt.Sprintf("%#v", e), fmt.Sprintf("%v", *e), fmt.Sprintf("%#v", *e), e.Error(), string(b), string(vb)} {
		if strings.Contains(s, "canary") {
			t.Errorf("formatting shows the secret: %s", s)
		}
	}
}

// fmt prints a struct holding Credentials in an unexported field by reflection; the configuration, with the
// environment, is behind a pointer there.
func TestCredentialsNestedFormatting(t *testing.T) {
	c, err := New(Config{Source: SourceFile, Margin: time.Minute, Timeout: time.Second, Home: t.TempDir(),
		Env: []string{"OPENAI_API_KEY=opaque-canary-env-4", "PATH=/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	type holder struct{ c Credentials }
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, holder{*c}); strings.Contains(s, "canary") {
			t.Errorf("%s shows the environment: %s", verb, s)
		}
	}
}

// A caller whose context has already ended never runs, even when the lock is free (select picks at random).
func TestCancelledCallerNeverRuns(t *testing.T) {
	ran := 0
	c, err := New(Config{Source: SourceFile, Margin: time.Minute, Timeout: time.Second, Home: t.TempDir(),
		Env: []string{"PATH=/usr/bin"}, Guard: func(string) error { ran++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 50 {
		if _, err := c.Token(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled Token: %v", err)
		}
	}
	if ran != 0 {
		t.Errorf("a cancelled caller read auth.json %d times", ran)
	}
}

// New copies the caller's slices: changing them later changes nothing.
func TestNewCopiesTheCallersSlices(t *testing.T) {
	env, cands := []string{"PATH=/usr/bin", "X=1"}, []string{"/a"}
	c, err := New(Config{Source: SourceFile, Margin: time.Minute, Timeout: time.Second, Home: t.TempDir(), Env: env, Candidates: cands})
	if err != nil {
		t.Fatal(err)
	}
	env[1], cands[0] = "X=2", "/b"
	if c.env()[1] != "X=1" || c.cfg.Candidates[0] != "/a" {
		t.Errorf("New kept the caller's slices: %q %q", c.env(), c.cfg.Candidates)
	}
}

// A FIFO named as CODEX_HOME, or as the confining folder, is refused at once instead of blocking the open.
func TestFIFOFolderIsRefusedAtOnce(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][2]string{{fifo, ""}, {fifo, dir}, {filepath.Join(dir, "x"), fifo}} {
		done := make(chan *Error, 1)
		go func() {
			loc, e := authFilePath(args[0], dir, args[1])
			loc.close()
			done <- e
		}()
		select {
		case e := <-done:
			if e == nil || e.State != StateCodexError {
				t.Errorf("%q confined to %q: %v", args[0], args[1], e)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q confined to %q blocked", args[0], args[1])
		}
	}
}

// A relative confining folder is made absolute before it is compared, as Swift compares it.
func TestRelativeConfinement(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	loc, e := authFilePath(home, dir, ".")
	if e != nil {
		t.Fatalf("a CODEX_HOME inside the relative confinement was refused: %v", e)
	}
	loc.close()
}
