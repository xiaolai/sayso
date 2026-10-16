// Package bounded reads a reply of bounded size into one buffer the caller owns, so a reply that holds a secret
// leaves no copy of itself in memory the program dropped (spec/credentials.md, hygiene). io.ReadAll grows its buffer
// by copying and drops the old one as it is.
package bounded

import "io"

// Read reads r until it ends, or until limit bytes (limit > 0) are read, into one buffer it returns, which the caller
// clears. The buffer is sized from declared, the length the sender announced (-1 when unknown), with room to see the
// end without growing, at most limit; when it fills before the end it moves into one twice the size, at most limit,
// and the one outgrown is cleared. A read error ends the reading and is returned with what was read; io.EOF, as
// io.ReadAll reads it, is no error.
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
	for n < limit {
		if n == len(buf) {
			grown := make([]byte, min(2*len(buf), limit))
			copy(grown, buf[:n])
			clear(buf)
			buf = grown
		}
		k, err := r.Read(buf[n:])
		n += k
		if err == io.EOF {
			break
		}
		if err != nil {
			return buf[:n], err
		}
	}
	return buf[:n], nil
}
