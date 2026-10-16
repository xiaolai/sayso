package codexapp

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// eventRecorder records the methods its handler sees; the handler blocks on first until it is released.
type eventRecorder struct {
	mu      sync.Mutex
	methods []string
	entered chan struct{} // closed when the first event is being handled
	release chan struct{} // closed to let the first event's handler return
	once    sync.Once
}

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

// waitFor waits up to 5 s for cond, which the test cannot otherwise synchronise on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
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
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
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
	time.Sleep(50 * time.Millisecond) // a second event would be admitted at once; none may be
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
	c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 8 * time.Second, OnEvent: r.handle})
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
		c, err := Start(t.Context(), Config{Bin: script, Env: EnvOf([]string{"PATH=/usr/bin:/bin"}), Timeout: 8 * time.Second, OnEvent: r.handle})
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
