package codexapp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

// A child that has exited stays unreaped until Close, so its process and group IDs cannot be reused while Close may
// still signal the group; Close reaps it.
func TestChildStaysUnreapedUntilClose(t *testing.T) {
	if !reservesGroup {
		t.Skip("the group is reserved on darwin only")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	script := filepath.Join(t.TempDir(), "codex")
	body := "#!" + python + "\nimport sys, json\nm = json.loads(sys.stdin.readline())\n" +
		"print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n" +
		"sys.stdin.readline()\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Config{Bin: script, Env: []string{"PATH=/usr/bin:/bin"}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := c.k.cmd.Process.Pid
	select {
	case <-c.k.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the child did not exit after initialized")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the exited child was reaped before Close: %v", err)
	}
	_ = c.Close()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Close left the child unreaped: %v", err)
	}
}

// fmt prints a struct holding an AuthStatus in an unexported field by reflection, without its redacting methods; the
// token is behind a pointer there, so only an address appears.
func TestAuthStatusNestedFormatting(t *testing.T) {
	secret.Register("opaque-canary-nested-1")
	st := AuthStatus{Method: "chatgpt", HasMethod: true, token: &sealedToken{"opaque-canary-nested-1"}}
	type holder struct{ status AuthStatus }
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, holder{st}); strings.Contains(s, "canary") {
			t.Errorf("%s shows the token: %s", verb, s)
		}
	}
	if st.Token() != "opaque-canary-nested-1" {
		t.Fatal("the token is unreadable through Token")
	}
}

// A resolution error quotes the caller's text scrubbed.
func TestResolveErrorIsScrubbed(t *testing.T) {
	secret.Register("opaque-canary-path-2")
	for _, explicit := range []string{"/x/opaque-canary-path-2", "opaque-canary-path-2"} {
		_, err := Resolve(explicit, "/nonexistent", nil)
		if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "canary") {
			t.Errorf("%q: %v", explicit, err)
		}
	}
}

// A number beyond a float64 makes Codex's JSON malformed wherever it is; one that underflows to zero does not.
func TestValidJSONRefusesNumbersBeyondFloat64(t *testing.T) {
	for in, want := range map[string]bool{
		`{"n":1e999}`: false, `{"n":-1e400}`: false, `{"n":1e-999}`: true, `{"n":123456789012345678901234567890}`: true,
		`{"s":"1e999"}`: true, `[1,2.5,-3e2]`: true, `{"a":true,"b":null}`: true,
	} {
		if got := ValidJSON([]byte(in)); got != want {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
}
