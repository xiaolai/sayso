package codexapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layout builds the folders and files of a test tree under a fresh folder of this user's, each with its mode, and
// links as name -> target.
func layout(t *testing.T, dirs map[string]os.FileMode, files map[string]os.FileMode, links map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for f, mode := range files {
		p := filepath.Join(root, f)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	for l, target := range links {
		if err := os.Symlink(target, filepath.Join(root, l)); err != nil {
			t.Fatal(err)
		}
	}
	for d, mode := range dirs { // last, so a closed folder was still writable while the tree was built
		if err := os.Chmod(filepath.Join(root, d), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for d := range dirs {
			_ = os.Chmod(filepath.Join(root, d), 0o755)
		}
	})
	return root
}

// A Codex is refused when every user can change it: the file is writable by all, or a folder read on the way to it,
// a link's own folder included, is; a sticky folder of this user's is not such a folder. Owners and group write are
// not judged.
func TestChangeableByOthers(t *testing.T) {
	root := layout(t,
		map[string]os.FileMode{
			"safe": 0o755, "safe/bin": 0o755, "safe/lib": 0o755, "open": 0o777, "open/deep": 0o755,
			"sticky": 0o777 | os.ModeSticky, "group": 0o775,
		},
		map[string]os.FileMode{
			"safe/codex": 0o755, "safe/lib/codex.js": 0o755, "safe/all": 0o757, "open/codex": 0o755,
			"open/deep/codex": 0o755, "sticky/codex": 0o755, "group/codex": 0o775,
		},
		map[string]string{
			"safe/bin/codex": "../lib/codex.js", "safe/to-open": "../open/codex", "open/to-safe": "../safe/codex",
			"safe/loop": "loop",
		})
	if err := os.Symlink(filepath.Join(root, "safe/lib/codex.js"), filepath.Join(root, "safe/abs")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"safe/codex", false},
		{"safe/bin/codex", false}, // a relative link with "..", every folder safe
		{"safe/abs", false},       // an absolute link
		{"sticky/codex", false},   // a sticky folder of this user's, holding this user's file
		{"group/codex", false},    // group write is not judged
		{"safe/all", true},        // the file is writable by all
		{"open/codex", true},      // its folder is
		{"open/deep/codex", true}, // an ancestor is
		{"open/to-safe", true},    // the link's own folder is: replacing the link changes the file
		{"safe/to-open", true},    // the link leads into an open folder
		{"safe/loop", true},       // a link loop
		{"safe/missing", true},    // what cannot be read counts as changeable
		{"safe", true},            // a folder is no executable
	} {
		if got := changeableByOthers(filepath.Join(root, tc.path)); got != tc.want {
			t.Errorf("%s: changeable %v, want %v", tc.path, got, tc.want)
		}
	}

	// A search skips a Codex everyone can change and takes the next; an explicit one is refused, saying why.
	bin, err := Resolve("", filepath.Join(root, "open")+":"+filepath.Join(root, "safe"), nil)
	if err != nil || bin != filepath.Join(root, "safe", "codex") {
		t.Fatalf("Resolve: %q, %v", bin, err)
	}
	if _, err := Resolve(filepath.Join(root, "open", "codex"), "", nil); err == nil || !strings.Contains(err.Error(), "writable by every user") {
		t.Fatalf("an explicit Codex everyone can change: %v", err)
	}
}
