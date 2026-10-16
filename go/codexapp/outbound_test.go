package codexapp

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/wire"
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

// A request admitted between two answers is written between them: the order is admission to the one queue, fixed
// before the writer runs.
func TestCallBetweenEarlierAndLaterRefusals(t *testing.T) {
	k, r, start := bareCore(t)
	refusal(k, "r1")
	done := sendAsync(t, k, map[string]string{"call": "B"})
	refusal(k, "r2")
	start()
	got := strings.Join(readLines(t, r, 3), " ")
	if want := `{"id":"r1","error":{}} {"call":"B"} {"id":"r2","error":{}}`; got != want {
		t.Errorf("written %s, want %s", got, want)
	}
	if err := <-done; err != nil {
		t.Errorf("send: %v", err)
	}
}

// A real answer to a server request read after a request was queued is written after it: refuse admits the answer
// behind whatever is already queued.
func TestLaterAnswerIsWrittenAfterAQueuedRequest(t *testing.T) {
	k, r, start := bareCore(t)
	done := sendAsync(t, k, map[string]string{"call": "A"})
	recv(t, "the answer to be queued", refuseAsync(t, k, "srv-1"))
	start()
	got := readLines(t, r, 2)
	if got[0] != `{"call":"A"}` || !strings.Contains(got[1], `"srv-1"`) || !strings.Contains(got[1], "-32601") {
		t.Errorf("written %q; want the call, then the answer to srv-1", got)
	}
	if err := recv(t, "the send", done); err != nil {
		t.Errorf("send: %v", err)
	}
}

// A failure while a write is blocked on a child that stopped reading interrupts it: the caller returns the failure at
// once, not a timeout at its deadline.
func TestFailureInterruptsABlockedWrite(t *testing.T) {
	k, r, start := bareCore(t) // read once only, so a message far larger than the pipe's buffer blocks in its write
	start()
	done := make(chan error, 1)
	go func() {
		done <- k.send(t.Context(), map[string]string{"pad": strings.Repeat("x", 1<<20)}, time.Now().Add(30*time.Second))
	}()
	// A byte read proves the write is under way: the rest cannot fit the pipe, so it is blocked when the failure comes.
	if _, err := r.ReadByte(); err != nil {
		t.Fatalf("the write did not start: %v", err)
	}
	k.fail(codexError("broken", nil))
	if err := recv(t, "the interrupted send", done); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("send: %v, want the failure", err)
	}
}

// Concurrent calls and answers contend for the writer; whatever order they were admitted in is the order written,
// and every call returns.
func TestConcurrentCallsAndRefusalsKeepAdmissionOrder(t *testing.T) {
	k, r, start := bareCore(t)
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%3 == 0 {
				refusal(k, fmt.Sprint("r", i))
				return
			}
			results <- k.send(t.Context(), map[string]int{"call": i}, time.Now().Add(5*time.Second))
		}()
	}
	waitFor(t, "every message to be queued", func() bool { return len(queuedLines(k)) == 32 })
	admitted := queuedLines(k)
	start()
	if got := readLines(t, r, 32); strings.Join(got, "\n") != strings.Join(admitted, "\n") {
		t.Errorf("written out of admission order:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(admitted, "\n"))
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("send: %v", err)
		}
	}
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

// An answer is admitted when its server request is read, before it is encoded: a request queued while the answer is
// still being encoded is written after it.
func TestAnswerIsAdmittedBeforeItIsEncoded(t *testing.T) {
	k, r, start := bareCore(t)
	admitted, release := make(chan struct{}), make(chan struct{})
	k.testRefusalAdmitted = func() { close(admitted); <-release }
	refused := refuseAsync(t, k, "srv-1")
	<-admitted
	done := sendAsync(t, k, map[string]string{"call": "B"})
	close(release)
	<-refused
	start()
	got := readLines(t, r, 2)
	if !strings.Contains(got[0], `"srv-1"`) || !strings.Contains(got[0], "-32601") || got[1] != `{"call":"B"}` {
		t.Errorf("written %q; want the answer to srv-1, then the call", got)
	}
	if err := <-done; err != nil {
		t.Errorf("send: %v", err)
	}
}

// A running writer that reaches an answer still being encoded waits at it: neither the answer nor a request behind it
// is written until the answer's bytes are in.
func TestWriterWaitsAtAnAnswerBeingEncoded(t *testing.T) {
	k, r, start := bareCore(t)
	admitted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(free) // a failed assertion must not leave refuse blocked
	k.testRefusalAdmitted = func() { close(admitted); <-release }
	waiting := make(chan struct{}, 8)
	k.testWriterWaiting = func() { waiting <- struct{}{} }
	refused := refuseAsync(t, k, "srv-1")
	recv(t, "the answer's admission", admitted)
	start()
	done := sendAsync(t, k, map[string]string{"call": "B"}) // its wake-up brings the writer to the slot
	recv(t, "the writer to reach the answer's slot", waiting)
	if q := queuedLines(k); len(q) != 2 || q[0] != "" {
		t.Fatalf("queue %q while the writer waits at the answer; want its empty slot, then the call", q)
	}
	free()
	recv(t, "the answer to be encoded", refused)
	got := readLines(t, r, 2)
	if !strings.Contains(got[0], `"srv-1"`) || got[1] != `{"call":"B"}` {
		t.Errorf("written %q; want the answer to srv-1, then the call", got)
	}
	if err := recv(t, "the send", done); err != nil {
		t.Errorf("send: %v", err)
	}
}

// refuseAsync answers the server request id in the background, as the reader does; the channel closes when refuse
// returns.
func refuseAsync(t *testing.T, k *core, id string) <-chan struct{} {
	t.Helper()
	node, err := wire.Parse([]byte(strconv.Quote(id)))
	if err != nil {
		t.Fatal(err)
	}
	refused := make(chan struct{})
	go func() { k.refuse(node); close(refused) }()
	return refused
}
