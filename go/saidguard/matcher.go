package saidguard

import (
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// --- matching ---

// svMatch simulates the units over the transcript positions as bitsets: bit j of the frontier says the units so far
// can end at token j. A step ANDs the frontier with the positions where an accepted token (or a hyphen chain that
// joins to it) starts and shifts by its length. Work follows the frontier's actual span, so a short script costs
// little and an adversarial one costs its span, not a memo of every (unit, position) pair.
func svMatch(units []svUnit, toks []svTok) bool {
	m := newSvMasks(toks)
	words := len(toks)/64 + 1
	frontier, next := newSvSet(words), newSvSet(words)
	a, b := newSvSet(words), newSvSet(words)
	frontier.set(0)
	for _, unit := range units {
		next.reset()
		for _, seq := range unit {
			cur := frontier
			for p, alt := range seq {
				dst := a
				if p%2 == 1 {
					dst = b
				}
				dst.reset()
				for _, s := range alt {
					for _, e := range m[s] {
						dst.orShifted(cur, e)
					}
				}
				cur = dst
				if cur.empty() {
					break
				}
			}
			next.or(cur)
		}
		frontier, next = next, frontier
		if frontier.empty() {
			return false
		}
	}
	return frontier.has(len(toks))
}

const svMaxChain = 8 // hyphen chains joined for matching ("e-mail" = "email"); longer chains match part by part

// svMask: the transcript positions where a token string (k = 1) or a hyphen chain of k tokens joining to it starts,
// stored sparsely by 64-bit word.
type svMask struct {
	k    int
	idx  []int
	bits []uint64
}

func newSvMasks(toks []svTok) map[string][]*svMask {
	m := map[string][]*svMask{}
	add := func(s string, k, pos int) {
		var e *svMask
		for _, x := range m[s] {
			if x.k == k {
				e = x
			}
		}
		if e == nil {
			e = &svMask{k: k}
			m[s] = append(m[s], e)
		}
		w := pos / 64
		if n := len(e.idx); n == 0 || e.idx[n-1] != w {
			e.idx, e.bits = append(e.idx, w), append(e.bits, 0)
		}
		e.bits[len(e.bits)-1] |= 1 << (pos % 64)
	}
	for j, t := range toks {
		for _, v := range svMarkReadings(t.text, t.lead, t.trail) {
			add(v, 1, j)
		}
		joined := t.text
		for k := 2; k <= svMaxChain && j+k-1 < len(toks) && toks[j+k-2].link; k++ {
			joined += toks[j+k-1].text
			// Normalised again: joined jamo or a base and its mark compose ("ᄀ-ᅡ" is 가), as script tokens are.
			for _, v := range svMarkReadings(norm.NFKC.String(joined), t.lead, toks[j+k-1].trail) {
				add(v, k, j)
			}
		}
	}
	return m
}

// svMarkReadings is s, then s with the runs of marks dropped quotation marks may have been, as apostrophes: the lead
// marks before it, the trail marks after it, or both. At most four readings, so the work stays linear in the marks.
func svMarkReadings(s string, lead, trail int) []string {
	before, after := strings.Repeat("'", lead), strings.Repeat("'", trail)
	out := []string{s}
	if lead > 0 {
		out = append(out, before+s)
	}
	if trail > 0 {
		out = append(out, s+after)
	}
	if lead > 0 && trail > 0 {
		out = append(out, before+s+after)
	}
	return out
}

type svSet struct {
	w      []uint64
	lo, hi int // words outside [lo, hi] are zero
}

func newSvSet(words int) *svSet { return &svSet{w: make([]uint64, words), lo: words, hi: -1} }

func (s *svSet) reset() {
	for i := s.lo; i <= s.hi; i++ {
		s.w[i] = 0
	}
	s.lo, s.hi = len(s.w), -1
}

func (s *svSet) put(i int, v uint64) {
	if v == 0 {
		return
	}
	s.w[i] |= v
	s.lo, s.hi = min(s.lo, i), max(s.hi, i)
}

func (s *svSet) set(pos int)      { s.put(pos/64, 1<<(pos%64)) }
func (s *svSet) has(pos int) bool { return s.w[pos/64]&(1<<(pos%64)) != 0 }
func (s *svSet) empty() bool      { return s.lo > s.hi }
func (s *svSet) or(src *svSet) {
	for i := src.lo; i <= src.hi; i++ {
		s.put(i, src.w[i])
	}
}

// orShifted: s |= (src AND the mask's starts) shifted by the mask's length.
func (s *svSet) orShifted(src *svSet, e *svMask) {
	if src.empty() {
		return
	}
	for n := sort.SearchInts(e.idx, src.lo); n < len(e.idx) && e.idx[n] <= src.hi; n++ {
		i := e.idx[n]
		x := src.w[i] & e.bits[n]
		if x == 0 {
			continue
		}
		s.put(i, x<<e.k)
		if i+1 < len(s.w) {
			s.put(i+1, x>>(64-e.k))
		}
	}
}
