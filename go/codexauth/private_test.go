package codexauth

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/memscan"
	"github.com/xiaolai/sayso/go/internal/prereq"
	"github.com/xiaolai/sayso/go/secret"
)

// The fixtures (fixtures/private-sign-in/) hold the contract; these tests cover what a fixture cannot set up: the
// umask, a write cut short, a path holding a NUL, and what the write leaves in memory.

// privateHome makes a folder that grants nothing to anyone else, as AUTH-08 requires.
func privateHome(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "codex")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// privateToken is a token naming an account, with the given signature segment.
func privateToken(signature string) secret.Token {
	// {"exp":1800003600,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}
	const payload = "eyJleHAiOjE4MDAwMDM2MDAsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEifX0"
	return secret.NewToken("eyJhbGciOiJub25lIn0."+payload+"."+signature, time.Time{}, "", "test")
}

var privateNow = time.Unix(1800000000, 0)

// The file is 0600 whatever the umask: one that removes the owner's write bit, or every bit, still leaves a file
// the private Codex can read, and one that keeps group and other bits leaves none for them.
func TestAPrivateSignInIsPrivateWhateverTheUmask(t *testing.T) {
	for _, mask := range []int{0, 0o022, 0o277, 0o777} {
		home := privateHome(t)
		old := syscall.Umask(mask)
		err := WritePrivateSignIn(home, privateToken("c2ln"), privateNow)
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("umask %04o: %v", mask, err)
		}
		fi, err := os.Lstat(filepath.Join(home, "auth.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != 0o600 {
			t.Errorf("umask %04o: mode %v, want -rw-------", mask, fi.Mode())
		}
	}
}

// A write cut short (here by the file size limit) removes the file it made: a private Codex never starts from a
// partial sign-in.
func TestAWriteCutShortLeavesNoFile(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ) // the write then fails with EFBIG instead of ending the process
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	home := privateHome(t)
	limit := syscall.Rlimit{Cur: 16, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	err := WritePrivateSignIn(home, privateToken("c2ln"), privateNow)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if StateOf(err) != StateCodexError {
		t.Fatalf("state %s, want codex-error (%v)", StateOf(err), err)
	}
	if _, err := os.Lstat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a partial auth.json was left behind: %v", err)
	}
}

func TestAPrivateHomePathHoldingANULIsRefused(t *testing.T) {
	home := privateHome(t)
	if err := WritePrivateSignIn(home+"\x00/x", privateToken("c2ln"), privateNow); StateOf(err) != StateCodexError {
		t.Fatalf("state %s, want codex-error", StateOf(err))
	}
	if _, err := os.Lstat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json was written: %v", err)
	}
}

// Writing a private sign-in leaves no copy of the token in memory beyond the one kept, and no byte of its payload:
// the file's bytes are built in a buffer cleared once written, and the payload is decoded into a buffer cleared once
// read, on success and when the write is refused.
func TestAPrivateSignInKeepsOnlyTheToken(t *testing.T) {
	for _, taken := range []bool{false, true} {
		s, err := memscan.New(1 << 12)
		if err != nil {
			t.Fatal(err)
		}
		// No collection runs until the scan is done: a collected span may be zeroed, hiding a copy dropped unwiped.
		gc := debug.SetGCPercent(-1)
		sig := s.Needle(40) // the token's signature: every copy of the token holds it
		start := s.Mark()
		emit(s, doc(jwtOf(text(`{"exp":1800003600,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"},"x":"`),
			needle(), text(`"}`))))
		encoded := s.Bytes([2]int{start, s.Mark() - len("c2ln")}) // header.payload.
		// One string holds the token, built without a temporary that would hold the signature too.
		b := make([]byte, len(encoded)+40)
		copy(b, encoded)
		copy(b[len(encoded):], s.Bytes(sig))
		tok := secret.NewToken(string(b), time.Time{}, "", "test")
		clear(b)
		home := privateHome(t)
		if taken {
			if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before, err := s.Occurrences()
		if errors.Is(err, memscan.ErrUnsupported) {
			prereq.Missing(t, err.Error())
		}
		err = WritePrivateSignIn(home, tok, privateNow)
		if (err != nil) != taken {
			t.Fatalf("taken %v: %v", taken, err)
		}
		after, _ := s.Occurrences()
		if before[0] != 1 || before[1] != 0 {
			t.Fatalf("the scan's baseline is %v, want [1 0]: the test itself left a copy", before)
		}
		if after[0] != before[0] || after[1] != 0 {
			t.Errorf("taken %v: needles found %v after the write, %v before\n%s", taken, after, before, s.Explain())
		}
		debug.SetGCPercent(gc)
		s.Close()
		if tok.Bearer() == "" { // the token kept must still be live when the scan runs
			t.Fatal("no token")
		}
	}
}

// A file writeNew cannot remove is reported with the failure that made it remove it: the file left behind blocks the
// next write. One already gone is no failure.
func TestARemovalThatFailsIsReported(t *testing.T) {
	home := privateHome(t)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := syscall.Open(home, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dir)
	if err := os.Chmod(home, 0o500); err != nil { // no write permission, so no unlink
		t.Fatal(err)
	}
	defer os.Chmod(home, 0o700)
	cause := errors.New("cause")
	e := removeCreated(dir, fail(StateCodexError, "auth.json cannot be written", cause))
	if !errors.Is(e, cause) || !errors.Is(e, syscall.EACCES) {
		t.Errorf("a failed removal: %v", e)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if e := removeCreated(dir, fail(StateCodexError, "auth.json cannot be written", cause)); errors.Is(e, syscall.ENOENT) {
		t.Errorf("a file already gone: %v", e)
	}
}
