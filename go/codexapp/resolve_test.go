package codexapp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaolai/sayso/go/secret"
)

// A resolution error scrubs the caller's text whenever it is formatted, so a secret registered after the error was
// built never shows, a quote inside the secret included, and it stays ErrNotFound.
func TestResolveErrorScrubsAtFormatTime(t *testing.T) {
	for _, canary := range []string{"opaque-canary-late-3", "opaque\"canary-late-4\x01"} {
		for _, explicit := range []string{"/x/" + canary, canary} {
			_, err := Resolve(explicit, "/nonexistent", nil)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("not ErrNotFound: %T", err)
			}
			secret.Register(canary)
			if strings.Contains(err.Error(), "canary") {
				t.Errorf("Error shows a secret registered after the error was built")
			}
			opaque(t, "resolve error", err, "canary")
			if wrapped := fmt.Errorf("lookup: %w", err); strings.Contains(wrapped.Error(), "canary") || !errors.Is(wrapped, ErrNotFound) {
				t.Errorf("a wrapping error shows the secret or loses ErrNotFound")
			}
		}
	}
	_, err := Resolve("/x/plain-name-5", "/nonexistent", nil)
	if !strings.Contains(err.Error(), `"/x/plain-name-5" is not an executable file`) {
		t.Errorf("the caller's text is not quoted: %v", err)
	}
}

// A name found on PATH but refused says why, rather than that it is not there.
func TestResolveSaysWhyANameOnPathIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve("codex", "/nonexistent:"+dir, nil)
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `"codex" is on PATH but is not an executable file`) {
		t.Errorf("Resolve: %v", err)
	}
	if _, err := Resolve("codex", "/nonexistent", nil); err == nil || !strings.Contains(err.Error(), `"codex" is not on PATH`) {
		t.Errorf("Resolve of a name on no PATH folder: %v", err)
	}
}

// A ".." is resolved by the filesystem, after the symbolic link before it, not cleaned away first: link/../codex is
// the codex beside the link's target, not the one beside the link.
func TestResolveFollowsTheLinkBeforeADotDot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "x", "y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "x", "y"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(root, "codex")) // beside the link only
	if p, err := Resolve(root+"/link/../codex", "", nil); err == nil {
		t.Errorf("an explicit path resolved to %q, the codex beside the link", p)
	}
	if p, err := Resolve("", root+"/link/..", nil); err == nil {
		t.Errorf("a PATH entry resolved to %q, the codex beside the link", p)
	}
	writeExec(t, filepath.Join(root, "x", "codex")) // beside the link's target
	if p, err := Resolve(root+"/link/../codex", "", nil); err != nil || p != root+"/link/../codex" {
		t.Errorf("Resolve: %q %v, want the path as written", p, err)
	}
}
