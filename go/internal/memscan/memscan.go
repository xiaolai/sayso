// Package memscan searches the process's writable memory, freed memory included, for bytes that exist nowhere else,
// for tests that prove a reader leaves no copy of what it read. Needles are made in pages of the scan's own, which
// the search skips, so it never finds itself. Go zeroes memory when it allocates it, not when it frees it, so a copy
// the reader dropped unwiped is still there for the scan to find, until the allocator reuses it.
package memscan

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

// ErrUnsupported is returned where the process's memory cannot be read.
var ErrUnsupported = errors.New("memscan: unsupported on this platform")

// ErrIncomplete is returned, with the counts of what could be read, when part of the writable memory could not be
// read: a copy there would be missed.
var ErrIncomplete = errors.New("memscan: incomplete scan")

func incomplete(missing uintptr) error {
	if missing == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d bytes of writable memory could not be read", ErrIncomplete, missing)
}

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

const (
	needleRoom = 1 << 12
	chunkSize  = 1 << 20
)

// Scan owns the skipped pages: needles, room for a document built from them, and the scan's read buffer.
type Scan struct {
	pages   []byte
	needles [][2]int
	nUsed   int
	used    int
	room    int
	hits    [][]uintptr // where Occurrences found each needle, for Explain
}

// New maps the skipped pages, with room bytes for a document.
func New(room int) (*Scan, error) {
	page := syscall.Getpagesize()
	n := (needleRoom + room + page - 1) / page * page
	b, err := mapPages(n + chunkSize)
	if err != nil {
		return nil, err
	}
	return &Scan{pages: b, used: needleRoom, room: n}, nil
}

// Close wipes and unmaps the pages.
func (s *Scan) Close() {
	clear(s.pages)
	unmapPages(s.pages)
}

// Needle makes n random lowercase letters, apart from any document, and returns their place. n must be positive.
func (s *Scan) Needle(n int) [2]int {
	if n <= 0 {
		panic("memscan: a needle must have at least one byte")
	}
	if s.nUsed+n > needleRoom {
		panic("memscan: out of needle room")
	}
	r := [2]int{s.nUsed, s.nUsed + n}
	b := s.pages[r[0]:r[1]]
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = 'a' + b[i]%26
	}
	s.nUsed += n
	s.needles = append(s.needles, r)
	return r
}

// Prefix counts the first n bytes of the needle at r as a needle of their own, and returns their place: a copy cut
// short, such as a buffer outgrown when it held only part of the needle, holds them without the whole needle. Every
// copy of the whole needle holds them too. n must be at least 1 and at most the needle's length.
func (s *Scan) Prefix(r [2]int, n int) [2]int {
	if n <= 0 || n > r[1]-r[0] {
		panic("memscan: a prefix must have at least one byte and no more than its needle")
	}
	p := [2]int{r[0], r[0] + n}
	s.needles = append(s.needles, p)
	return p
}

// Digits rewrites the needle at r as digits without a leading zero, a number's text.
func (s *Scan) Digits(r [2]int) {
	for i := r[0]; i < r[1]; i++ {
		s.pages[i] = '1' + s.pages[i]%9
	}
}

// Bytes is the skipped pages' bytes at r.
func (s *Scan) Bytes(r [2]int) []byte { return s.pages[r[0]:r[1]] }

// Mark is where the next appended byte goes.
func (s *Scan) Mark() int { return s.used }

// Append appends text, which must hold no needle.
func (s *Scan) Append(text string) { s.AppendBytes([]byte(text)) }

// AppendBytes appends b; b may be a needle's own bytes.
func (s *Scan) AppendBytes(b []byte) {
	if s.used+len(b) > s.room {
		panic("memscan: out of room")
	}
	s.used += copy(s.pages[s.used:], b)
}

// Reset clears what was appended after mark.
func (s *Scan) Reset(mark int) {
	clear(s.pages[mark:s.used])
	s.used = mark
}

