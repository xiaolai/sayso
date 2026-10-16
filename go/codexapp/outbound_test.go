package codexapp

import (
	"bufio"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// bareCore is a core with no child: its stdin is a pipe the test reads, and writeLoop starts only when the test says,
// so the queue's order is fixed before anything is written.
func bareCore(t *testing.T) (*core, *bufio.Reader, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	k := &core{stdin: w, timeout: 5 * time.Second, logger: slog.New(slog.DiscardHandler),
		outWake: make(chan struct{}, 1), dead: make(chan struct{}), pending: map[string]chan response{}}
	stopped := make(chan struct{})
	var once sync.Once
	start := func() {
		once.Do(func() {
			go func() { k.writeLoop(); close(stopped) }()
		})
	}
	t.Cleanup(func() {
		k.fail(codexError("test over", nil))
		once.Do(func() { close(stopped) })
		<-stopped
		_ = w.Close()
		_ = r.Close()
	})
	return k, bufio.NewReader(r), start
}

// queuedLines is the queue's messages in order, without their newlines.
func queuedLines(k *core) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, o := range k.out {
		out = append(out, strings.TrimSuffix(string(o.b), "\n"))
	}
	return out
}

func refusal(k *core, id string) {
	k.enqueue(&outbound{b: []byte(`{"id":"` + id + `","error":{}}` + "\n"), refusal: true})
}

func readLines(t *testing.T, r *bufio.Reader, n int) []string {
	t.Helper()
	var out []string
	for range n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %d lines: %v", len(out), err)
		}
		out = append(out, strings.TrimSuffix(line, "\n"))
	}
	return out
}

// sendAsync starts a send of v and waits until it is queued; the returned channel yields its outcome.
func sendAsync(t *testing.T, k *core, v any) <-chan error {
	t.Helper()
	before := len(queuedLines(k))
	done := make(chan error, 1)
	go func() { done <- k.send(t.Context(), v, time.Now().Add(5*time.Second)) }()
	waitFor(t, "the request to be queued", func() bool { return len(queuedLines(k)) == before+1 })
	return done
}

// A failure drops the outbound queue: nothing queued is kept or written, every caller still queued returns the
// failure, and the refusal accounting is released.
func TestFailureDropsTheOutboundQueue(t *testing.T) {
	k, r, start := bareCore(t)
	refusal(k, "r1")
	done := sendAsync(t, k, map[string]string{"call": "C"})
	refusal(k, "r2")
	k.fail(codexError("broken", nil))
	k.mu.Lock()
	held, bytes, cap := len(k.out), k.refusalBytes, cap(k.out)
	k.mu.Unlock()
	if held != 0 || bytes != 0 || cap != 0 {
		t.Errorf("after the failure: %d messages, %d refusal bytes, capacity %d kept", held, bytes, cap)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("queued call: %v, want the failure", err)
	}
	if k.enqueue(&outbound{b: []byte("late\n")}) {
		t.Error("a message was queued after the failure")
	}
	_ = r
	start() // the writer finds the failure and an empty queue, and ends without writing
}
