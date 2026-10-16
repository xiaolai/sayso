package bounded

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
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
