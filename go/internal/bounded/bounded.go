// Package bounded reads a reply of bounded size into one buffer the caller owns, so a reply that holds a secret
// leaves no copy of itself in memory the program dropped (spec/credentials.md, hygiene). io.ReadAll grows its buffer
// by copying and drops the old one as it is.
package bounded

import "io"

// Read reads r until it ends, or until limit bytes (limit > 0) are read, into one buffer it returns, which the caller
// clears. The buffer is sized from declared, the length the sender announced (-1 when unknown), with room to see the
// end without growing, at most limit; when it fills before the end it moves into one twice the size, at most limit,
// and the one outgrown is cleared. A read error ends the reading and is returned with what was read; io.EOF, as
// io.ReadAll reads it, is no error. A reader that returns nothing 100 times in a row ends the reading with
// io.ErrNoProgress, as bufio does. The bytes beyond those returned, which a reader may use as scratch, are cleared.
func Read(r io.Reader, declared int64, limit int) ([]byte, error) {
	size := min(512, limit)
	switch {
	case declared >= int64(limit):
		size = limit
	case declared >= 0:
		size = int(declared) + 1
	}
	buf := make([]byte, size)
	n := 0
	var err error
	for empty := 0; n < limit; {
		if n == len(buf) {
			grown := make([]byte, min(2*len(buf), limit))
			copy(grown, buf[:n])
			clear(buf)
			buf = grown
		}
		var k int
		k, err = r.Read(buf[n:])
		n += k
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		if k > 0 {
			empty = 0
		} else if empty++; empty >= 100 {
			err = io.ErrNoProgress
			break
		}
	}
	clear(buf[n:])
	return buf[:n], err
}
