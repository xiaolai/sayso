package memscan

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

var mem *os.File

// regions calls f with each private writable mapping, from /proc/self/maps.
func regions(f func(lo, hi uintptr)) error {
	if mem == nil {
		m, err := os.Open("/proc/self/mem")
		if err != nil {
			return ErrUnsupported
		}
		mem = m
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return ErrUnsupported
	}
	var spans [][2]uintptr
	sc := bufio.NewScanner(strings.NewReader(string(maps)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "rw") {
			continue
		}
		if len(fields) >= 6 && (fields[5] == "[vvar]" || fields[5] == "[vsyscall]") {
			continue
		}
		lohi := strings.SplitN(fields[0], "-", 2)
		lo, err1 := strconv.ParseUint(lohi[0], 16, 64)
		hi, err2 := strconv.ParseUint(lohi[1], 16, 64)
		if err1 == nil && err2 == nil {
			spans = append(spans, [2]uintptr{uintptr(lo), uintptr(hi)})
		}
	}
	spans, _ = union(spans)
	for _, s := range spans {
		f(s[0], s[1])
	}
	return nil
}

// read copies memory at a into b and returns how much it could.
func read(a uintptr, b []byte) int {
	n, _ := mem.ReadAt(b, int64(a))
	return n
}
