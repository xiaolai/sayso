package prereq

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Missing skips outside CI and fails inside it. Each outcome runs in a child process, where the test it ends is
// the child's own.
func TestMissingFailsOnlyInCI(t *testing.T) {
	if os.Getenv("SAYSO_PREREQ_CHILD") != "" {
		Missing(t, "needs the moon")
		return
	}
	for _, c := range []struct{ ci, want string }{{"", "--- SKIP"}, {"true", "--- FAIL"}} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMissingFailsOnlyInCI$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "SAYSO_PREREQ_CHILD=1", "CI="+c.ci)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), c.want) || !strings.Contains(string(out), "needs the moon") {
			t.Errorf("CI=%q: want %s naming what is missing; got:\n%s", c.ci, c.want, out)
		}
	}
}
