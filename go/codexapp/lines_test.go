package codexapp

import (
	"bufio"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// pieces reads data in pieces of the given sizes, the last one repeating, then returns err. Like a pipe, it never
// returns data with an error.
type pieces struct {
	data  []byte
	sizes []int
	err   error
}

func (p *pieces) Read(b []byte) (int, error) {
	if len(p.data) == 0 {
		return 0, p.err
	}
	n := min(len(b), len(p.data), p.sizes[0])
	if len(p.sizes) > 1 {
		p.sizes = p.sizes[1:]
	}
	copy(b, p.data[:n])
	p.data = p.data[n:]
	return n, nil
}

var errStopped = errors.New("stopped")

// The reader splits a stream into the lines bufio.Scanner did with a buffer of maxLine+1 bytes, and ends as it did,
// however the stream is cut into reads and whether it ends or fails.
func TestLinesAreTheScannersLines(t *testing.T) {
	long := strings.Repeat("y", maxLine)
	short := []string{"", "\n", "a", "a\n", "a\r\nb\r", "\r\n\r", "a\n\n\nb", "\ra\r\r\n", "{\"id\":1}\r\n {} \n"}
	big := []string{long + "\nz\n", long + "\r\n", long + "y\n", long, long + "y", "a\n" + long + "\n",
		"a\n" + long + "y\nb\n", strings.Repeat("0123456789abcde\n", 20000), strings.Repeat("z", 100<<10) + "\n"}
	for _, c := range []struct {
		streams []string
		sizes   []int
	}{{short, []int{1, 2, 7, 4096}}, {big, []int{4096, 65537, 1 << 20}}} {
		for _, s := range c.streams {
			for _, size := range c.sizes {
				for _, end := range []error{io.EOF, errStopped} {
					want, wantEnd := scannerLines(s, size, end)
					got, gotEnd := readerLines(s, size, end)
					if !slices.Equal(got, want) || !errors.Is(gotEnd, wantEnd) {
						t.Errorf("a %d-byte stream read %d bytes at a time, then %v: %d lines, then %v; the scanner read %d, "+
							"then %v", len(s), size, end, len(got), gotEnd, len(want), wantEnd)
					}
				}
			}
		}
	}
}

func scannerLines(s string, size int, end error) ([]string, error) {
	sc := bufio.NewScanner(&pieces{data: []byte(s), sizes: []int{size}, err: end})
	sc.Buffer(make([]byte, 0, 64<<10), maxLine+1)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func readerLines(s string, size int, end error) ([]string, error) {
	l := newLineReader(&pieces{data: []byte(s), sizes: []int{size}, err: end})
	var lines []string
	for {
		line, err := l.next()
		switch {
		case errors.Is(err, io.EOF):
			return lines, nil
		case errors.Is(err, errLineTooLong):
			return lines, bufio.ErrTooLong
		case err != nil:
			return lines, err
		}
		lines = append(lines, string(line))
	}
}

// Reading leaves nothing of the stream behind once the caller has cleared each line it was handed: a buffer outgrown
// is cleared, so are the bytes left behind when the unread ones move to the front, and so is each piece read after
// the lines have failed.
func TestTheLineReaderLeavesNothing(t *testing.T) {
	// readAll clears each line as readLoop does.
	readAll := func(l *lineReader) error {
		for {
			line, err := l.next()
			if err != nil {
				return err
			}
			clear(line)
		}
	}

	t.Run("a buffer outgrown", func(t *testing.T) {
		w := newWipeScan(t, 1<<18)
		stream := w.emit(doc(text("{"), needle(), text(strings.Repeat("x", 100<<10))))
		l := newLineReader(&pieces{data: w.s.Bytes(stream), sizes: []int{30000}, err: io.EOF})
		if err := readAll(l); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		l.wipe()
		w.check(t)
	})

	t.Run("the bytes the unread ones leave", func(t *testing.T) {
		// The first read fills the 64 KiB buffer: a first line, and the start of a second that holds the needle near
		// the buffer's end. Once the first is handed out the second moves to the front, and only 11 more bytes are
		// read over its old place before the stream fails, so the needle's old place is never read over.
		w := newWipeScan(t, 1<<17)
		stream := w.emit(doc(text(strings.Repeat("a", 999)+"\n"+strings.Repeat("b", 64000)), needle(),
			text(strings.Repeat("b", 506))))
		l := newLineReader(&pieces{data: w.s.Bytes(stream), sizes: []int{64 << 10, 10}, err: errStopped})
		if err := readAll(l); !errors.Is(err, errStopped) {
			t.Fatal(err)
		}
		w.check(t) // before wipe: what moved must not have left a copy
	})

	t.Run("a line too long, then the rest drained", func(t *testing.T) {
		// One needle starts the line and one is 2 MiB in, both inside the maxLine+1 bytes read before the line is
		// refused; the last is in what is drained after.
		w := newWipeScan(t, 5<<20)
		x := strings.Repeat("x", maxLine)
		stream := w.emit(doc(needle(), text(x[:2<<20]), needle(), text(x[2<<20:]+strings.Repeat("x", 100)), needle()))
		l := newLineReader(&pieces{data: w.s.Bytes(stream), sizes: []int{1 << 20}, err: io.EOF})
		if err := readAll(l); !errors.Is(err, errLineTooLong) {
			t.Fatal(err)
		}
		l.wipe() // as readLoop does before it drains
		l.drain()
		w.check(t)
	})
}
