package codexapp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrNotFound reports that no Codex executable could be resolved.
var ErrNotFound = errors.New("codex executable not found")

// Candidates are the folders searched after PATH, in order, for an app launched from the Finder, which has no
// login-shell PATH: Homebrew, /usr/local, the common per-user installers, then every nvm Node version, newest first.
func Candidates(home string) []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin"}
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
			if p, ok := executable(explicit); ok {
				return p, nil
			}
			return "", fmt.Errorf("%w: %q is not an executable file", ErrNotFound, explicit)
		}
		if p, ok := lookIn(filepath.SplitList(path), explicit); ok {
			return p, nil
		}
		return "", fmt.Errorf("%w: %q is not on PATH", ErrNotFound, explicit)
	}
	if p, ok := lookIn(filepath.SplitList(path), "codex"); ok {
		return p, nil
	}
	if p, ok := lookIn(candidates, "codex"); ok {
		return p, nil
	}
	return "", ErrNotFound
}

func lookIn(dirs []string, name string) (string, bool) {
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) { // a relative PATH entry would resolve against our working folder
			continue
		}
		if p, ok := executable(filepath.Join(d, name)); ok {
			return p, true
		}
	}
	return "", false
}

// executable returns the absolute form of p when it is a regular file with an execute bit.
func executable(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return abs, true
}

// VersionLess reports whether dotted numeric version a is below b. Missing components count as zero.
func VersionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		x, y := 0, 0
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
