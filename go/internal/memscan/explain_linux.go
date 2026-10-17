package memscan

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Explain says where Occurrences found each needle, for a test that failed: the mapping, whether it is the Go
// arena, whether the hit sits in the XMM part of an XSAVE area (a signal frame or other saved registers, outside the
// boundary a package can wipe), and the bytes around it. Where the copy is decides what it is.
func (s *Scan) Explain() string {
	maps, _ := os.ReadFile("/proc/self/maps")
	var b strings.Builder
	for k, hs := range s.hits {
		for _, a := range hs {
			fmt.Fprintf(&b, "needle %d (len %d) at %#x: %s\n", k, s.needles[k][1]-s.needles[k][0], a, describe(a, string(maps)))
		}
	}
	return b.String()
}

// describe places one hit in maps, the text of /proc/self/maps.
func describe(a uintptr, maps string) string {
	line := "?"
	for _, l := range strings.Split(maps, "\n") {
		f := strings.Fields(l)
		if len(f) < 1 {
			continue
		}
		lohi := strings.SplitN(f[0], "-", 2)
		if len(lohi) != 2 {
			continue
		}
		lo, _ := strconv.ParseUint(lohi[0], 16, 64)
		hi, _ := strconv.ParseUint(lohi[1], 16, 64)
		if uint64(a) >= lo && uint64(a) < hi {
			line = l + fmt.Sprintf(" [+%#x]", uint64(a)-lo)
		}
	}
	arena := a >= 0xc000000000 && a < 0xc100000000
	xs := "no xsave layout"
	buf := make([]byte, 32)
	for base := (a - 160 - 255) &^ 63; base <= a-160; base += 64 {
		if read(base, buf) < 32 {
			continue
		}
		if buf[24] == 0x80 && buf[25] == 0x1f && buf[26] == 0 && buf[27] == 0 {
			xs = fmt.Sprintf("XSAVE area at %#x: FCW %#04x MXCSR ok, needle at XMM byte %d (XMM%d)", base,
				uint16(buf[0])|uint16(buf[1])<<8, a-base-160, (a-base-160)/16)
			break
		}
	}
	ctx := make([]byte, 96)
	n := read(a-48, ctx)
	return fmt.Sprintf("goarena=%v | %s | %s\n    ctx[-48:+48]=%q", arena, line, xs, ctx[:n])
}
