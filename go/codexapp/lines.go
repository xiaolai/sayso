package codexapp

import (
	"bytes"
	"errors"
	"io"
)

// errLineTooLong is a line of more than maxLine bytes before its newline.
var errLineTooLong = errors.New("codexapp: line too long")

// lineReader splits the child's output into lines inside one buffer of its own, so nothing the child wrote is left
// in memory the package dropped: a buffer outgrown is cleared before it is dropped, and so are the bytes left behind
// when the unread ones move to the front. The caller clears each line once it is handled, and wipe clears the rest.
// Lines are those bufio.Scanner reads with a buffer of maxLine+1 bytes: a line of exactly maxLine bytes fits with
// its newline, a carriage return before the newline is dropped, and a last line ended by the end of the stream, or by
// a read error, is still a line.
type lineReader struct {
	r          io.Reader
	buf        []byte
	start, end int   // the bytes not yet returned are buf[start:end]
	err        error // what ended the stream, once it has ended
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r, buf: make([]byte, 64<<10)} }

// next returns the next line, without its newline, as a view of the buffer that is valid until the next call; or,
// once the lines are done, the error that ended them: io.EOF, errLineTooLong, or the read error.
func (l *lineReader) next() ([]byte, error) {
	for empty := 0; ; {
		if i := bytes.IndexByte(l.buf[l.start:l.end], '\n'); i >= 0 {
			line := l.buf[l.start : l.start+i]
			l.start += i + 1
			return dropCR(line), nil
		}
		if l.end-l.start > maxLine {
			return nil, errLineTooLong
		}
		if l.err != nil {
			if l.start == l.end {
				return nil, l.err
			}
			line := l.buf[l.start:l.end]
			l.start = l.end
			return dropCR(line), nil
		}
		if l.end == len(l.buf) {
			l.makeRoom()
		}
		n, err := l.r.Read(l.buf[l.end:])
		l.end += n
		switch {
		case err != nil:
			l.err = err
		case n > 0:
			empty = 0
		default:
			if empty++; empty > 100 { // as bufio.Scanner gives up on a reader that returns nothing
				l.err = io.ErrNoProgress
			}
		}
	}
}

// makeRoom moves the unread bytes to the front and clears the copy they leave behind; when they fill the buffer it
// moves them into one twice the size, at most maxLine+1 bytes, and clears the one outgrown.
func (l *lineReader) makeRoom() {
	unread := l.end - l.start
	if l.start > 0 {
		copy(l.buf, l.buf[l.start:l.end])
		clear(l.buf[unread:l.end])
	} else {
		grown := make([]byte, min(2*len(l.buf), maxLine+1))
		copy(grown, l.buf[:l.end])
		clear(l.buf)
		l.buf = grown
	}
	l.start, l.end = 0, unread
}

// wipe clears the buffer, the bytes not yet returned included.
func (l *lineReader) wipe() {
	clear(l.buf)
	l.start, l.end = 0, 0
}

// drain reads the rest of the stream into the buffer, which the caller has wiped, clearing each read, so a child
// that keeps writing does not block on a full pipe; it returns when the stream ends, or, as next does, after 100
// reads in a row that return nothing.
func (l *lineReader) drain() {
	for empty := 0; l.err == nil; {
		n, err := l.r.Read(l.buf)
		clear(l.buf[:n])
		l.err = err
		switch {
		case err != nil:
		case n > 0:
			empty = 0
		default:
			if empty++; empty > 100 {
				l.err = io.ErrNoProgress
			}
		}
	}
}

// dropCR drops a carriage return that ends b, as bufio.ScanLines does.
func dropCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
