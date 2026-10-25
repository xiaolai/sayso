package codexauth

import (
	"os"
	"path/filepath"
	"testing"
)

// tree makes base/confine/a/b and base/outside/a/b, each holding an auth.json with its own token, and returns base.
func tree(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"confine", "outside"} {
		d := filepath.Join(base, side, "a", "b")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"tokens":{"access_token":"` + side + `-token"}}`
		if err := os.WriteFile(filepath.Join(d, "auth.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// swapForLink replaces the folder at p with a link to target.
func swapForLink(t *testing.T, p, target string) {
	t.Helper()
	if err := os.Rename(p, p+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// A folder of the resolved path replaced with a link between resolution and opening, CODEX_HOME itself or an
// ancestor, confined or not, fails the open: the link is never followed out of the confinement.
func TestFolderSwappedAfterResolutionIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, swap, confine string }{
		{"codex home, confined", "confine/a/b", "confine"},
		{"ancestor, confined", "confine/a", "confine"},
		{"codex home, unconfined", "confine/a/b", ""},
		{"ancestor, unconfined", "confine/a", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tree(t)
			confine := ""
			if tc.confine != "" {
				confine = filepath.Join(base, tc.confine)
			}
			rel, _ := filepath.Rel("confine", tc.swap)
			afterResolve = func() { swapForLink(t, filepath.Join(base, tc.swap), filepath.Join(base, "outside", rel)) }
			defer func() { afterResolve = nil }()
			loc, e := authFilePath(filepath.Join(base, "confine", "a", "b"), base, confine)
			if e == nil {
				tok, _ := readAuthFile(t.Context(), loc, nil)
				loc.close()
				t.Fatalf("the swapped path was opened; it read %q", tok.Source)
			}
			if e.State != StateCodexError {
				t.Fatalf("state %s, want codex-error", e.State)
			}
		})
	}
}

// A CODEX_HOME that is a link when resolved is followed (spec/credentials.md): only a change after resolution fails.
func TestLinkedCodexHomeStillResolves(t *testing.T) {
	base := tree(t)
	link := filepath.Join(base, "confine", "link")
	if err := os.Symlink(filepath.Join(base, "confine", "a", "b"), link); err != nil {
		t.Fatal(err)
	}
	loc, e := authFilePath(link, base, filepath.Join(base, "confine"))
	if e != nil {
		t.Fatalf("a linked CODEX_HOME inside the confinement was refused: %v", e)
	}
	defer loc.close()
	if tok, e := readAuthFile(t.Context(), loc, nil); e != nil || tok.Bearer() != "confine-token" {
		t.Fatalf("read %v", e)
	}
}

// auth.json replaced with a relative link to a renamed copy of itself (the same inode, so a same-file check after
// following it passes) is refused, on the first read and on the re-read after a torn write.
func TestAuthFileLinkToItsOwnInodeIsRefused(t *testing.T) {
	for _, onRetry := range []bool{false, true} {
		base := tree(t)
		dir := filepath.Join(base, "confine", "a", "b")
		auth := filepath.Join(dir, "auth.json")
		swap := func() {
			if err := os.Rename(auth, filepath.Join(dir, "saved.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("saved.json", auth); err != nil {
				t.Fatal(err)
			}
		}
		var hook func()
		if onRetry {
			if err := os.WriteFile(filepath.Join(dir, "torn.json"), []byte(`{"tokens":`), 0o600); err != nil {
				t.Fatal(err)
			}
			// The first read sees a torn file; the swap happens before the re-read.
			if err := os.Rename(auth, filepath.Join(dir, "whole.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(dir, "torn.json"), auth); err != nil {
				t.Fatal(err)
			}
			hook = func() {
				if err := os.Rename(filepath.Join(dir, "whole.json"), auth); err != nil {
					t.Fatal(err)
				}
				swap()
			}
		} else {
			swap()
		}
		loc, e := authFilePath(dir, base, filepath.Join(base, "confine"))
		if e != nil {
			t.Fatal(e)
		}
		tok, e := readAuthFile(t.Context(), loc, hook)
		loc.close()
		if e == nil || e.State != StateCodexError {
			t.Fatalf("retry %v: read %q, %v; want codex-error", onRetry, tok.Bearer(), e)
		}
	}
}

// An ancestor the user may search but not read, as a 0711 home folder is to others, does not refuse CODEX_HOME:
// walking a path needs search permission only, so each folder is opened for search (O_SEARCH, O_PATH on Linux).
// 0311 on the user's own folder stands in for a folder another user owns.
func TestSearchableUnreadableAncestorIsWalked(t *testing.T) {
	base := tree(t)
	a := filepath.Join(base, "confine", "a")
	if err := os.Chmod(a, 0o311); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(a, 0o700) }()
	if _, err := os.ReadDir(a); err == nil {
		t.Skip("the folder is readable anyway (running as root?)")
	}
	loc, e := authFilePath(filepath.Join(a, "b"), base, filepath.Join(base, "confine"))
	if e != nil {
		t.Fatalf("authFilePath: %v", e)
	}
	defer loc.close()
	tok, e := readAuthFile(t.Context(), loc, nil)
	if e != nil || tok.Bearer() != "confine-token" {
		t.Fatalf("read through a searchable, unreadable ancestor: %v", e)
	}
}

// Confinement compares folders, not paths: on a case-insensitive volume a CODEX_HOME, or the confinement, spelt in
// another case names the same folder and is inside, as Swift, whose realpath returns the stored case, finds it; so
// is a CODEX_HOME reached through the data volume's own mount point on macOS, which resolving links keeps. The
// folder beside it, spelt the same way, stays outside.
func TestConfinementComparesFoldersNotSpellings(t *testing.T) {
	base := tree(t)
	if _, err := os.Stat(filepath.Join(base, "CONFINE")); err != nil {
		t.Skip("the temporary folder is on a case-sensitive volume")
	}
	cases := []struct{ home, confine string }{
		{filepath.Join(base, "CONFINE/a/b"), "confine"},
		{filepath.Join(base, "confine/A/B"), "confine"},
		{filepath.Join(base, "confine/a/b"), "Confine"},
	}
	if data := "/System/Volumes/Data" + base; dirExists(data) {
		cases = append(cases, struct{ home, confine string }{filepath.Join(data, "confine/a/b"), "confine"})
	}
	for _, tc := range cases {
		loc, e := authFilePath(tc.home, base, filepath.Join(base, tc.confine))
		if e != nil {
			t.Fatalf("%s confined to %s: %v", tc.home, tc.confine, e)
		}
		tok, e := readAuthFile(t.Context(), loc, nil)
		loc.close()
		if e != nil || tok.Bearer() != "confine-token" {
			t.Fatalf("%s confined to %s: read %v", tc.home, tc.confine, e)
		}
	}
	if _, e := authFilePath(filepath.Join(base, "OUTSIDE", "a", "b"), base, filepath.Join(base, "Confine")); e == nil {
		t.Fatal("the folder beside the confinement was read")
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
