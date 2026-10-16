package canon

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// The oracle is Equivalent's definition, computed by listing: every combination of a piece's compound hyphens, kept
// or removed, canonicalised with Canon itself, and beside each reading a path for each year pair in it.

func enumReadings(s string) *graph {
	g := &graph{next: [][]arc{nil}}
	at := 0
	for _, piece := range pieces(s) {
		end := g.node()
		seen := map[string]bool{}
		for _, v := range everyCompoundVariant(piece) {
			toks := canon(v)
			if k := strings.Join(texts(toks), " ") + "|" + brks(toks); !seen[k] {
				seen[k] = true
				g.enumReading(at, end, toks)
			}
		}
		at = end
	}
	g.end = at
	return g
}

func brks(toks []tok) string {
	var b strings.Builder
	for _, t := range toks {
		if t.brk {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// enumReading adds toks from node a to node b, and beside every year pair in them a path spelling its cardinal: the
// rule the year stage follows one token at a time.
func (g *graph) enumReading(a, b int, toks []tok) {
	at := make([]int, len(toks)+1)
	at[0], at[len(toks)] = a, b
	for i := 1; i < len(toks); i++ {
		at[i] = g.node()
	}
	if len(toks) == 0 {
		g.path(a, b, nil)
		return
	}
	words := texts(toks)
	for i, t := range toks {
		g.next[at[i]] = append(g.next[at[i]], arc{t.text, at[i+1]})
	}
	for i := range toks {
		if v, n := yearPairAt(words, i); n > 0 && (i == 0 || toks[i].brk || !isNumberWord(words[i-1]) && !isScale(words[i-1])) {
			g.path(at[i], at[i+n], cardinal(uint64(v)))
		}
	}
}

// everyCompoundVariant is s with each hyphen between two letters kept or removed, in every combination.
func everyCompoundVariant(s string) []string {
	rs := []rune(width.Fold.String(norm.NFKC.String(strings.ToValidUTF8(s, " "))))
	var at []int
	for i := 1; i+1 < len(rs); i++ {
		if isHyphen(rs[i]) && (unicode.IsLetter(rs[i-1]) || unicode.IsMark(rs[i-1])) && unicode.IsLetter(rs[i+1]) {
			at = append(at, i)
		}
	}
	out := make([]string, 0, 1<<len(at))
	for m := 0; m < 1<<len(at); m++ {
		var b strings.Builder
		k := 0
		for i, r := range rs {
			if k < len(at) && at[k] == i {
				k++
				if m&(1<<(k-1)) != 0 {
					continue
				}
			}
			b.WriteRune(r)
		}
		out = append(out, b.String())
	}
	return out
}

// language is every token string a graph spells from node 0 to its end, joined by spaces.
func (g *graph) language() map[string]bool {
	out := map[string]bool{}
	var walk func(n int, words []string)
	walk = func(n int, words []string) {
		if n == g.end {
			out[strings.Join(words, " ")] = true
		}
		for _, e := range g.next[n] {
			if e.tok == "" {
				walk(e.to, words)
			} else {
				walk(e.to, append(slices.Clone(words), e.tok))
			}
		}
	}
	walk(0, nil)
	return out
}

func sameLanguage(t *testing.T, s string) {
	t.Helper()
	g, ok := readings(s, &budget{left: equivalentWork})
	if !ok {
		t.Fatalf("%q: out of budget", s)
	}
	got, want := g.language(), enumReadings(s).language()
	for w := range want {
		if !got[w] {
			t.Fatalf("%q: the lattice lacks the reading %q", s, w)
		}
	}
	for w := range got {
		if !want[w] {
			t.Fatalf("%q: the lattice reads %q, which no combination does", s, w)
		}
	}
}

// For random texts of compounds and the words Canon's token rules read (number, scale and zero words, "and", "a",
// "point", single letters, abbreviations, digits and punctuation), the lattice spells exactly the readings that
// listing every combination gives.
func TestLatticeMatchesEveryCombination(t *testing.T) {
	vocab := []string{
		"o-n-e", "t-w-o", "o-h", "a-nd", "fif-teen", "twenty-one", "twenty-something", "nineteen-ninety", "o-ring",
		"oh-five", "x-ray", "a-list", "and-or", "point-blank", "e-mail", "St-Louis", "Dr-x", "v-s", "e-t-c", "1st-rate",
		"x2-y", "一-二", "a-b-c", "o", "oh", "a", "and", "point", "x", "b", "St", "St.", "Dr", "vs", "hundred", "five",
		"nineteen", "ninety", "zero", "twenty", "ten", "1999", "2010,", "20;", "ring", "Louis", "e.g.", "U.S.", "M's",
		"x.com", "one-hundred-and-five", "a-hundred", "seven-oh-four", "nineteen-oh-five", "twenty", "oh", "one",
	}
	rng := rand.New(rand.NewPCG(105, 15))
	for range 3000 {
		var words []string
		hyphens := 0
		for len(words) < 2+rng.IntN(5) {
			w := vocab[rng.IntN(len(vocab))]
			if h := strings.Count(w, "-"); hyphens+h <= 10 {
				words, hyphens = append(words, w), hyphens+h
			}
		}
		sameLanguage(t, strings.Join(words, " "))
	}
}

// The same on texts of letters and hyphens alone, where every span is a single letter or a run of them, and on the
// fixed cases of the other tests.
func TestLatticeMatchesOnLetterChains(t *testing.T) {
	rng := rand.New(rand.NewPCG(94, 105))
	letters := []string{"o", "h", "n", "e", "a", "d", "t", "w", "x", "s", "v"}
	for range 2000 {
		var b strings.Builder
		for i, n := 0, 1+rng.IntN(10); i < n; i++ {
			b.WriteString(letters[rng.IntN(len(letters))])
			if i < n-1 {
				b.WriteString([]string{"-", " ", "-"}[rng.IntN(3)])
			}
		}
		sameLanguage(t, b.String())
	}
	for _, s := range []string{
		"twenty-one twenty-one twenty-one twenty-one twenty-one", "e-mail long-term well-known part-time full-time x-ray",
		"2010, 20 people; nineteen ninety nine", "nineteen twenty people, twenty ten", "St-Louis St Louis Main St",
		"one hundred and five a hundred oh seven", "e-mail, long-term; well-known part-time. Full-time",
	} {
		sameLanguage(t, s)
	}
}
