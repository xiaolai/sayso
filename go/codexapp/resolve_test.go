package codexapp

import (
	"errors"
	"fmt"
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
