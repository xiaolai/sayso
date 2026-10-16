// Package prereq is how a test reports a missing prerequisite: a skip on a developer's machine, and a failure in CI,
// where a skipped test would let the job pass without having run it.
package prereq

import (
	"os"
	"os/exec"
	"testing"
)

// InCI reports whether the tests run in CI: the CI variable is set and not empty, as GitHub Actions sets CI=true.
func InCI() bool { return os.Getenv("CI") != "" }

// Missing skips t saying what is missing, or fails it in CI.
func Missing(t testing.TB, what string) {
	t.Helper()
	if InCI() {
		t.Fatalf("%s, which CI must provide (CI=%q)", what, os.Getenv("CI"))
	}
	t.Skip(what)
}

// Python returns the path of python3, which the fake Codex scripts run on.
func Python(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		Missing(t, "needs python3")
	}
	return p
}
