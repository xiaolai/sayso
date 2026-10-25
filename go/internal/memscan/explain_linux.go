package memscan

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Explain says where Occurrences found each needle, for a test that failed: the mapping, whether it lies in the
// range the 64-bit Go heap is first placed at, whether the hit may sit in the XMM part of an XSAVE area (a signal
// frame or other saved registers, outside the boundary a package can wipe), and the bytes around it. Where the copy
// is decides what it is.
func (s *Scan) Explain() string {
	maps, _ := os.ReadFile("/proc/self/maps")
	m := parseMappings(string(maps))
	var b strings.Builder
	for k, hs := range s.hits {
		for _, a := range hs {
			fmt.Fprintf(&b, "needle %d (len %d) at %#x: %s\n", k, s.needles[k][1]-s.needles[k][0], a, describe(a, m))
		}
	}
	return b.String()
}

// mapping is one line of /proc/self/maps with the range it names.
type mapping struct {
	lo, hi uint64
	line   string
}

// parseMappings reads the text of /proc/self/maps once for every hit Explain places.
func parseMappings(maps string) []mapping {
	var out []mapping
	for _, l := range strings.Split(maps, "\n") {
		f := strings.Fields(l)
		if len(f) < 1 {
			continue
		}
		los, his, ok := strings.Cut(f[0], "-")
		if !ok {
			continue
		}
		lo, _ := strconv.ParseUint(los, 16, 64)
		hi, _ := strconv.ParseUint(his, 16, 64)
		out = append(out, mapping{lo, hi, l})
	}
	return out
}

// describe places one hit in the mappings. The heap range is a hint, not proof: it is where the runtime first places
// the heap on 64-bit systems, and the heap may grow elsewhere. The XSAVE layout is x86's, so it is looked for on
// amd64 and 386 only, and a match (the default x87 control word and MXCSR where the legacy area keeps them) is a
// candidate, not a verified frame.
func describe(a uintptr, maps []mapping) string {
	line := "?"
	for _, m := range maps {
		if uint64(a) >= m.lo && uint64(a) < m.hi {
			line = m.line + fmt.Sprintf(" [+%#x]", uint64(a)-m.lo)
		}
	}
	heapHint := uint64(a) >= 0xc000000000 && uint64(a) < 0xc100000000
	xs := "no xsave layout"
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
		xs = "xsave not looked for on " + runtime.GOARCH
	} else if a >= 160+255 {
		buf := make([]byte, 32)
		for base := (a - 160 - 255) &^ 63; base <= a-160; base += 64 {
			if read(base, buf) < 32 {
				continue
			}
			fcw := uint16(buf[0]) | uint16(buf[1])<<8
			if fcw == 0x037f && buf[24] == 0x80 && buf[25] == 0x1f && buf[26] == 0 && buf[27] == 0 {
				xs = fmt.Sprintf("XSAVE candidate at %#x: FCW %#04x MXCSR 0x1f80, needle at XMM byte %d (XMM%d)", base,
					fcw, a-base-160, (a-base-160)/16)
				break
			}
		}
	}
	ctx := make([]byte, 96)
	n := read(a-48, ctx)
	return fmt.Sprintf("heap-hint=%v | %s | %s\n    ctx[-48:+48]=%q", heapHint, line, xs, ctx[:n])
}
