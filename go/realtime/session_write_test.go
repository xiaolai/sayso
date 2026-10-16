package realtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaolai/sayso/go/internal/parked"
	"github.com/xiaolai/sayso/go/realtime"
)

// Close releases every Send still waiting for its turn behind a stalled write, as ErrClosed, along with the stalled
// write itself; a Send after Close is ErrClosed too (spec/session.md).
func TestCloseReleasesEveryQueuedSend(t *testing.T) {
	s, first := stalledSession(t, context.Background())
	const waiting = 3
	queued := make(chan error, waiting)
	for range waiting {
		go func() { queued <- s.Send(context.Background(), map[string]string{"type": "queued"}) }()
	}
	parked.WaitN(t, "realtime.(*sessionCore).send", waiting) // each waits for the turn the stalled write holds
	begin := time.Now()
	_ = s.Close()
	for i := range waiting + 1 {
		var err error
		select {
		case err = <-queued:
		case err = <-first:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d sends still running 5 s after Close", waiting+1-i, waiting+1)
		}
		if !errors.Is(err, realtime.ErrClosed) {
			t.Errorf("a send ended by Close returned %v, want ErrClosed", err)
		}
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("the sends ended %v after Close began", d)
	}
	if err := s.Send(context.Background(), map[string]string{"type": "late"}); !errors.Is(err, realtime.ErrClosed) {
		t.Errorf("a Send after Close returned %v, want ErrClosed", err)
	}
}

// A Send cancelled while its write is in flight returns its context's error and ends the session: the write cannot be
// taken back, so the connection is dropped and every later call fails (spec/session.md).
func TestCancellingASendInFlightEndsTheSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, first := stalledSession(t, ctx)
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled Send returned %v, want its context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not end the write in flight")
	}
	if err := s.Send(context.Background(), map[string]string{"type": "after"}); err == nil {
		t.Error("a Send after a write cancelled in flight succeeded")
	}
	next, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := s.Next(next); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next after a write cancelled in flight = %v, want the session's end", err)
	}
}
