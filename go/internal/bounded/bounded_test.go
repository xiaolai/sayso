package bounded

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
	"time"
)

// Read returns what io.ReadAll of the first limit bytes returns, whatever length the sender declared, true or not,
// and however the reads come.
func TestReadIsReadAllOfTheLimit(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 300)
	for _, declared := range []int64{-1, 0, 10, 2999, 3000, 3001, 1 << 40} {
		for _, limit := range []int{1, 511, 512, 513, 2999, 3000, 3001, 10000} {
			got, err := Read(iotest.HalfReader(bytes.NewReader(data)), declared, limit)
			want, _ := io.ReadAll(io.LimitReader(bytes.NewReader(data), int64(limit)))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("declared %d, limit %d: %d bytes, %v; want %d", declared, limit, len(got), err, len(want))
			}
		}
	}
}

// A read error ends the reading and comes back with what was read.
func TestReadReturnsTheErrorWithWhatWasRead(t *testing.T) {
	boom := errors.New("boom")
	got, err := Read(io.MultiReader(bytes.NewReader([]byte("partial")), iotest.ErrReader(boom)), -1, 1<<20)
	if !errors.Is(err, boom) || string(got) != "partial" {
		t.Fatalf("%q, %v", got, err)
	}
}

// scratchReader returns "ok" once, with err, after writing secret bytes over the rest of the slice it was given,
// which io.Reader allows.
type scratchReader struct {
	err  error
	done bool
}

func (r *scratchReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	for i := range p {
		p[i] = 's'
	}
	return copy(p, "ok"), r.err
}

// What a reader scribbled beyond the bytes it returned is cleared, whether the reading ends at the end or at an
// error, since the caller clears only the bytes returned.
func TestReadClearsScratchBeyondTheBytesRead(t *testing.T) {
	for _, err := range []error{nil, io.EOF, errors.New("boom")} {
		got, _ := Read(&scratchReader{err: err}, 100, 1<<20)
		if string(got) != "ok" {
			t.Fatalf("%v: read %q", err, got)
		}
		if rest := got[len(got):cap(got)]; bytes.ContainsRune(rest, 's') {
			t.Errorf("%v: scratch left beyond the bytes read", err)
		}
	}
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

// A reader that keeps returning nothing ends the reading with io.ErrNoProgress, as bufio does, rather than spinning.
func TestReadGivesUpOnAReaderThatReturnsNothing(t *testing.T) {
	done := make(chan error, 1)
	go func() { _, err := Read(emptyReader{}, -1, 1<<20); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrNoProgress) {
			t.Errorf("err = %v, want io.ErrNoProgress", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read spins on a reader that returns nothing")
	}
}
