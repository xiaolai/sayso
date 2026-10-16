package canon

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// A piece's readings, built without listing its compounds' combinations.
//
// The input is a lattice of spans. Each compound hyphen is kept or removed, so a word is read as a sequence of spans,
// each running from the word's start or a kept hyphen to the next kept hyphen or the word's end, with the hyphens
// inside it removed. A span's Canon fields are those of its own text (fields): no text pass reads across a space or
// a hyphen, and the one that reads the next word, "St" before a capital, sees the same first letter whichever span
// follows. A word of h compound hyphens has (h+1)(h+2)/2 spans.
//
// The output is Canon's token passes run along every path of spans at once. Each pass is a stage that reads one token
// at a time and keeps a small state: a held "and" or "a" until the next token decides it, the run of single letters
// being joined, the reading guessed at the start of a run of zeros and checked at its end, and the year pair being
// read. A node of the output graph is a span boundary and the stages' states, so the graph grows with the spans and
// the states reached, never with the combinations; the walk in meets compares two such graphs.

// pieceGraph adds to g, from node at, the readings of piece, and returns the node they end at.
func (g *graph) pieceGraph(at int, piece string) int {
	in := spans(piece)
	end := g.node()
	type key struct {
		node int
		c    cascade
	}
	nodes := map[key]int{{0, cascade{}}: at}
	todo := []key{{0, cascade{}}}
	type arcKey struct {
		a, b  int
		words string
	}
	seen := map[arcKey]bool{}
	path := func(a, b int, words []string) { // two ways often emit the same tokens between the same nodes
		k := arcKey{a, b, strings.Join(words, " ")}
		if !seen[k] {
			seen[k] = true
			g.path(a, b, words)
		}
	}
	for len(todo) > 0 {
		k := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		from := nodes[k]
		if k.node == in.final {
			for _, words := range k.c.finish(0) {
				path(from, end, words)
			}
			continue
		}
		for _, e := range in.next[k.node] {
			for _, b := range k.c.read(e.fields) {
				to := key{e.to, b.c}
				n, ok := nodes[to]
				if !ok {
					n = g.node()
					nodes[to] = n
					todo = append(todo, to)
				}
				path(from, n, b.out)
			}
		}
	}
	return end
}

// spanLattice: node 0 is the piece's start and final its end; next[n] are the spans from node n.
type spanLattice struct {
	next  [][]span
	final int
}

type span struct {
	fields []string
	to     int
}

// spans is the lattice of piece's spans (see pieceGraph).
func spans(piece string) spanLattice {
	rs := []rune(width.Fold.String(norm.NFKC.String(strings.ToValidUTF8(piece, " "))))
	type word struct{ start, end int }
	var words []word
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && !unicode.IsSpace(rs[j]) {
			j++
		}
		words = append(words, word{i, j})
		i = j
	}
	// A node is a span start: a word's start, or the letter after one of its compound hyphens.
	var l spanLattice
	first := make([]int, len(words)+1) // first[w]: the node of word w's start; first[len(words)] is final
	hyphens := make([][]int, len(words))
	for w, wd := range words {
		first[w] = len(l.next)
		for i := wd.start + 1; i+1 < wd.end; i++ {
			if isHyphen(rs[i]) && (unicode.IsLetter(rs[i-1]) || unicode.IsMark(rs[i-1])) && unicode.IsLetter(rs[i+1]) {
				hyphens[w] = append(hyphens[w], i)
			}
		}
		l.next = append(l.next, make([][]span, len(hyphens[w])+1)...)
	}
	l.final = len(l.next)
	first[len(words)] = l.final
	l.next = append(l.next, nil)
	for w, wd := range words {
		hy := hyphens[w]
		starts := append([]int{wd.start}, make([]int, len(hy))...)
		for k, h := range hy {
			starts[k+1] = h + 1
		}
		// After the word, "St" reads the next word's first letter; before a kept hyphen it reads the hyphen.
		nextUpper := w+1 < len(words) && unicode.IsUpper(rs[words[w+1].start])
		stops := append(slices.Clone(hy), wd.end)
		for a := range starts {
			var text []rune
			var lower strings.Builder
			ascii := true
			for b := a; b <= len(hy); b++ {
				to, upper := first[w+1], nextUpper
				if b < len(hy) {
					to, upper = first[w]+b+1, false
				}
				part := rs[starts[b]:stops[b]]
				text = append(text, part...)
				ascii = ascii && asciiLetters(part)
				var f []string
				if ascii {
					for _, r := range part {
						lower.WriteRune(unicode.ToLower(r))
					}
					if !IsAbbreviation(lower.String()) {
						// Every text pass leaves ASCII letters that are no abbreviation as they are, but in lower case.
						f = []string{lower.String()}
					}
				}
				if f == nil {
					f = spanFields(string(text), upper)
				}
				l.next[first[w]+a] = append(l.next[first[w]+a], span{f, to})
			}
		}
	}
	return l
}

