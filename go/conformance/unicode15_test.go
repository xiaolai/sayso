package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/rangetable"

	"github.com/xiaolai/sayso/go/internal/fixtures"
	"github.com/xiaolai/sayso/go/internal/wire"
)

// tools/differential.py keeps said-guard inputs to the characters tools/unicode-15.0-assigned.txt lists, so a table
// difference newer than that (spec/said-guard.md) is never generated. The file must be exactly the set the wire
// pins, x/text's Unicode 15.0 assigned code points.
func TestUnicode15Table(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures.Dir, "..", "tools", "unicode-15.0-assigned.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			got = append(got, line)
		}
	}
	assigned := rangetable.Assigned(wire.PinnedUnicode)
	var want []string
	start := rune(-1)
	for r := rune(0); r <= unicode.MaxRune+1; r++ {
		in := r <= unicode.MaxRune && unicode.Is(assigned, r)
		if in && start < 0 {
			start = r
		} else if !in && start >= 0 {
			want = append(want, fmt.Sprintf("%04X..%04X", start, r-1))
			start = -1
		}
	}
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("tools/unicode-15.0-assigned.txt has %d ranges, Unicode %s %d; regenerate it from rangetable.Assigned",
			len(got), wire.PinnedUnicode, len(want))
	}
}
