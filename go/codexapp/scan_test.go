package codexapp

import (
	"bytes"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"testing"
	"unsafe"

	"github.com/xiaolai/sayso/go/internal/memscan"
	"github.com/xiaolai/sayso/go/internal/prereq"
	"github.com/xiaolai/sayso/go/secret"
)

// The no-copy tests build what the child sends in a memory scan's pages (package memscan) and count, after, how
// often each needle, and each needle's first 16 bytes, are found in the process's writable memory, dropped garbage
// included: a fragment left in a buffer outgrown mid-needle holds the first bytes without the rest.

// part is a piece of a reply built in the scan's pages: text, or a fresh needle, written as it is ('n') or as \u
// escapes ('e'). 't' and 'T' are such needles the status keeps as its token; 'r' is a needle registered with the
// scrubber before the reply is read.
type part struct {
	text string
	kind byte
}

func text(s string) part   { return part{text: s} }
func needle() part         { return part{kind: 'n'} }
func escaped() part        { return part{kind: 'e'} }
func token() part          { return part{kind: 't'} }
func escapedToken() part   { return part{kind: 'T'} }
func registered() part     { return part{kind: 'r'} }
func doc(p ...part) []part { return p }

// wipeScan builds replies in a scan's pages and knows what the scan should find.
type wipeScan struct {
	s       *memscan.Scan
	needles [][2]int
	want    []int // as Occurrences counts: each needle, then its first 16 bytes; 1 for the token kept, else 0
}

func newWipeScan(t *testing.T, room int) *wipeScan {
	t.Helper()
	s, err := memscan.New(room)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// No collection runs until the test is done: a collection can return a span of garbage to the system, which
	// zeroes it, and a copy dropped unwiped would then go unseen.
	old := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(old) })
	return &wipeScan{s: s}
}

// emit appends parts, then a newline, to the pages and returns their place.
func (w *wipeScan) emit(parts []part) [2]int {
	start := w.s.Mark()
	for _, p := range parts {
		if p.kind == 0 {
			w.s.Append(p.text)
			continue
		}
		r := w.s.Needle(40)
		w.s.Prefix(r, 16)
		w.needles = append(w.needles, r)
		kept := 0
		if p.kind == 't' || p.kind == 'T' {
			kept = 1
		}
		w.want = append(w.want, kept, kept)
		if p.kind == 'r' {
			b := w.s.Bytes(r)
			secret.Register(unsafe.String(unsafe.SliceData(b), len(b))) // read in place: the registry keeps no copy
		}
		if p.kind == 'e' || p.kind == 'T' {
			for _, b := range w.s.Bytes(r) {
				w.s.Append(fmt.Sprintf(`\u%04x`, b))
			}
		} else {
			w.s.AppendBytes(w.s.Bytes(r))
		}
	}
	w.s.Append("\n")
	return [2]int{start, w.s.Mark()}
}

// check fails t unless the scan finds what it should.
func (w *wipeScan) check(t *testing.T) {
	t.Helper()
	found, err := w.s.Occurrences()
	if errors.Is(err, memscan.ErrUnsupported) {
		prereq.Missing(t, err.Error())
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(found, w.want) {
		t.Errorf("each needle, then each one's first 16 bytes, found %v times; want %v", found, w.want)
	}
}

var wipeSink []byte

// The scan finds a dropped copy, and a dropped fragment by the needle's first bytes: what the other tests rely on.
func TestTheScanFindsADroppedFragment(t *testing.T) {
	w := newWipeScan(t, 1<<12)
	r := w.s.Needle(40)
	w.s.Prefix(r, 16)
	wipeSink = bytes.Clone(w.s.Bytes(r)[:20])
	wipeSink = nil
	w.want = []int{0, 1}
	w.check(t)
	wipeSink = bytes.Clone(w.s.Bytes(r))
	wipeSink = nil
	w.want = []int{1, 2}
	w.check(t)
}
