package codexapp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func python(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3")
	}
	return p
}

// fakeCodex writes an executable Python child: it answers initialize, reads initialized, then runs body.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codex")
	src := "#!" + python(t) + "\nimport sys, json, os, time, subprocess\n" +
		"m = json.loads(sys.stdin.readline())\n" +
		"print(json.dumps({'id': m['id'], 'result': {'userAgent': 'codex_cli_rs/0.162.1'}}), flush=True)\n" +
		"sys.stdin.readline()\n" + body
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// When the child exits leaving a member of its group alive, Close still signals that member, because the exited
// child stays unreaped and keeps the group ID ours; Close reaps it last.
func TestCloseSignalsTheGroupOfAnExitedChild(t *testing.T) {
	if !reservesGroup {
		t.Skip("the group is reserved on macOS and Linux only")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	pidFile := filepath.Join(t.TempDir(), "member")
	script := fakeCodex(t, "p = subprocess.Popen(['/bin/sleep', '30'])\n"+
		"open(os.environ['MEMBER_FILE'] + '.tmp', 'w').write(str(p.pid))\n"+
		"os.rename(os.environ['MEMBER_FILE'] + '.tmp', os.environ['MEMBER_FILE'])\n"+
		"os._exit(0)\n")
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin", "MEMBER_FILE=" + pidFile}), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := c.PID()
	select {
	case <-c.k.exited:
	case <-time.After(8 * time.Second):
		t.Fatal("the child did not exit")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if pgid, err := syscall.Getpgid(member); err != nil || pgid != pid {
		t.Fatalf("the member is not in the child's group: %d %v", pgid, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the exited child was reaped before Close: %v", err)
	}
	start := time.Now()
	_ = c.Close()
	if d := time.Since(start); d > killGrace {
		t.Errorf("close took %v: SIGTERM did not reach the member", d)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Close left the child unreaped: %v", err)
	}
	// The member, reparented, is reaped by init once SIGTERM ends it.
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(member, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(member, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(member, syscall.SIGKILL)
		t.Errorf("the member outlived Close: %v", err)
	}
}

// groupAlive tells a group holding only our exited, unreaped child from one with a live member.
func TestGroupAliveIgnoresTheZombieLeader(t *testing.T) {
	if !reservesGroup {
		t.Skip("the group is reserved on macOS and Linux only")
	}
	script := fakeCodex(t, "os._exit(0)\n")
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	select {
	case <-c.k.exited:
	case <-time.After(8 * time.Second):
		t.Fatal("the child did not exit")
	}
	if groupAlive(c.PID()) {
		t.Error("a group holding only the zombie leader counts as alive")
	}
}
