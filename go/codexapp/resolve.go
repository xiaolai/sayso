package codexapp

import (
	"errors"
	"fmt"
	"github.com/xiaolai/sayso/go/secret"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ErrNotFound reports that no Codex executable could be resolved.
var ErrNotFound = errors.New("codex executable not found")

// Candidates are the folders searched after PATH, in order, for an app launched from the Finder, which has no
// login-shell PATH: Homebrew, /usr/local, the common per-user installers, then every nvm Node version, newest first.
func Candidates(home string) []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if strings.ContainsRune(home, 0) {
		return dirs // joining would clean the NUL away ("/x\x00/../y" is "/y"), so a home holding one adds nothing
	}
	for _, rel := range []string{".local/bin", ".npm-global/bin", ".volta/bin", ".bun/bin", "Library/pnpm"} {
		dirs = append(dirs, filepath.Join(home, rel))
	}
	return append(dirs, nvmBins(home)...)
}

// nvmBins lists ~/.nvm/versions/node/*/bin, newest version first by numeric comparison.
func nvmBins(home string) []string {
	root := filepath.Join(home, ".nvm", "versions", "node")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	slices.SortFunc(versions, func(a, b string) int {
		switch {
		case VersionLess(strings.TrimPrefix(b, "v"), strings.TrimPrefix(a, "v")):
			return -1
		case VersionLess(strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")):
			return 1
		}
		return strings.Compare(a, b)
	})
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = filepath.Join(root, v, "bin")
	}
	return out
}

// Resolve returns the absolute path of the Codex executable: explicit when set (a path, or a name looked up in
// path), otherwise `codex` in path, then in each of candidates. An explicit choice that does not resolve is an
// error, never a fall-through. The result is absolute, a regular file, and executable.
func Resolve(explicit, path string, candidates []string) (string, error) {
	if explicit != "" {
		if strings.Contains(explicit, "/") {
			p, why := executable(explicit)
			if why == "" {
				return p, nil
			}
			return "", &resolveError{explicit: sealed(explicit), reason: why}
		}
		if p, ok := lookIn(filepath.SplitList(path), explicit); ok {
			return p, nil
		}
		return "", &resolveError{explicit: sealed(explicit), reason: "is not on PATH"}
	}
	if p, ok := lookIn(filepath.SplitList(path), "codex"); ok {
		return p, nil
	}
	if p, ok := lookIn(candidates, "codex"); ok {
		return p, nil
	}
	return "", ErrNotFound
}

// resolveError is an explicit choice that did not resolve. It quotes the caller's text, which may hold a secret
// registered later (spec/credentials.md, hygiene), so the text is scrubbed whenever the error is formatted, not when
// it is built, and is held in a closure that neither nested formatting nor reflection can read. It is ErrNotFound
// for errors.Is.
type resolveError struct {
	explicit func() string // nil for ""
	reason   string
}

func (e *resolveError) Error() string {
	text := ""
	if e.explicit != nil {
		text = e.explicit()
	}
	// Scrubbed before quoting, whose escapes would hide a secret holding a quote or a control character, and after.
	return secret.Scrub(fmt.Sprintf("%s: %q %s", ErrNotFound, secret.Scrub(text), e.reason))
}

func (e *resolveError) Unwrap() error { return ErrNotFound }

// GoString redacts like Error.
func (e *resolveError) GoString() string { return e.Error() }

// Format prints Error for every verb.
func (e *resolveError) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(e.Error())) }

func lookIn(dirs []string, name string) (string, bool) {
	if strings.ContainsRune(name, 0) {
		return "", false
	}
	for _, d := range dirs {
		// A relative PATH entry would resolve against our working folder; a NUL would be cleaned away by Join.
		if d == "" || !filepath.IsAbs(d) || strings.ContainsRune(d, 0) {
			continue
		}
		if p, why := executable(filepath.Join(d, name)); why == "" {
			return p, true
		}
	}
	return "", false
}

// executable returns the absolute form of p when it is a regular file this process may execute (an execute bit for
// another user only would stop the search at a file that cannot start) and that not every user can change; otherwise
// why not.
func executable(p string) (string, string) {
	const notExecutable = "is not an executable file"
	if strings.ContainsRune(p, 0) {
		return "", notExecutable
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", notExecutable
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || syscall.Access(abs, xOK) != nil {
		return "", notExecutable
	}
	if changeableByOthers(abs) {
		return "", "is writable by every user, or lies in a folder that is"
	}
	return abs, ""
}

// xOK is access(2)'s X_OK.
const xOK = 0x1

// VersionLess reports whether dotted numeric version a is below b (spec/credentials.md, executable). Components
// compare as signed 64-bit integers: a missing or non-numeric one is 0, and one too large saturates.
func VersionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int64
		if i < len(as) {
			x = component(as[i])
		}
		if i < len(bs) {
			y = component(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// component reads an optional sign and decimal digits, saturating at the int64 bounds; anything else is 0. The
// whole component is checked before it is parsed, so an overflow cannot hide a non-numeric suffix.
func component(s string) int64 {
	digits := strings.TrimLeft(s[:min(len(s), 1)], "+-") + s[min(len(s), 1):]
	if digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil { // only a range error remains: saturate
		if s[0] == '-' {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return n
}
