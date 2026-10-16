package canon

import (
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type shape struct{ name, a, b string }

// worstShapes are texts of n bytes at most of the shapes that cost Equivalent most: prose with compounds and numbers,
// one word of many hyphens, letters that spell number words, single letters, zeros, year pairs, a span of many
// fields.
func worstShapes(n int) []shape {
	rep := func(w string) string { return strings.Repeat(w, n/len(w)) }
	var a, b strings.Builder
	words := []string{"the", "e-mail", "came", "1999;", "a", "long-term", "plan", "twenty-one", "people", "said"}
	alts := []string{"the", "email", "came", "nineteen ninety nine;", "a", "long term", "plan", "twentyone", "people", "said"}
	for i := 0; ; i++ {
		w, x := words[i%len(words)]+" ", alts[i%len(alts)]+" "
		if a.Len()+len(w) > n || b.Len()+len(x) > n {
			break
		}
		a.WriteString(w)
		b.WriteString(x)
	}
	joined := func(s string) string { return strings.ReplaceAll(s, "-", "") }
	return []shape{
		{"prose", a.String(), b.String()},
		{"one-word", rep("ab-"), joined(rep("ab-"))},
		// 1,350 single letters in one word at MaxBytes: as many spans as the budget allows, each a run of letters.
		{"letter-word", strings.Repeat("o-n-e-", 450*n/MaxBytes), joined(strings.Repeat("o-n-e-", 450*n/MaxBytes))},
		{"o-n-e", rep("o-n-e "), joined(rep("o-n-e "))},
		{"x-y-z-w", rep("x-y-z-w "), joined(rep("x-y-z-w "))},
		{"a-b", rep("a-b "), rep("a b ")},
		{"twenty-one", rep("twenty-one "), joined(rep("twenty-one "))},
		{"oh-oh", rep("oh-oh "), rep("oh oh ")},
		{"oh.span", rep("oh."), rep("oh ")},
		{"nineteen", rep("nineteen-twenty "), rep("nineteen twenty ")},
		// A long run of single letters still being joined, then compounds of single letters (audit).
		{"letters-then-compounds", rep("q ")[:n/2] + rep("x-y-z ")[:n/2], rep("q ")[:n/2] + rep("x-y-z ")[:n/2]},
	}
}

// equivalentWithin is Equivalent, also reporting the work it spent.
func equivalentWithin(a, b string) (bool, int) {
	w := &budget{left: equivalentWork}
	spent := func() int { return equivalentWork - w.left }
	if len(a) > MaxBytes || len(b) > MaxBytes || !w.spend(len(a)+len(b)) {
		return false, spent()
	}
	ga, ok := readings(a, w)
	if !ok {
		return false, spent()
	}
	gb, ok := readings(b, w)
	return ok && ga.meets(gb, w), spent()
}

// Each costliest shape, at MaxBytes, finishes within a time and a memory bound, and the real ones within a fraction of
// the work budget: the work of one call is bounded by equivalentWork (see budget), not by the shape of its input. The
// bounds are about three times what was measured (2.8 s, 841 MB allocated and 241 MB in use for the costliest, a word
// of 1,350 single letters). Under the race detector the texts are shorter.
func TestEquivalentWorstCase(t *testing.T) {
	n := MaxBytes / raceFactor
	real := map[string]int{"prose": 8, "twenty-one": 8, "nineteen": 8, "oh-oh": 2} // shape: work at most budget/x
	for _, c := range worstShapes(n) {
		if len(c.a) > n || len(c.b) > n {
			t.Fatalf("%s: %d and %d bytes, over %d", c.name, len(c.a), len(c.b), n)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		stop, sampled := make(chan struct{}), make(chan struct{})
		go func() { // the heap in use, sampled while the call runs
			defer close(sampled)
			var m runtime.MemStats
			for {
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
		}()
		start := time.Now()
		eq, work := equivalentWithin(c.a, c.b)
		took := time.Since(start)
		close(stop)
		<-sampled
		runtime.ReadMemStats(&after)
		allocated, inUse := after.TotalAlloc-before.TotalAlloc, peak.Load()-min(peak.Load(), before.HeapInuse)
		t.Logf("%s: equivalent %v, work %d, %v, %d MB allocated, %d MB peak in use", c.name, eq, work, took, allocated>>20, inUse>>20)
		// Work and memory are the bounds; the clock only catches a hang, so a loaded machine cannot fail a correct
		// walk. work never exceeds the budget by more than the one spend that ran it out.
		if took > raceSlowdown*60*time.Second || allocated > 2<<30 || inUse > 768<<20 {
			t.Errorf("%s: %v, %d MB allocated, %d MB in use: over the bound", c.name, took, allocated>>20, inUse>>20)
		}
		if raceFactor == 1 && eq != Equivalent(c.a, c.b) { // once is enough under the race detector
			t.Errorf("%s: equivalentWithin and Equivalent disagree", c.name)
		}
		if x, ok := real[c.name]; ok && (!eq || work > equivalentWork/x) {
			t.Errorf("%s: equivalent %v with work %d; want true within %d", c.name, eq, work, equivalentWork/x)
		}
	}
	// Past MaxBytes a text is refused before any work, even compared with itself.
	long := strings.Repeat("a", MaxBytes+1)
	if eq, work := equivalentWithin(long, long); eq || work != 0 || Equivalent(long, long) {
		t.Errorf("a text over MaxBytes was compared: equivalent %v, work %d", eq, work)
	}
}

// A run of year guesses that each stay open, read across punctuation inside one span, costs work linear in its
// length: the walk merges ways by state field by field. Listing every way through the span doubled them per pair,
// spent far past the budget before checking it, and reported a text unequal to itself (audit).
func TestOpenYearGuessesCostLinearWork(t *testing.T) {
	for _, n := range []int{10, 40, 400 / raceFactor} {
		a := strings.Repeat("nineteen;twenty;", n)
		b := strings.Repeat("1920;", n)
		for _, c := range [][2]string{{a, a}, {a, b}} {
			ok, work := equivalentWithin(c[0], c[1])
			if !ok || work > 300*n {
				t.Errorf("n=%d: equivalent %v with work %d; want true within %d", n, ok, work, 300*n)
			}
		}
	}
	if Equivalent(strings.Repeat("nineteen;twenty;", 40), strings.Repeat("1920;", 39)+"1921;") {
		t.Error("a different last year was accepted")
	}
}
