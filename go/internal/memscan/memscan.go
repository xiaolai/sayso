// Package memscan searches the process's writable memory, freed memory included, for bytes that exist nowhere else,
// for tests that prove a reader leaves no copy of what it read. Needles are made in pages of the scan's own, which
// the search skips, so it never finds itself. Go zeroes memory when it allocates it, not when it frees it, so a copy
// the reader dropped unwiped is still there for the scan to find, until the allocator reuses it.
package memscan

import (
	"bytes"
	"crypto/rand"
	"errors"
	"syscall"
)

// ErrUnsupported is returned where the process's memory cannot be read.
var ErrUnsupported = errors.New("memscan: unsupported on this platform")

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
}

// New maps the skipped pages, with room bytes for a document.
func New(room int) (*Scan, error) {
	page := syscall.Getpagesize()
	n := (needleRoom + room + page - 1) / page * page
	b, err := syscall.Mmap(-1, 0, n+chunkSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	return &Scan{pages: b, used: needleRoom, room: n}, nil
}

// Close wipes and unmaps the pages.
func (s *Scan) Close() {
	clear(s.pages)
	_ = syscall.Munmap(s.pages)
}

// Needle makes n random lowercase letters, apart from any document, and returns their place.
func (s *Scan) Needle(n int) [2]int {
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
// copy of the whole needle holds them too.
func (s *Scan) Prefix(r [2]int, n int) [2]int {
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

// Occurrences counts each needle in writable memory outside the skipped pages.
func (s *Scan) Occurrences() ([]int, error) {
	counts := make([]int, len(s.needles))
	longest := 0
	for _, r := range s.needles {
		longest = max(longest, r[1]-r[0])
	}
	skip := [2]uintptr{addr(s.pages), addr(s.pages) + uintptr(len(s.pages))}
	chunk := s.pages[s.room:]
	err := regions(func(lo, hi uintptr) {
		for _, part := range [][2]uintptr{{lo, min(hi, max(lo, skip[0]))}, {max(lo, min(hi, skip[1])), hi}} {
			for at := part[0]; at < part[1]; {
				n := min(uintptr(chunkSize), part[1]-at)
				got := read(at, chunk[:n])
				for k, r := range s.needles {
					needle := s.pages[r[0]:r[1]]
					for off := 0; off+len(needle) <= got; {
						i := index(chunk[off:got], needle)
						if i < 0 {
							break
						}
						// A hit starting in the overlap is counted by the next chunk, which starts there.
						if at+n >= part[1] || off+i < int(n)-(longest-1) {
							counts[k]++
						}
						off += i + 1
					}
				}
				clear(chunk[:got])
				if at+n >= part[1] {
					break
				}
				at += n - uintptr(longest-1)
			}
		}
	})
	return counts, err
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
