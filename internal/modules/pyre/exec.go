package pyre

// The backtracking machine. It follows _sre's sre_lib.h: the same order
// of alternatives, REPEAT/MAX_UNTIL/MIN_UNTIL with their zero-width
// protection (an iteration that ends where it started stops the
// repeat), REPEAT_ONE/MIN_REPEAT_ONE for single-character bodies,
// possessive repeats and atomic groups as committed sub-matches,
// lookarounds that see the whole string before pos, and the top-level
// SUCCESS checks of fullmatch and of the iterators' must-advance rule.
// Choice points live on an explicit stack and every write to a capture
// mark or repeat counter is logged on a trail, so backtracking restores
// them exactly; nothing recurses per character.

type choiceKind uint8

const (
	cAlt choiceKind = iota
	cRepOne
	cMinRepOne
	cMinUntil
)

type choice struct {
	kind  choiceKind
	pc    int
	pos   int
	trail int
	ip    int // instruction of a repeat choice
	count int
}

type trailEntry struct {
	kind uint8 // 0 mark, 1 repeat count, 2 repeat last position
	idx  int
	old  int
}

type repState struct{ count, last int }

type machine struct {
	s           []rune
	end         int
	start       int
	matchAll    bool
	mustAdvance bool
	marks       []int
	reps        []repState
	trail       []trailEntry
	stack       []choice
}

func (m *machine) setMark(i, v int) {
	m.trail = append(m.trail, trailEntry{0, i, m.marks[i]})
	m.marks[i] = v
}

func (m *machine) setRep(id int, count, last int) {
	r := &m.reps[id]
	m.trail = append(m.trail, trailEntry{1, id, r.count}, trailEntry{2, id, r.last})
	r.count, r.last = count, last
}

func (m *machine) setRepCount(id int, count int) {
	r := &m.reps[id]
	m.trail = append(m.trail, trailEntry{1, id, r.count})
	r.count = count
}

func (m *machine) unwind(n int) {
	for len(m.trail) > n {
		e := m.trail[len(m.trail)-1]
		m.trail = m.trail[:len(m.trail)-1]
		switch e.kind {
		case 0:
			m.marks[e.idx] = e.old
		case 1:
			m.reps[e.idx].count = e.old
		case 2:
			m.reps[e.idx].last = e.old
		}
	}
}

func isWordAt(uni bool, c rune) bool {
	if uni {
		return uniIsWord(c)
	}
	return asciiIsWord(c)
}

func (m *machine) at(code atCode, uni bool, pos int) bool {
	switch code {
	case atBeginning, atBeginningString:
		return pos == 0
	case atBeginningLine:
		return pos == 0 || m.s[pos-1] == '\n'
	case atEnd:
		return pos == m.end || pos+1 == m.end && m.s[pos] == '\n'
	case atEndLine:
		return pos == m.end || m.s[pos] == '\n'
	case atEndString:
		return pos == m.end
	case atBoundary, atNonBoundary:
		if m.end == 0 && code == atBoundary {
			return false // 3.14: \B matches the empty string
		}
		that := pos > 0 && isWordAt(uni, m.s[pos-1])
		this := pos < m.end && isWordAt(uni, m.s[pos])
		if code == atBoundary {
			return this != that
		}
		return this == that
	}
	return false
}

func (m *machine) count(u *unit, pos int, limit uint64) int {
	n := 0
	for pos+n < m.end && uint64(n) < limit && u.match(m.s[pos+n]) {
		n++
	}
	return n
}

