package codexapp

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"
	"go.uber.org/goleak"
)

// eventRecorder records the methods its handler sees; the handler blocks on first until it is released.
type eventRecorder struct {
	mu      sync.Mutex
	methods []string
	entered chan struct{} // closed when the first event is being handled
	release chan struct{} // closed to let the first event's handler return
	once    sync.Once
	freed   sync.Once
}

// free releases the first event's handler; it may be called more than once.
func (r *eventRecorder) free() { r.freed.Do(func() { close(r.release) }) }

func newRecorder() *eventRecorder {
	return &eventRecorder{entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *eventRecorder) handle(e Event) {
	r.mu.Lock()
	r.methods = append(r.methods, e.Method())
	first := len(r.methods) == 1
	r.mu.Unlock()
	if first {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
}

func (r *eventRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...)
}

// recv receives from ch, failing the test after 5 s: a barrier that is never reached fails where it is, rather than
// hanging until the package times out.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// waitFor waits up to 5 s for cond, which the test cannot otherwise synchronise on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(bulkTimeout); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// After an ordinary failure (here malformed output), the events queued before it are still delivered, in order;
// nothing read after it is.
func TestEventsQueuedBeforeAFailureAreDelivered(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	script := fakeCodex(t, "for m in ['a', 'b', 'c']:\n"+
		"    print(json.dumps({'method': m}), flush=True)\n"+
		"print('not json', flush=True)\n"+
		"print(json.dumps({'method': 'after'}), flush=True)\n"+
		"time.sleep(30)\n")
	r := newRecorder()
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second, OnEvent: r.handle})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-r.entered
	waitFor(t, "the failure", func() bool { return c.k.failed() != nil })
	close(r.release)
	waitFor(t, "the queued events", func() bool { return len(r.seen()) == 3 })
	if got := strings.Join(r.seen(), ","); got != "a,b,c" {
		t.Errorf("delivered %s, want a,b,c", got)
	}
}

// Once Close has begun no queued event is admitted, the queue and its counts are released, and Close does not wait
// for the handler already running.
func TestCloseAdmitsNoFurtherEvent(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	script := fakeCodex(t, "for m in ['first', 'second', 'third']:\n"+
		"    print(json.dumps({'method': m, 'params': {'pad': 'x' * 1000}}), flush=True)\n"+
		"time.sleep(30)\n")
	r := newRecorder()
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second, OnEvent: r.handle})
	if err != nil {
		t.Fatal(err)
	}
	<-r.entered
	waitFor(t, "the queued events", func() bool {
		c.k.mu.Lock()
		defer c.k.mu.Unlock()
		return len(c.k.events) == 2
	})
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the running handler")
	}
	c.k.mu.Lock()
	held, bytes, count := len(c.k.events), c.k.eventBytes, c.k.eventCount
	c.k.mu.Unlock()
	if held != 0 || bytes != 0 || count != 0 {
		t.Errorf("after Close: %d events, %d bytes, count %d still held", held, bytes, count)
	}
	close(r.release)
	// The barrier is the event goroutine's exit: once it has ended, nothing more can be delivered, so what it
	// delivered is final. A second event would have been admitted before it ended; none may be.
	goleak.VerifyNone(t, baseline)
	if got := strings.Join(r.seen(), ","); got != "first" {
		t.Errorf("delivered %s, want first only", got)
	}
}

