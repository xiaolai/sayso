//go:build unix

package saidguard

import (
	"syscall"
	"time"
)

// cpuTime is the process's user and system CPU time. The bounds test measures it rather than wall time, so a loaded
// machine does not fail it; the package's tests run one at a time.
func cpuTime() time.Duration {
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		return 0
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}
