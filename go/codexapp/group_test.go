package codexapp

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/xiaolai/sayso/go/internal/prereq"
)

// fakeCodex writes an executable Python child: it answers initialize, reads initialized, then runs body.
// bulkTimeout bounds the tests whose child writes megabytes or tens of thousands of lines: under the race detector on
// a loaded host that takes seconds, so a shorter bound fails a correct client. What those tests tell apart is the
// error (the overflow, the answer) and not the time, and a client that stalls still fails at this bound.
const bulkTimeout = 60 * time.Second

func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codex")
	src := "#!" + prereq.Python(t) + "\nimport sys, json, os, time, subprocess\n" +
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

// A host that ignores SIGCHLD has the kernel reap the child at exit, so by the time Close runs its ID, and its group
// ID, may be another process's: Close then signals nothing, even when a group with that ID is alive.
func TestCloseSignalsNothingOnceTheSystemReapedTheChild(t *testing.T) {
	if os.Getenv("SAYSO_TEST_SIGCHLD_IGNORED") != "1" {
		// Ignoring SIGCHLD is process-wide, and signal.Reset does not undo it (the runtime keeps its own handler for
		// SIGCHLD), so the case runs in a test process of its own.
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "SAYSO_TEST_SIGCHLD_IGNORED=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	signal.Ignore(syscall.SIGCHLD)
	c, err := Start(t.Context(), Config{Bin: fakeCodex(t, "os._exit(0)\n"), Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := c.PID()
	select {
	case <-c.k.exited:
	case <-time.After(8 * time.Second):
		t.Fatal("the child did not exit")
	}
	if held(pid) {
		t.Fatal("the child is still held: SIGCHLD was not ignored")
	}
	var sent []syscall.Signal
	c.k.testLiveGroup = func(int) bool { return true } // another process's group, under the reused ID
	c.k.testSignal = func(_ int, sig syscall.Signal) { sent = append(sent, sig) }
	start := time.Now()
	_ = c.Close()
	if len(sent) > 0 {
		t.Errorf("Close signalled a group that is not ours: %v", sent)
	}
	if d := time.Since(start); d >= killGrace {
		t.Errorf("Close waited %v on a group that is not ours", d)
	}
}

// A probe that finds the group empty is not the last word: a member can fork and exit between its steps, leaving a
// live member it never saw. While the exited child is unreaped the group is still ours, so Close sends it SIGKILL
// before reaping.
func TestCloseKillsTheGroupItFoundEmptyBeforeReaping(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c, err := Start(t.Context(), Config{Bin: fakeCodex(t, "os._exit(0)\n"), Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	pid := c.PID()
	recv(t, "the child's exit", c.k.exited)
	var sent []syscall.Signal
	c.k.testLiveGroup = func(int) bool { return false } // the probe missed a member
	c.k.testSignal = func(_ int, sig syscall.Signal) { sent = append(sent, sig) }
	_ = c.Close()
	if len(sent) != 1 || sent[0] != syscall.SIGKILL {
		t.Errorf("Close sent %v to the group it found empty, want one SIGKILL", sent)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Close left the child unreaped: %v", err)
	}
}
