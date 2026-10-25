package memscan

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// noPreempt is the GODEBUG setting Main runs the tests under.
const noPreempt = "asyncpreemptoff=1"

// Main runs m's tests with Go's asynchronous preemption off, re-running the test binary with GODEBUG set when it is
// not, since the runtime reads the setting only as it starts. A preemption saves the interrupted goroutine's vector
// registers to memory a package cannot wipe, and the bytes in flight through them with them: on amd64, whole needles
// a reader was scanning or copying were found there in most runs (spec/credentials.md). With it off, the scan
// measures the buffers the package owns. A package whose tests scan memory calls it from its TestMain.
func Main(m *testing.M) {
	godebug := os.Getenv("GODEBUG")
	if preemptOff(godebug) {
		os.Exit(m.Run())
	}
	if godebug != "" {
		godebug += ","
	}
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), "GODEBUG="+godebug+noPreempt)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		os.Exit(max(exit.ExitCode(), 1)) // -1 for a test binary ended by a signal
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "memscan: re-running the tests:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// preemptOff reports whether godebug turns asynchronous preemption off. The runtime applies the last entry for a
// key, so the last asyncpreemptoff entry decides.
func preemptOff(godebug string) bool {
	off := false
	for entry := range strings.SplitSeq(godebug, ",") {
		if key, value, _ := strings.Cut(entry, "="); key == "asyncpreemptoff" {
			off = value == "1"
		}
	}
	return off
}