// A handler may call Call and Close itself: neither waits for the event goroutine.
func TestHandlerMayCallAndClose(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	script := fakeCodex(t, "print(json.dumps({'method': 'go'}), flush=True)\n"+
		"for line in sys.stdin:\n"+
		"    m = json.loads(line)\n"+
		"    if 'id' in m and 'method' in m:\n"+
		"        print(json.dumps({'id': m['id'], 'result': {'ok': True}}), flush=True)\n")
	type outcome struct {
		call  error
		ok    bool
		close bool
	}
	done := make(chan outcome, 1)
	var c *Client
	ready := make(chan struct{})
	handler := func(e Event) {
		<-ready
		var out struct{ OK bool }
		err := c.Call(t.Context(), "echo", nil, &out)
		_ = c.Close()
		done <- outcome{err, out.OK, true}
	}
	var err error
	c, err = Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second, OnEvent: handler})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	select {
	case o := <-done:
		if o.call != nil || !o.ok || !o.close {
			t.Errorf("from the handler: call %v, result %v", o.call, o.ok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Call or Close from the handler deadlocked")
	}
}

// Events beyond the count bound fail the client with the overflow, even when each is empty, and a reply the child
// writes after them is not handed out: the failure came first. Later calls fail at once.
func TestEmptyEventsTripTheBounds(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	script := fakeCodex(t, "call = json.loads(sys.stdin.readline())\n"+
		"line = json.dumps({'method': ''}) + '\\n'\n"+
		"sys.stdout.write(line * 70000)\n"+
		"sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"+
		"sys.stdout.flush()\n"+
		"time.sleep(30)\n")
	r := newRecorder()
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: bulkTimeout, OnEvent: r.handle})
	if err != nil {
		t.Fatal(err)
	}
	defer close(r.release)
	defer c.Close()
	if err := c.Call(t.Context(), "echo", nil, nil); err == nil || !strings.Contains(err.Error(), "faster than the handler") {
		t.Fatalf("call: %v, want the overflow", err)
	}
	start := time.Now()
	if err := c.Call(t.Context(), "echo", nil, nil); err == nil || !strings.Contains(err.Error(), "faster than the handler") {
		t.Errorf("a later call: %v, want the overflow", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("a later call took %v to fail", d)
	}
}

// The bounds charge the event being handled too, so the exact boundary is deterministic: 65,536 events are held,
// one more fails the client.
func TestEventCountBoundIsExact(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	for _, tc := range []struct {
		n    int
		fail bool
	}{{maxEvents, false}, {maxEvents + 1, true}} {
		script := fakeCodex(t, "call = json.loads(sys.stdin.readline())\n"+
			"line = json.dumps({'method': ''}) + '\\n'\n"+
			"sys.stdout.write(line * "+strconv.Itoa(tc.n)+")\n"+
			"sys.stdout.write(json.dumps({'id': call['id'], 'result': {'ok': True}}) + '\\n')\n"+
			"sys.stdout.flush()\n"+
			"time.sleep(30)\n")
		r := newRecorder()
		c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: bulkTimeout, OnEvent: r.handle})
		if err != nil {
			t.Fatal(err)
		}
		err = c.Call(t.Context(), "echo", nil, nil)
		if failed := err != nil; failed != tc.fail {
			t.Errorf("%d events: call %v, want failure %v", tc.n, err, tc.fail)
		}
		_ = c.Close()
		close(r.release)
	}
}

// An answer to a server request that cannot be written fails the client: the child would wait for it, and every
// later request would be written ahead of it.
func TestUnwritableRefusalFailsTheClient(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// The child never reads again: its input pipe fills with refusals, and the next one cannot be written at all.
	script := fakeCodex(t, "for i in range(3000):\n"+
		"    sys.stdout.write(json.dumps({'id': 'srv-%d' % i, 'method': 'approve', 'params': {'pad': 'x' * 100}}) + '\\n')\n"+
		"sys.stdout.flush()\n"+
		"time.sleep(30)\n")
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 3 * time.Second}) // initialize too: long enough for the child to start under load
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "the failure", func() bool { return c.k.failed() != nil })
	if err := c.k.failed(); !strings.Contains(err.Error(), "did not read the answer to a server request") &&
		!strings.Contains(err.Error(), "cut off") {
		t.Errorf("failure: %v", err)
	}
	start := time.Now()
	if err := c.Call(t.Context(), "echo", nil, nil); err == nil || time.Since(start) > 100*time.Millisecond {
		t.Errorf("a call after the failure: %v after %v", err, time.Since(start))
	}
}