func asciiLetters(rs []rune) bool {
	for _, r := range rs {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return len(rs) > 0
}

// spanFields is the Canon fields of a span's text. A sentinel word after it carries the case of the next word's first
// letter, which is all "St" reads there ("St Louis" is saint, "Main St" street), and is dropped.
func spanFields(text string, nextUpper bool) []string {
	sentinel := " q"
	if nextUpper {
		sentinel = " Q"
	}
	f := fields(text + sentinel)
	if len(f) == 0 || f[len(f)-1] != "q" {
		panic("canon: the span sentinel did not stay a word of its own")
	}
	return f[:len(f)-1]
}

// cascade is the state of Canon's token passes (tokenPasses), in their order. Every field is comparable, so a
// cascade is a map key.
type cascade struct {
	mark  bool // marked: a punctuation mark since the last token
	and1  andStage
	art1  articleStage
	zero1 zeroStage
	join  joinStage
	and2  andStage
	art2  articleStage
	zero2 zeroStage
	year  yearStage
}

const stages = 8 // and1 ... year; expandTokens, between zero2 and year, keeps no state

type branch struct {
	c   cascade
	out []string
}

// read feeds a span's fields through the passes: every way they can go, with the tokens each way emits.
func (c cascade) read(fields []string) []branch {
	cur := []branch{{c, nil}}
	for _, f := range fields {
		var next []branch
		for _, b := range cur {
			if f == "\x00" {
				b.c.mark = true
				next = append(next, b)
				continue
			}
			t := tok{f, b.c.mark}
			b.c.mark = false
			for _, d := range b.c.flow(0, []tok{t}) {
				next = append(next, branch{d.c, append(slices.Clone(b.out), d.out...)})
			}
		}
		cur = next
	}
	return cur
}

// flow pushes toks through stage k and the stages after it.
func (c cascade) flow(k int, toks []tok) []branch {
	if k == stages {
		return []branch{{c, texts(toks)}}
	}
	cur := []branch{{c, nil}}
	for _, t := range toks {
		var next []branch
		for _, b := range cur {
			for _, s := range b.c.push(k, t) {
				for _, d := range s.c.flow(k+1, s.toks) {
					next = append(next, branch{d.c, append(slices.Clone(b.out), d.out...)})
				}
			}
		}
		cur = next
	}
	return cur
}

// finish ends the passes from stage k on: what each stage still holds goes through the stages after it, which then end
// too. A way whose guess the end refutes yields nothing.
func (c cascade) finish(k int) [][]string {
	if k == stages {
		return [][]string{nil}
	}
	var out [][]string
	for _, s := range c.end(k) {
		for _, d := range s.c.flow(k+1, s.toks) {
			for _, tail := range d.c.finish(k + 1) {
				out = append(out, append(slices.Clone(d.out), tail...))
			}
		}
	}
	return out
}

type step struct {
	c    cascade
	toks []tok
}

func (c cascade) push(k int, t tok) []step {
	one := func(c cascade, toks []tok) []step { return []step{{c, toks}} }
	switch k {
	case 0:
		var out []tok
		c.and1, out = c.and1.push(t)
		return one(c, out)
	case 1:
		var out []tok
		c.art1, out = c.art1.push(t)
		return one(c, out)
	case 2:
		var steps []step
		for _, z := range c.zero1.push(t) {
			d := c
			d.zero1 = z.s
			steps = append(steps, step{d, z.toks})
		}
		return steps
	case 3:
		var out []tok
		c.join, out = c.join.push(t)
		return one(c, out)
	case 4:
		var out []tok
		c.and2, out = c.and2.push(t)
		return one(c, out)
	case 5:
		var out []tok
		c.art2, out = c.art2.push(t)
		return one(c, out)
	case 6:
		var steps []step
		for _, z := range c.zero2.push(t) {
			d := c
			d.zero2 = z.s
			steps = append(steps, step{d, expandTokens(z.toks)})
		}
		return steps
	default:
		var steps []step
		for _, y := range c.year.push(t) {
			d := c
			d.year = y.s
			steps = append(steps, step{d, y.toks})
		}
		return steps
	}
}

func (c cascade) end(k int) []step {
	switch k {
	case 0:
		return []step{{c, c.and1.end()}}
	case 1:
		return []step{{c, c.art1.end()}}
	case 2:
		toks, ok := c.zero1.end()
		if !ok {
			return nil
		}
		return []step{{c, toks}}
	case 3:
		return []step{{c, c.join.end()}}
	case 4:
		return []step{{c, c.and2.end()}}
	case 5:
		return []step{{c, c.art2.end()}}
	case 6:
		toks, ok := c.zero2.end()
		if !ok {
			return nil
		}
		return []step{{c, expandTokens(toks)}}
	default:
		if !c.year.end() {
			return nil
		}
		return []step{{c, nil}}
	}
}

// andStage is dropNumberAnd: an "and" after a scale word is held until the next token shows whether it is a number
// word, which drops it and takes its punctuation mark.
type andStage struct {
	prev, prevScale bool // a token came before; it is a scale word
	held, heldBrk   bool
	carry           bool // a dropped "and" had a mark before it
}

func (s andStage) push(t tok) (andStage, []tok) {
	var out []tok
	if s.held {
		s.held = false
		if isNumberWord(t.text) {
			s.carry = s.carry || s.heldBrk
		} else {
			out = append(out, tok{"and", s.heldBrk || s.carry})
			s.carry = false
		}
		s.prevScale = false
	}
	if t.text == "and" && s.prev && s.prevScale {
		s.held, s.heldBrk, s.prevScale = true, t.brk, false
		return s, out
	}
	out = append(out, tok{t.text, t.brk || s.carry})
	s.carry = false
	s.prev, s.prevScale = true, isScale(t.text)
	return s, out
}

func (s andStage) end() []tok {
	if s.held {
		return []tok{{"and", s.heldBrk || s.carry}}
	}
	return nil
}

// articleStage is scaleArticle: an "a" is held until the next token shows whether it is a scale word.
type articleStage struct{ held, heldBrk bool }

func (s articleStage) push(t tok) (articleStage, []tok) {
	var out []tok
	if s.held {
		w := "a"
		if isScale(t.text) {
			w = "one"
		}
		out = append(out, tok{w, s.heldBrk})
		s.held = false
	}
	if t.text == "a" {
		s.held, s.heldBrk = true, t.brk
		return s, out
	}
	return s, append(out, t)
}

func (s articleStage) end() []tok {
	if s.held {
		return []tok{{"a", s.heldBrk}}
	}
	return nil
}

// zeroStage is spokenZeros. Whether a run of "oh" and "o" reads as zeros depends on the tokens on both sides of it, so
// the reading is guessed where the run starts and checked where it ends; a wrong guess yields nothing.
type zeroStage struct {
	prevLetter, prevNumber bool // the token before: a single letter; a number word or "point"
	inRun, convert         bool
}

type zeroStep struct {
	s    zeroStage
	toks []tok
}

func singleLetter(w string) bool { return len(w) == 1 && w[0] >= 'a' && w[0] <= 'z' }

func (s zeroStage) push(t tok) []zeroStep {
	if t.text == "oh" || t.text == "o" {
		read := func(s zeroStage) zeroStep {
			if s.convert {
				return zeroStep{s, []tok{{"zero", t.brk}}}
			}
			return zeroStep{s, []tok{t}}
		}
		if s.inRun {
			return []zeroStep{read(s)}
		}
		s.inRun = true
		var out []zeroStep
		for _, convert := range []bool{false, true} {
			s.convert = convert
			out = append(out, read(s))
		}
		return out
	}
	if s.inRun {
		if s.convert != (!s.prevLetter && !singleLetter(t.text) && (s.prevNumber || isNumberWord(t.text))) {
			return nil
		}
		s.inRun = false
	}
	s.prevLetter, s.prevNumber = singleLetter(t.text), isNumberWord(t.text) || t.text == "point"
	return []zeroStep{{s, []tok{t}}}
}

func (s zeroStage) end() ([]tok, bool) {
	return nil, !s.inRun || s.convert == (!s.prevLetter && s.prevNumber)
}

// joinStage is joinLetters: a run of single letters becomes one token when the run ends.
type joinStage struct {
	letters string
	brk     bool
}

func (s joinStage) push(t tok) (joinStage, []tok) {
	if singleLetter(t.text) {
		if s.letters == "" {
			s.brk = t.brk
		}
		s.letters += t.text
		return s, nil
	}
	out := s.end()
	return joinStage{}, append(out, t)
}

func (s joinStage) end() []tok {
	if s.letters == "" {
		return nil
	}
	return []tok{{s.letters, s.brk}}
}

// yearStage reads year pairs (yearPairAt) beside the tokens: at a token that may start a pair, one way passes the
// token on and another reads the pair, emitting its cardinal once the pair's last token is read. A pair whose second
// half is a tens word and a unit ("nineteen twenty one") is read as the pair ending at the tens word followed by the
// unit: the cardinal of 1920 and "one" spell the cardinal of 1921, so no state waits for the unit.
type yearStage struct {
	prev, prevNumber bool // a token came before; it is a number or scale word
	mode             int8 // yearNone, or how much of a pair has been read
	hi               int
}

const (
	yearNone = iota
	yearHi   // the pair's first number, hi
	yearZero // hi, then "zero": a unit must follow
)

type yearStep struct {
	s    yearStage
	toks []tok
}

func unitValue(w string) (int, bool) {
	v, ok := smallValue(w)
	return v, ok && v >= 1 && v <= 9
}

func (s yearStage) push(t tok) []yearStep {
	after := s
	after.prev, after.prevNumber = true, isNumberWord(t.text) || isScale(t.text)
	pair := func(v int) []tok { // the pair ends here: its cardinal replaces it
		var out []tok
		for _, w := range cardinal(uint64(v)) {
			out = append(out, tok{text: w})
		}
		return out
	}
	done := func(v int) yearStep {
		after.mode = yearNone
		return yearStep{after, pair(v)}
	}
	switch s.mode {
	case yearHi:
		switch t.text {
		case "hundred":
			return []yearStep{done(s.hi * 100)}
		case "zero":
			after.mode = yearZero
			return []yearStep{{after, nil}}
		}
		// The second half is 10 to 99: a unit is below 10, and after "twenty" it makes one number, not a pair.
		if v, ok := smallValue(t.text); ok && v >= 10 {
			return []yearStep{done(s.hi*100 + v)}
		}
		return nil
	case yearZero:
		if u, ok := unitValue(t.text); ok {
			return []yearStep{done(s.hi*100 + u)}
		}
		return nil
	}
	steps := []yearStep{{after, []tok{t}}}
	if hi, ok := smallValue(t.text); ok && hi >= 10 && hi <= 20 && (!s.prev || t.brk || !s.prevNumber) {
		start := after
		start.mode, start.hi = yearHi, hi
		steps = append(steps, yearStep{start, nil})
	}
	return steps
}

// end reports whether the tokens read so far are a reading: a pair still being read is not one.
func (s yearStage) end() bool { return s.mode == yearNone }
