// Package parked lets a test wait until a goroutine is blocked in a given function: a state with no signal of its
// own, such as a reader offering a value on an unbuffered channel or a caller waiting for a lock. Waiting for it
// replaces a sleep that guesses how long the goroutine takes to get there.
package parked

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// Limit is how long Wait waits before it fails the test.
const Limit = 10 * time.Second

// Wait returns once some goroutine is blocked in a select or on a channel with fn as its innermost frame outside the
// runtime, and fails the test after Limit. fn is a name as a stack trace prints it, package-qualified, such as
// "realtime.(*Session).readLoop".
func Wait(t testing.TB, fn string) {
	t.Helper()
	WaitN(t, fn, 1)
}

// WaitN is Wait for at least n such goroutines.
func WaitN(t testing.TB, fn string, n int) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(Limit); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		size := runtime.Stack(buf, true)
		for size == len(buf) { // truncated: retry with room for every goroutine
			buf = make([]byte, 2*len(buf))
			size = runtime.Stack(buf, true)
		}
		if Count(string(buf[:size]), fn) >= n {
			return
		}
	}
	t.Fatalf("fewer than %d goroutines were blocked in %s within %v", n, fn, Limit)
}

// In reports whether dump, as runtime.Stack writes it for every goroutine, has a goroutine blocked in a select or on
// a channel whose innermost frame outside the runtime is fn.
func In(dump, fn string) bool { return Count(dump, fn) > 0 }

// Count is how many goroutines of dump In would find.
func Count(dump, fn string) int {
	n := 0
	for _, g := range strings.Split(dump, "\n\n") {
		header, frames, _ := strings.Cut(strings.TrimLeft(g, "\n"), "\n")
		if !strings.Contains(header, "[select") && !strings.Contains(header, "[chan ") {
			continue
		}
		for _, line := range strings.Split(frames, "\n") {
			if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "runtime.") {
				continue
			}
			if strings.Contains(line, "/"+fn+"(") || strings.HasPrefix(line, fn+"(") {
				n++
			}
			break
		}
	}
	return n
}
