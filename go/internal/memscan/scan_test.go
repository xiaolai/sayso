//go:build linux || (darwin && cgo)

package memscan

import (
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The scan of a span reads around a page it cannot read: a needle in a later page of the same chunk is still found,
// and the unreadable page is counted, never skipped in silence. The page is unmapped, not made PROT_NONE: Linux reads
// /proc/self/mem past a page's protection, so only a hole is unreadable on both systems.
func TestScanSpanReadsAroundAnUnreadablePage(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := s.Needle(32)
	page := syscall.Getpagesize()
	m, err := syscall.Mmap(-1, 0, 3*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Munmap(m) }()
	copy(m[2*page+100:], s.Bytes(r))
	if err := unix.MunmapPtr(unsafe.Pointer(&m[page]), uintptr(page)); err != nil {
		t.Fatal(err)
	}
	counts := make([]int, len(s.needles))
	s.hits = make([][]uintptr, len(s.needles))
	missing := s.scanSpan(addr(m), addr(m)+uintptr(len(m)), 32, counts)
	if counts[0] != 1 {
		t.Errorf("found the needle %d times beyond the unreadable page, want 1", counts[0])
	}
	if missing != uintptr(page) {
		t.Errorf("%d bytes counted unreadable, want %d", missing, page)
	}
}

// A needle across the boundary of two touching spans is found once their union is scanned.
func TestScanFindsANeedleAcrossTouchingSpans(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := s.Needle(32)
	page := syscall.Getpagesize()
	m, err := syscall.Mmap(-1, 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Munmap(m) }()
	copy(m[page-10:], s.Bytes(r))
	lo := addr(m)
	spans, _ := union([][2]uintptr{{lo, lo + uintptr(page)}, {lo + uintptr(page), lo + uintptr(2*page)}})
	counts := make([]int, len(s.needles))
	s.hits = make([][]uintptr, len(s.needles))
	for _, sp := range spans {
		s.scanSpan(sp[0], sp[1], 32, counts)
	}
	if counts[0] != 1 {
		t.Errorf("found the straddling needle %d times, want 1", counts[0])
	}
}

// A needle or prefix must have at least one byte, and a prefix no more than its needle's.
func TestNeedleAndPrefixLengthsAreChecked(t *testing.T) {
	s, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := s.Needle(8)
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s was accepted", name)
			}
		}()
		f()
	}
	panics("Needle(0)", func() { s.Needle(0) })
	panics("Needle(-1)", func() { s.Needle(-1) })
	panics("Prefix(r, 0)", func() { s.Prefix(r, 0) })
	panics("Prefix(r, 9)", func() { s.Prefix(r, 9) })
	if p := s.Prefix(r, 8); p != r {
		t.Errorf("the whole needle as a prefix is %v, want %v", p, r)
	}
}

// The scan reports the bytes it could not read as an incomplete scan.
func TestIncompleteIsAnError(t *testing.T) {
	if err := incomplete(4096); !errors.Is(err, ErrIncomplete) {
		t.Errorf("incomplete(4096) = %v", err)
	}
	if err := incomplete(0); err != nil {
		t.Errorf("incomplete(0) = %v", err)
	}
}