// run matches p from pos; toplevel applies fullmatch's and must-advance's
// checks at the final SUCCESS. On success the trail keeps the writes of
// the successful path; on failure it is unwound.
func (m *machine) run(p *prog, pos int, toplevel bool) (int, bool) {
	base := len(m.stack)
	trailBase := len(m.trail)
	pc := 0
	for {
		in := &p.insts[pc]
		ok := true
		switch in.op {
		case iUnit:
			if pos < m.end && in.u.match(m.s[pos]) {
				pos++
				pc++
			} else {
				ok = false
			}
		case iAt:
			if m.at(in.at, in.uni, pos) {
				pc++
			} else {
				ok = false
			}
		case iMark:
			m.setMark(in.n, pos)
			pc++
		case iSplit:
			m.stack = append(m.stack, choice{kind: cAlt, pc: in.x, pos: pos, trail: len(m.trail)})
			pc++
		case iJmp:
			pc = in.x
		case iFail:
			ok = false
		case iMatch:
			if toplevel && (m.matchAll && pos != m.end || m.mustAdvance && pos == m.start) {
				ok = false
				break
			}
			m.stack = m.stack[:base]
			return pos, true
		case iGroupref:
			b, e := m.marks[2*(in.n-1)], m.marks[2*(in.n-1)+1]
			if b < 0 || e < 0 || e < b {
				ok = false
				break
			}
			for i := b; i < e; i++ {
				if pos >= m.end {
					ok = false
					break
				}
				x, y := m.s[pos], m.s[i]
				switch in.ignore {
				case 1:
					x, y = asciiLower(x), asciiLower(y)
				case 2:
					x, y = uniLower(x), uniLower(y)
				}
				if x != y {
					ok = false
					break
				}
				pos++
			}
			if ok {
				pc++
			}
		case iGroupExists:
			if m.marks[2*(in.n-1)] >= 0 && m.marks[2*(in.n-1)+1] >= 0 {
				pc++
			} else {
				pc = in.x
			}
		case iRepeat:
			m.setRep(in.n, -1, -1)
			pc = in.x
		case iMaxUntil:
			r := m.reps[in.n]
			cnt := r.count + 1
			if uint64(cnt) < in.min {
				m.setRepCount(in.n, cnt)
				pc = in.x
				break
			}
			if (uint64(cnt) < in.max || in.max == maxRepeat) && pos != r.last {
				m.stack = append(m.stack, choice{kind: cAlt, pc: pc + 1, pos: pos, trail: len(m.trail)})
				m.setRep(in.n, cnt, pos)
				pc = in.x
				break
			}
			pc++
		case iMinUntil:
			r := m.reps[in.n]
			cnt := r.count + 1
			if uint64(cnt) < in.min {
				m.setRepCount(in.n, cnt)
				pc = in.x
				break
			}
			m.stack = append(m.stack, choice{kind: cMinUntil, ip: pc, pos: pos, trail: len(m.trail), count: cnt})
			pc++
		case iRepOne:
			n := m.count(in.u, pos, in.max)
			if uint64(n) < in.min {
				ok = false
				break
			}
			if uint64(n) > in.min {
				m.stack = append(m.stack, choice{kind: cRepOne, ip: pc, pc: pc + 1, pos: pos, trail: len(m.trail), count: n})
			}
			pos += n
			pc++
		case iMinRepOne:
			n := 0
			if in.min > 0 {
				n = m.count(in.u, pos, in.min)
				if uint64(n) < in.min {
					ok = false
					break
				}
			}
			pos += n
			m.stack = append(m.stack, choice{kind: cMinRepOne, ip: pc, pc: pc + 1, pos: pos, trail: len(m.trail), count: n})
			pc++
		case iPossRepOne:
			n := m.count(in.u, pos, in.max)
			if uint64(n) < in.min {
				ok = false
				break
			}
			pos += n
			pc++
		case iPossRepeat:
			cnt := uint64(0)
			for cnt < in.min {
				e, matched := m.run(in.sub, pos, false)
				if !matched {
					ok = false
					break
				}
				pos = e
				cnt++
			}
			if !ok {
				break
			}
			last := -1
			for (cnt < in.max || in.max == maxRepeat) && pos != last {
				last = pos
				e, matched := m.run(in.sub, pos, false)
				if !matched {
					break
				}
				pos = e
				cnt++
			}
			pc++
		case iAtomic:
			e, matched := m.run(in.sub, pos, false)
			if !matched {
				ok = false
				break
			}
			pos = e
			pc++
		case iAssert:
			from := pos
			if in.dir < 0 {
				from = pos - in.width
				if from < 0 {
					ok = false
					break
				}
			}
			if _, matched := m.run(in.sub, from, false); !matched {
				ok = false
				break
			}
			pc++
		case iAssertNot:
			from := pos
			if in.dir < 0 {
				from = pos - in.width
			}
			if from >= 0 {
				mark := len(m.trail)
				if _, matched := m.run(in.sub, from, false); matched {
					m.unwind(mark)
					ok = false
					break
				}
			}
			pc++
		}
		if ok {
			continue
		}
		// backtrack
		for {
			if len(m.stack) == base {
				m.unwind(trailBase)
				return 0, false
			}
			c := m.stack[len(m.stack)-1]
			m.stack = m.stack[:len(m.stack)-1]
			m.unwind(c.trail)
			switch c.kind {
			case cAlt:
				pc, pos = c.pc, c.pos
			case cRepOne:
				in := &p.insts[c.ip]
				n := c.count - 1
				if uint64(n) > in.min {
					c.count = n
					m.stack = append(m.stack, c)
				}
				pc, pos = c.pc, c.pos+n
			case cMinRepOne:
				in := &p.insts[c.ip]
				if c.pos >= m.end || !in.u.match(m.s[c.pos]) {
					continue
				}
				c.pos++
				c.count++
				if in.max != maxRepeat && uint64(c.count) > in.max {
					continue
				}
				m.stack = append(m.stack, c)
				pc, pos = c.pc, c.pos
			case cMinUntil:
				in := &p.insts[c.ip]
				if in.max != maxRepeat && uint64(c.count) >= in.max || c.pos == m.reps[in.n].last {
					continue
				}
				m.setRep(in.n, c.count, c.pos)
				pc, pos = in.x, c.pos
			}
			break
		}
	}
}
