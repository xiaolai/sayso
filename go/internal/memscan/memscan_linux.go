package memscan

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// mem is /proc/self/mem, opened once whichever scan comes first.
var mem = sync.OnceValues(func() (*os.File, error) {
	m, err := os.Open("/proc/self/mem")
	if err != nil {
		return nil, ErrUnsupported
	}
	return m, nil
})

// regions calls f with each private writable mapping, from /proc/self/maps. The whole listing is parsed before f is
// first called, and a line it cannot read fails the scan rather than end it early.
func regions(f func(lo, hi uintptr)) error {
	if _, err := mem(); err != nil {
		return err
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return ErrUnsupported
	}
	var spans [][2]uintptr
	for line := range strings.Lines(string(maps)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 {
			return fmt.Errorf("memscan: unreadable /proc/self/maps line %q", line)
		}
		if !strings.HasPrefix(fields[1], "rw") {
			continue
		}
		if len(fields) >= 6 && (fields[5] == "[vvar]" || fields[5] == "[vsyscall]") {
			continue
		}
		los, his, ok := strings.Cut(fields[0], "-")
		lo, err1 := strconv.ParseUint(los, 16, 64)
		hi, err2 := strconv.ParseUint(his, 16, 64)
		if !ok || err1 != nil || err2 != nil {
			return fmt.Errorf("memscan: unreadable /proc/self/maps line %q", line)
		}
		spans = append(spans, [2]uintptr{uintptr(lo), uintptr(hi)})
	}
	spans, _ = union(spans)
	for _, s := range spans {
		f(s[0], s[1])
	}
	return nil
}

// read copies memory at a into b and returns how much it could.
func read(a uintptr, b []byte) int {
	m, err := mem()
	if err != nil {
		return 0
	}
	n, _ := m.ReadAt(b, int64(a))
	return n
}
