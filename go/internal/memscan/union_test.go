package memscan

import (
	"slices"
	"testing"
)

// Spans listed twice, or overlapping, are scanned once; spans that only touch are joined, without counting as an
// overlap, so a copy across their boundary is found; spans apart are kept as listed.
func TestUnionScansEachByteOnce(t *testing.T) {
	for _, c := range []struct {
		name       string
		in, want   [][2]uintptr
		overlapped bool
	}{
		{"apart", [][2]uintptr{{30, 40}, {10, 20}}, [][2]uintptr{{10, 20}, {30, 40}}, false},
		{"touching", [][2]uintptr{{10, 20}, {20, 30}}, [][2]uintptr{{10, 30}}, false},
		{"touching and overlapping", [][2]uintptr{{20, 30}, {10, 20}, {25, 40}}, [][2]uintptr{{10, 40}}, true},
		{"a mapping listed before and after it grew", [][2]uintptr{{0xc000000000, 0xc000400000}, {0x1000, 0x2000},
			{0xc000000000, 0xc000800000}}, [][2]uintptr{{0x1000, 0x2000}, {0xc000000000, 0xc000800000}}, true},
		{"listed twice", [][2]uintptr{{10, 20}, {10, 20}}, [][2]uintptr{{10, 20}}, true},
		{"one inside another", [][2]uintptr{{10, 50}, {20, 30}, {60, 70}}, [][2]uintptr{{10, 50}, {60, 70}}, true},
		{"overlapping in a chain", [][2]uintptr{{10, 25}, {20, 35}, {30, 40}}, [][2]uintptr{{10, 40}}, true},
	} {
		got, overlapped := union(c.in)
		if !slices.Equal(got, c.want) || overlapped != c.overlapped {
			t.Errorf("%s: %v, %v; want %v, %v", c.name, got, overlapped, c.want, c.overlapped)
		}
	}
}