// Occurrences counts each needle in writable memory outside the skipped pages. Memory that could not be read is an
// ErrIncomplete, returned with the counts of the rest.
func (s *Scan) Occurrences() ([]int, error) {
	counts := make([]int, len(s.needles))
	s.hits = make([][]uintptr, len(s.needles))
	longest := 0
	for _, r := range s.needles {
		longest = max(longest, r[1]-r[0])
	}
	skip := [2]uintptr{addr(s.pages), addr(s.pages) + uintptr(len(s.pages))}
	var missing uintptr
	err := regions(func(lo, hi uintptr) {
		for _, part := range [][2]uintptr{{lo, min(hi, max(lo, skip[0]))}, {max(lo, min(hi, skip[1])), hi}} {
			missing += s.scanSpan(part[0], part[1], longest, counts)
		}
	})
	if err != nil {
		return counts, err
	}
	return counts, incomplete(missing)
}

// scanSpan counts the needles in [lo, hi) a chunk at a time, each chunk starting longest-1 bytes before the last one
// ended so a needle across their boundary is in one of them, and returns how many bytes could not be read.
func (s *Scan) scanSpan(lo, hi uintptr, longest int, counts []int) uintptr {
	chunk := s.pages[s.room:]
	var missing uintptr
	for at := lo; at < hi; {
		n := min(uintptr(chunkSize), hi-at)
		missing += readFully(at, chunk[:n])
		last := at+n >= hi
		s.match(chunk[:n], at, last, longest, counts)
		clear(chunk[:n])
		if last {
			break
		}
		at += n - uintptr(longest-1)
	}
	return missing
}

// match records each needle in buf, the memory at at. A hit starting in the last longest-1 bytes of a chunk that is
// not its span's last is left to the next chunk, which starts there.
func (s *Scan) match(buf []byte, at uintptr, last bool, longest int, counts []int) {
	for k, r := range s.needles {
		needle := s.pages[r[0]:r[1]]
		for off := 0; off+len(needle) <= len(buf); {
			i := index(buf[off:], needle)
			if i < 0 {
				break
			}
			if last || off+i < len(buf)-(longest-1) {
				counts[k]++
				s.hits[k] = append(s.hits[k], at+uintptr(off+i))
			}
			off += i + 1
		}
	}
}

// readFully fills b with the memory at a and returns how many bytes it could not read. A read cut short is retried a
// page at a time, so a page that cannot be read costs only itself; its bytes are zeroed, which no needle holds.
func readFully(a uintptr, b []byte) uintptr {
	page := uintptr(syscall.Getpagesize())
	var missing uintptr
	for off := uintptr(read(a, b)); off < uintptr(len(b)); {
		end := min((a+off)/page*page+page-a, uintptr(len(b)))
		got := uintptr(read(a+off, b[off:end]))
		clear(b[off+got : end])
		missing += end - off - got
		off = end
	}
	return missing
}

// union sorts spans and joins those that overlap, so no byte is scanned twice, and says whether any did. Spans that
// touch are joined too, without counting as an overlap, so a copy across their boundary is in one span.
// /proc/self/maps is read a page at a time and is not a snapshot: a mapping that grows or merges with its neighbour
// while it is read, as the Go heap's does, can be listed twice, as it was and as it became, and a copy inside both
// would be counted twice. On amd64 CI it was: one copy at one address, counted twice.
func union(spans [][2]uintptr) ([][2]uintptr, bool) {
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(a, b [2]uintptr) int { return cmp.Compare(a[0], b[0]) })
	var out [][2]uintptr
	overlapped := false
	for _, s := range spans {
		if n := len(out); n > 0 && s[0] <= out[n-1][1] {
			overlapped = overlapped || s[0] < out[n-1][1]
			out[n-1][1] = max(out[n-1][1], s[1])
			continue
		}
		out = append(out, s)
	}
	return out, overlapped
}

// index is bytes.Index without holding the needle in vector registers. An asynchronous preemption saves a goroutine's
// registers to memory, and bytes.Index keeps up to 32 bytes of a needle in vector registers for its whole search, so
// the scan could find its own copy there: the first 16 bytes of a needle, without the rest, now and then. Only the
// first byte is searched for in bulk; the rest is compared a byte at a time.
func index(s, needle []byte) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		j := bytes.IndexByte(s[i:len(s)-len(needle)+1], needle[0])
		if j < 0 {
			return -1
		}
		i += j
		k := 1
		for k < len(needle) && s[i+k] == needle[k] {
			k++
		}
		if k == len(needle) {
			return i
		}
	}
	return -1
}
