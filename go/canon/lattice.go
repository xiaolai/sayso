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

// pieceGraph adds to g, from node at, the readings of piece, and returns the node they end at; false when the budget
// runs out.
func (g *graph) pieceGraph(at int, piece string, w *budget) (int, bool) {
	in, ok := spans(piece, w)
	if !ok {
		return 0, false
	}
	end := g.node()
	// A key is a place in the input lattice and the stages' states there: a span boundary (edge -1), or the boundary
	// before field pos of edge edge from node. The walk advances one field at a time, so ways that reach the same
	// place in the same state merge, and the readings of a span of many fields are arcs of the output graph rather
	// than a list of every way through it: a run of guesses that each stay open (years read across punctuation)
	// grows the graph by a constant per field instead of doubling it.
	type key struct {
		node, edge, pos int
		c               cascade
	}
	start := key{0, -1, 0, cascade{}}
	nodes := map[key]int{start: at}
	todo := []key{start}
	// Every arc is recorded, duplicates included: each field leaves a constant number of ways, so duplicate arcs
	// cost a constant factor, while recognising them would mean reading, or copying into a key, every byte of their
	// tokens. An arc shares its tokens' bytes with the text, or with the state that built them, and copies none.
	path := func(a, b int, words []string) bool {
		if !w.spend(1 + len(words)) {
			return false
		}
		g.path(a, b, words)
		return true
	}
	reach := func(from int, to key, words []string) bool {
		n, ok := nodes[to]
		if !ok {
			n = g.node()
			nodes[to] = n
			todo = append(todo, to)
		}
		return path(from, n, words)
	}
	for len(todo) > 0 {
		k := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		from := nodes[k]
		// A state holds at most the letters of a run being joined; the rest is a few flags.
		if !w.spend(1 + len(k.c.join.letters)) {
			return 0, false
		}
		if k.edge < 0 && k.node == in.final {
			ok := true
			k.c.finish(0, nil, func(out *emitted) { ok = ok && path(from, end, out.words()) })
			if !ok {
				return 0, false
			}
			continue
		}
		// At a span boundary every span from it is read from its first field; inside a span, the next field.
		edges, pos := in.next[k.node], k.pos
		if k.edge >= 0 {
			edges = edges[k.edge : k.edge+1]
		}
		for i, e := range edges {
			if k.edge >= 0 {
				i = k.edge
			}
			if !w.spend(1) {
				return 0, false
			}
			if len(e.fields) == 0 {
				if !reach(from, key{e.to, -1, 0, k.c}, nil) {
					return 0, false
				}
				continue
			}
			for _, b := range k.c.read(e.fields[pos : pos+1]) {
				to := key{k.node, i, pos + 1, b.c}
				if pos+1 == len(e.fields) {
					to = key{e.to, -1, 0, b.c}
				}
				if !reach(from, to, b.out) {
					return 0, false
				}
			}
		}
	}
	return end, true
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

// spans is the lattice of piece's spans (see pieceGraph); false when the budget runs out, which a word of many
// hyphens does before any span is read: its spans are counted first.
func spans(piece string, work *budget) (spanLattice, bool) {
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
	for _, hy := range hyphens {
		if n := len(hy) + 1; !work.spend(n * (n + 1) / 2) {
			return spanLattice{}, false
		}
	}
	for w, wd := range words {
		hy := hyphens[w]
		starts := append([]int{wd.start}, make([]int, len(hy))...)
		for k, h := range hy {
			starts[k+1] = h + 1
		}
		// After the word, "St" reads the next word's first letter; before a kept hyphen it reads the hyphen.
		nextUpper := w+1 < len(words) && unicode.IsUpper(rs[words[w+1].start])
		stops := append(slices.Clone(hy), wd.end)
		// The word's parts in lower case, joined, so a span of ASCII letters is a substring of it, read in constant time.
		var joined strings.Builder
		at := make([]int, len(starts)+1)        // at[k]: where part k starts in joined
		asciiUpTo := make([]int, len(starts)+1) // asciiUpTo[k]: how many of the parts before k are ASCII letters
		for k := range starts {
			part := rs[starts[k]:stops[k]]
			at[k] = joined.Len()
			joined.WriteString(strings.ToLower(string(part)))
			asciiUpTo[k+1] = asciiUpTo[k]
			if asciiLetters(part) {
				asciiUpTo[k+1]++
			}
		}
		at[len(starts)] = joined.Len()
		lowered := joined.String()
		for a := range starts {
			for b := a; b <= len(hy); b++ {
				to, upper := first[w+1], nextUpper
				if b < len(hy) {
					to, upper = first[w]+b+1, false
				}
				var f []string
				// Every text pass leaves ASCII letters that are no abbreviation (three letters at most) as they are, in
				// lower case.
				if sub := lowered[at[a]:at[b+1]]; asciiUpTo[b+1]-asciiUpTo[a] == b+1-a && (len(sub) > 3 || !IsAbbreviation(sub)) {
					f = []string{sub}
				} else {
					n := 0
					for k := a; k <= b; k++ {
						n += stops[k] - starts[k]
					}
					if !work.spend(n) { // before the text is built
						return spanLattice{}, false
					}
					text := make([]rune, 0, n)
					for k := a; k <= b; k++ {
						text = append(text, rs[starts[k]:stops[k]]...)
					}
					f = spanFields(string(text), upper)
				}
				l.next[first[w]+a] = append(l.next[first[w]+a], span{f, to})
			}
		}
	}
	return l, true
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

// emitted is the tokens a way has emitted, newest first: ways that split share what they emitted before.
type emitted struct {
	word string
	prev *emitted
	n    int
}

func (e *emitted) add(w string) *emitted {
	n := 1
	if e != nil {
		n = e.n + 1
	}
	return &emitted{w, e, n}
}

func (e *emitted) words() []string {
	if e == nil {
		return nil
	}
	out := make([]string, e.n)
	for ; e != nil; e = e.prev {
		out[e.n-1] = e.word
	}
	return out
}

// read feeds a span's fields through the passes: every way they can go, with the tokens each way emits. The ways
// share what they emitted before they split, so a span of many fields costs its length.
func (c cascade) read(fields []string) []branch {
	type way struct {
		c   cascade
		out *emitted
	}
	cur := []way{{c, nil}}
	for _, f := range fields {
		var next []way
		for _, b := range cur {
			if f == "\x00" {
				b.c.mark = true
				next = append(next, b)
				continue
			}
			t := tok{f, b.c.mark}
			b.c.mark = false
			b.c.flow(0, []tok{t}, b.out, func(d cascade, out *emitted) { next = append(next, way{d, out}) })
		}
		cur = next
	}
	ways := make([]branch, len(cur))
	for i, b := range cur {
		ways[i] = branch{b.c, b.out.words()}
	}
	return ways
}

// flow pushes toks through stage k and the stages after it, and calls emit with each way's state and tokens.
func (c cascade) flow(k int, toks []tok, out *emitted, emit func(cascade, *emitted)) {
	if len(toks) == 0 {
		emit(c, out)
		return
	}
	if k == stages {
		for _, t := range toks {
			out = out.add(t.text)
		}
		emit(c, out)
		return
	}
	st := c.push(k, toks[0])
	for _, s := range st.s[:st.n] {
		s.c.flow(k+1, s.toks, out, func(d cascade, o *emitted) { d.flow(k, toks[1:], o, emit) })
	}
}

// finish ends the passes from stage k on: what each stage still holds goes through the stages after it, which then end
// too, and emit is called with each way's tokens. A way whose guess the end refutes yields nothing.
func (c cascade) finish(k int, out *emitted, emit func(*emitted)) {
	if k == stages {
		emit(out)
		return
	}
	st := c.end(k)
	for _, s := range st.s[:st.n] {
		s.c.flow(k+1, s.toks, out, func(d cascade, o *emitted) { d.finish(k+1, o, emit) })
	}
}

type step struct {
	c    cascade
	toks []tok
}

// steps are the ways one token can go through one stage: at most two, a guess and its alternative.
type steps struct {
	s [2]step
	n int
}

func one(c cascade, toks []tok) steps { return steps{[2]step{{c, toks}}, 1} }

func (c cascade) push(k int, t tok) steps {
	var out []tok
	switch k {
	case 0:
		c.and1, out = c.and1.push(t)
	case 1:
		c.art1, out = c.art1.push(t)
	case 2:
		var st steps
		for _, z := range c.zero1.push(t) {
			d := c
			d.zero1 = z.s
			st.s[st.n] = step{d, z.toks}
			st.n++
		}
		return st
	case 3:
		c.join, out = c.join.push(t)
	case 4:
		c.and2, out = c.and2.push(t)
	case 5:
		c.art2, out = c.art2.push(t)
	case 6:
		var st steps
		for _, z := range c.zero2.push(t) {
			d := c
			d.zero2 = z.s
			st.s[st.n] = step{d, expandTokens(z.toks)}
			st.n++
		}
		return st
	default:
		var st steps
		for _, y := range c.year.push(t) {
			d := c
			d.year = y.s
			st.s[st.n] = step{d, y.toks}
			st.n++
		}
		return st
	}
	return one(c, out)
}

func (c cascade) end(k int) steps {
	switch k {
	case 0:
		return one(c, c.and1.end())
	case 1:
		return one(c, c.art1.end())
	case 2:
		if toks, ok := c.zero1.end(); ok {
			return one(c, toks)
		}
	case 3:
		return one(c, c.join.end())
	case 4:
		return one(c, c.and2.end())
	case 5:
		return one(c, c.art2.end())
	case 6:
		if toks, ok := c.zero2.end(); ok {
			return one(c, expandTokens(toks))
		}
	default:
		if c.year.end() {
			return one(c, nil)
		}
	}
	return steps{}
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
