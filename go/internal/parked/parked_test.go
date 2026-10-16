package parked

import (
	"runtime"
	"testing"
)

//go:noinline
func offer(ch chan<- int, stop <-chan struct{}) {
	select {
	case ch <- 1:
	case <-stop:
	}
}

//go:noinline
func receive(ch <-chan int) { <-ch }

// Wait finds a goroutine parked in a select of the named function; In does not mistake a goroutine parked deeper,
// in a function the named one called, for it.
func TestWaitFindsTheParkedFunction(t *testing.T) {
	ch, stop := make(chan int), make(chan struct{})
	defer close(stop)
	go offer(ch, stop)
	Wait(t, "parked.offer")

	inner := make(chan int)
	defer close(inner)
	go func() { receive(inner) }()
	Wait(t, "parked.receive")
	buf := make([]byte, 1<<20)
	if dump := string(buf[:runtime.Stack(buf, true)]); In(dump, "parked.TestWaitFindsTheParkedFunction.func1") {
		t.Fatal("a goroutine parked in receive was reported as parked in its caller")
	}
}

func TestInSkipsRuntimeFramesAndRunningGoroutines(t *testing.T) {
	dump := "goroutine 7 [select]:\nruntime.gopark(0x0)\n\t/go/src/runtime/proc.go:1 +0x1\n" +
		"example.com/m/realtime.(*Session).readLoop(0x1)\n\t/m/session.go:203 +0x2\n\n" +
		"goroutine 8 [running]:\nexample.com/m/realtime.(*Session).Next(0x1)\n\t/m/session.go:250 +0x3\n"
	if !In(dump, "realtime.(*Session).readLoop") {
		t.Error("a parked readLoop was not found past the runtime's frames")
	}
	if In(dump, "realtime.(*Session).Next") {
		t.Error("a running goroutine was reported as parked")
	}
	if In(dump, "m/realtime.(*Session).readLoo") {
		t.Error("a name that is only a prefix matched")
	}
	if n := Count(dump+"\n\n"+dump, "realtime.(*Session).readLoop"); n != 2 {
		t.Errorf("Count found %d parked readLoops, want 2", n)
	}
}
