package codexapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/secret"
)

func TestVersionComponents(t *testing.T) {
	if !VersionLess("0.999999999999999999999x", "0.162.0") {
		t.Error("a non-numeric component that overflows first is not 0")
	}
	if VersionLess("0.999999999999999999999", "0.162.0") {
		t.Error("an overflowing component does not saturate")
	}
	if !VersionLess("3000000000", "4000000000") {
		t.Error("components beyond 32 bits do not compare")
	}
}

func TestNULIsRefusedBeforeJoining(t *testing.T) {
	root := t.TempDir()
	writeExec(t, filepath.Join(root, "trusted", "codex"))
	bad := root + "/bad\x00/../trusted"
	if p, err := Resolve("", bad, nil); err == nil {
		t.Errorf("a PATH entry holding a NUL resolved %q", p)
	}
	if p, err := Resolve("", "", []string{bad}); err == nil {
		t.Errorf("a candidate holding a NUL resolved %q", p)
	}
	if got := Candidates(root + "\x00/.."); len(got) != 2 {
		t.Errorf("a home holding a NUL added candidates: %q", got)
	}
}

func TestAuthStatusEncodingRedacts(t *testing.T) {
	secret.Register("opaque-canary-status-1")
	st := AuthStatus{Method: "opaque-canary-status-1", HasMethod: true, token: &sealedToken{"opaque-canary-status-1"}}
	b, err := json.Marshal(st)
	if err != nil || strings.Contains(string(b), "canary") {
		t.Fatalf("json: %s %v", b, err)
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("x", "status", st)
	if strings.Contains(log.String(), "canary") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestDecodeRateLimits(t *testing.T) {
	ok := `{"ordinaryUsageAllowed":null,"rateLimits":{"planType":"plus",` +
		`"primary":{"usedPercent":10,"USEDPERCENT":99,"windowDurationMins":300,"resetsAt":1800000000}}}`
	rl, err := decodeRateLimits(json.RawMessage(ok))
	if err != nil {
		t.Fatal(err)
	}
	if rl.OrdinaryUsageKnown || rl.OrdinaryUsageAllowed {
		t.Errorf("null permission read as known: %+v", rl)
	}
	if len(rl.Windows) != 1 || rl.Windows[0].UsedPercent != 10 || rl.Windows[0].WindowMins != 300 ||
		!rl.Windows[0].ResetsAt.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("windows: %+v", rl.Windows)
	}
	if rl, err := decodeRateLimits(json.RawMessage(`{"ordinaryUsageAllowed":false}`)); err != nil || !rl.OrdinaryUsageKnown || rl.OrdinaryUsageAllowed {
		t.Errorf("a denial: %+v %v", rl, err)
	}
	for _, bad := range []string{
		`{"rateLimits":{"primary":{}}}`,                   // a window without usedPercent
		`{"rateLimits":{"primary":{"usedPercent":"10"}}}`, // the wrong type
		`{"ordinaryUsageAllowed":"yes"}`,                  // the wrong type
		`{"rateLimits":{"primary":{"usedPercent":1,"resetsAt":1.5}}}`,
		`{"rateLimits":{"primary":{"usedPercent":12.5}}}`,       // Codex's schema: an int32
		`{"rateLimits":{"primary":{"usedPercent":2147483648}}}`, // beyond int32
	} {
		if _, err := decodeRateLimits(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if rl, err := decodeRateLimits(json.RawMessage(`null`)); err != nil || len(rl.Windows) != 0 {
		t.Errorf("null: %+v %v", rl, err)
	}
}

// stalled starts a child that answers initialize and never reads again.
func stalled(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	script := filepath.Join(t.TempDir(), "codex")
	body := "#!" + python + "\nimport sys, json, time\nm = json.loads(sys.stdin.readline())\n" +
		"print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\ntime.sleep(30)\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Config{Bin: script, Env: []string{"PATH=/usr/bin:/bin"}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var big = map[string]any{"pad": strings.Repeat("x", 4<<20)} // far beyond a pipe buffer

// A write cut off inside a request leaves the stream unusable, so the next call fails at once; an already-cancelled
// call sends nothing; a waiting writer gives up at its own deadline; cancellation interrupts a blocked write.
func TestWritePathUnderFailure(t *testing.T) {
	c := stalled(t, 3*time.Second)
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Call(ctx, "x", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled call: %v", err)
	}

	blocked := make(chan error, 1)
	go func() { blocked <- c.Call(t.Context(), "big", big, nil) }()
	time.Sleep(300 * time.Millisecond)
	short, cancelShort := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	if err := c.Call(short, "small", nil, nil); err == nil || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("a writer waiting its turn returned %v after %v", err, time.Since(start))
	}
	if err := <-blocked; err == nil {
		t.Fatal("the blocked call succeeded")
	}
	start = time.Now()
	if err := c.Call(t.Context(), "small", nil, nil); err == nil || time.Since(start) > time.Second {
		t.Errorf("a call after a cut-off write returned %v after %v", err, time.Since(start))
	}

	d := stalled(t, 10*time.Second)
	defer d.Close()
	ctx, cancel = context.WithCancel(t.Context())
	go func() { blocked <- d.Call(ctx, "big", big, nil) }()
	time.Sleep(300 * time.Millisecond)
	start = time.Now()
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt the write")
	}
}

// A Client dropped without Close ends its child when it is collected.
func TestDroppedClientEndsItsChild(t *testing.T) {
	pid := func() int {
		c := stalled(t, 3*time.Second)
		return c.PID()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a dropped client left its child running")
}