// A client closed as soon as it starts ends at once and keeps nothing queued.
func TestImmediateClose(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	script := fakeCodex(t, "for line in sys.stdin:\n    pass\n")
	r := newRecorder()
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 5 * time.Second, OnEvent: r.handle})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.k.mu.Lock()
	out, events := len(c.k.out), len(c.k.events)
	c.k.mu.Unlock()
	if out != 0 || events != 0 {
		t.Errorf("after Close: %d messages and %d events kept", out, events)
	}
	if err := c.Call(t.Context(), "echo", nil, nil); err == nil {
		t.Error("a call after Close succeeded")
	}
}

// After an overflow, the events held before it are still delivered once the handler is released, before Close; the
// event that overflowed and everything after it are not.
func TestEventsHeldAtAnOverflowAreDelivered(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	script := fakeCodex(t, "call = json.loads(sys.stdin.readline())\n"+
		"sys.stdout.write(''.join(json.dumps({'method': 'e%d' % i}) + '\\n' for i in range("+strconv.Itoa(maxEvents+10)+")))\n"+
		"sys.stdout.write(json.dumps({'id': call['id'], 'result': {}}) + '\\n')\n"+
		"sys.stdout.flush()\n"+
		"time.sleep(30)\n")
	r := newRecorder()
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: bulkTimeout, OnEvent: r.handle})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.free(); _ = c.Close() }) // a failed step must not leave the handler blocked or the child running
	if err := c.Call(t.Context(), "echo", nil, nil); err == nil || !strings.Contains(err.Error(), "faster than the handler") {
		t.Fatalf("call: %v, want the overflow", err)
	}
	r.free() // before Close: the held events drain
	waitFor(t, "the held events", func() bool { return len(r.seen()) == maxEvents })
	_ = c.Close()
	goleak.VerifyNone(t, baseline) // the event goroutine has ended: the count is final
	seen := r.seen()
	if len(seen) != maxEvents {
		t.Fatalf("delivered %d events, want %d", len(seen), maxEvents)
	}
	for i, m := range seen {
		if want := "e" + strconv.Itoa(i); m != want {
			t.Fatalf("event %d is %q, want %q", i, m, want)
		}
	}
}

// An event is admitted when the loop takes it from the queue, under the lock Close takes to begin. One taken before
// Close began is delivered even though Close runs before its handler is called; none queued behind it is.
func TestEventAdmittedBeforeCloseIsDeliveredAndNoneAfter(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	var mu sync.Mutex
	var seen []string
	k := &core{logger: slog.New(slog.DiscardHandler), eventWake: make(chan struct{}, 1), dead: make(chan struct{}),
		onEvent: func(e Event) { mu.Lock(); seen = append(seen, e.Method()); mu.Unlock() }}
	admitted, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	k.testEventAdmitted = func() { once.Do(func() { close(admitted); <-release }) }
	for _, m := range []string{"a", "b", "c"} {
		root, err := wire.Parse([]byte(`{"method":"` + m + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		obj, err := wire.AsObject(root)
		if err != nil {
			t.Fatal(err)
		}
		k.queueEvent(obj, false)
	}
	stopped := make(chan struct{})
	go func() { k.eventLoop(); close(stopped) }()
	// Whatever step fails, the loop is closed and released and has ended before the leak check; both are idempotent.
	t.Cleanup(func() {
		k.closeEvents()
		free()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("the event goroutine did not end")
		}
		goleak.VerifyNone(t, baseline)
	})
	recv(t, "the first event's admission", admitted) // "a" is taken; its handler has not been called
	k.closeEvents()
	free()
	recv(t, "the event goroutine to end", stopped)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(seen, " ") != "a" {
		t.Errorf("delivered %q; want only the event admitted before Close", seen)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.events) != 0 || k.eventBytes != 0 || k.eventCount != 0 {
		t.Errorf("after Close: %d events, %d bytes, count %d held", len(k.events), k.eventBytes, k.eventCount)
	}
}
