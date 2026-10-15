//go:build !unix

package saidguard

import "time"

var cpuStart = time.Now()

// cpuTime falls back to wall time where getrusage is unavailable.
func cpuTime() time.Duration { return time.Since(cpuStart) }
