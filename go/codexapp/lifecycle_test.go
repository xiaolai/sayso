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
