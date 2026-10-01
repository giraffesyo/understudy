package pyre

import "sort"

// The compiler: the parsed pattern becomes a program for the
// backtracking machine in exec.go, with re/_compiler.py's flag handling
// (scoped flags, IGNORECASE's case folding and _EXTRA_CASES, the charset
// optimizer's observable rules) and its errors.

type instOp uint8

const (
	iUnit instOp = iota
	iAt
	iMark
	iSplit
	iJmp
	iFail
	iMatch
	iGroupref
	iGroupExists
	iRepeat
	iMaxUntil
	iMinUntil
	iRepOne
	iMinRepOne
	iPossRepOne
	iPossRepeat
	iAtomic
	iAssert
	iAssertNot
)

type unitKind uint8

const (
	uLit unitKind = iota
	uNotLit
	uLitIgnore
	uNotLitIgnore
	uAny
	uAnyAll
	uSet
)

type unit struct {
	kind  unitKind
	c     rune
	ascii bool // uLitIgnore / uNotLitIgnore: ASCII lowering
	set   *charset
}

type charset struct {
	negate bool
	// ignore: 0 tests the character itself; 1 its ASCII lowercase; 2 its
	// Unicode lowercase (and, for uniRanges, the uppercase of that).
	ignore    int
	bmp       []rangeTab // literals and ranges (lowered charmap when ignoring case)
	tailLits  []rune
	ranges    []rangeTab
	uniRanges []rangeTab
	cats      []category
	uniCats   bool
}

type inst struct {
	op       instOp
	u        *unit
	at       atCode
	uni      bool // iAt boundary: Unicode words
	n        int  // mark index, group index, repeat id
	x        int  // jump target: split alternative, until/body pc, "no" branch
	min, max uint64
	sub      *prog
	dir      int
	width    int
	ignore   int // iGroupref: 0, 1 ascii, 2 unicode
}

type prog struct {
	insts []inst
}

type compiler struct {
	reps int
}

func combineFlags(flags, add, del Flag) Flag {
	if add&typeFlags != 0 {
		flags &^= typeFlags
	}
	return (flags | add) &^ del
}

func isSimple(p *subPattern) bool {
	if len(p.data) != 1 {
		return false
	}
	it := p.data[0]
	if it.op == opSubpattern {
		return it.group == 0 && isSimple(it.sub)
	}
	return it.op == opLiteral || it.op == opNotLiteral || it.op == opAny || it.op == opIn
}

// simpleUnit compiles a simple repeat body to its unit.
func (c *compiler) simpleUnit(p *subPattern, flags Flag) *unit {
	it := p.data[0]
	if it.op == opSubpattern {
		return c.simpleUnit(it.sub, combineFlags(flags, it.addFlags, it.delFlags))
	}
	return c.unitOf(it, flags)
}

func (c *compiler) unitOf(it item, flags Flag) *unit {
	switch it.op {
	case opLiteral, opNotLiteral:
		not := it.op == opNotLiteral
		if flags&IGNORECASE == 0 {
			if not {
				return &unit{kind: uNotLit, c: it.c}
			}
			return &unit{kind: uLit, c: it.c}
		}
		if flags&UNICODE == 0 {
			if !asciiIsCased(it.c) {
				if not {
					return &unit{kind: uNotLit, c: it.c}
				}
				return &unit{kind: uLit, c: it.c}
			}
			k := uLitIgnore
			if not {
				k = uNotLitIgnore
			}
			return &unit{kind: k, c: asciiLower(it.c), ascii: true}
		}
		if !uniIsCased(it.c) {
			if not {
				return &unit{kind: uNotLit, c: it.c}
			}
			return &unit{kind: uLit, c: it.c}
		}
		lo := uniLower(it.c)
		fixes, ok := extraCases[lo]
		if !ok {
			k := uLitIgnore
			if not {
				k = uNotLitIgnore
			}
			return &unit{kind: k, c: lo}
		}
		cs := &charset{negate: not, ignore: 2}
		rs := []rangeTab{{lo, lo}}
		for _, f := range fixes {
			rs = append(rs, rangeTab{f, f})
		}
		cs.bmp = normRanges(rs)
		return &unit{kind: uSet, set: cs}
	case opAny:
		if flags&DOTALL != 0 {
			return &unit{kind: uAnyAll}
		}
		return &unit{kind: uAny}
	case opIn:
		return &unit{kind: uSet, set: optimizeCharset(it.set, flags)}
	}
	panic("pyre: not a unit")
}

func normRanges(rs []rangeTab) []rangeTab {
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	var out []rangeTab
	for _, r := range rs {
		if n := len(out); n > 0 && r.lo <= out[n-1].hi+1 {
			out[n-1].hi = max(out[n-1].hi, r.hi)
			continue
		}
		out = append(out, r)
	}
	return out
}

// optimizeCharset is _optimize_charset plus the IN/IN_IGNORE/
// IN_UNI_IGNORE choice of _compile.
func optimizeCharset(items []setItem, flags Flag) *charset {
	uni := flags&UNICODE != 0
	cs := &charset{uniCats: uni}
	if flags&IGNORECASE == 0 {
		var rs []rangeTab
		for _, it := range items {
			switch it.op {
			case setNegate:
				cs.negate = true
			case setLiteral:
				rs = append(rs, rangeTab{it.lo, it.lo})
			case setRange:
				rs = append(rs, rangeTab{it.lo, it.hi})
			case setCategory:
				cs.cats = append(cs.cats, it.cat)
			}
		}
		cs.bmp = normRanges(rs)
		return cs
	}
	lower, iscased := asciiLower, asciiIsCased
	if uni {
		lower, iscased = uniLower, uniIsCased
	}
	var charmap []bool
	set := func(c rune) {
		if charmap == nil {
			charmap = make([]bool, 0x10000)
		}
		charmap[c] = true
	}
	hascased := false
	var plain []rangeTab // the items as given, for a set without cased characters
	for _, it := range items {
		switch it.op {
		case setNegate:
			cs.negate = true
		case setLiteral:
			plain = append(plain, rangeTab{it.lo, it.lo})
			av := lower(it.lo)
			if av < 0x10000 {
				set(av)
				if uni {
					for _, k := range extraCases[av] {
						set(k)
					}
				}
			} else {
				cs.tailLits = append(cs.tailLits, av)
			}
			if !hascased && iscased(av) {
				hascased = true
			}
		case setRange:
			plain = append(plain, rangeTab{it.lo, it.hi})
			for i := it.lo; i <= it.hi && i < 0x10000; i++ {
				l := lower(i)
				set(l)
				if uni {
					for _, k := range extraCases[l] {
						set(k)
					}
				}
			}
			if it.hi >= 0x10000 {
				if uni {
					cs.uniRanges = append(cs.uniRanges, rangeTab{it.lo, it.hi})
				} else {
					cs.ranges = append(cs.ranges, rangeTab{it.lo, it.hi})
				}
				hascased = true
			} else if !hascased {
				for i := it.lo; i <= it.hi; i++ {
					if iscased(i) {
						hascased = true
						break
					}
				}
			}
		case setCategory:
			cs.cats = append(cs.cats, it.cat)
		}
	}
	if !hascased {
		cs.bmp = normRanges(plain)
		cs.tailLits, cs.ranges, cs.uniRanges = nil, nil, nil
		return cs
	}
	cs.ignore = 1
	if uni {
		cs.ignore = 2
	}
	var rs []rangeTab
	for i := 0; i < len(charmap); i++ {
		if !charmap[i] {
			continue
		}
		j := i
		for j+1 < len(charmap) && charmap[j+1] {
			j++
		}
		rs = append(rs, rangeTab{rune(i), rune(j)})
		i = j
	}
	cs.bmp = rs
	return cs
}

func (cs *charset) match(ch rune) bool {
	switch cs.ignore {
	case 1:
		ch = asciiLower(ch)
	case 2:
		ch = uniLower(ch)
	}
	return cs.test(ch) != cs.negate
}

func (cs *charset) test(ch rune) bool {
	if inTab(cs.bmp, ch) {
		return true
	}
	for _, l := range cs.tailLits {
		if l == ch {
			return true
		}
	}
	for _, r := range cs.ranges {
		if r.lo <= ch && ch <= r.hi {
			return true
		}
	}
	if len(cs.uniRanges) > 0 {
		up := uniUpper(ch)
		for _, r := range cs.uniRanges {
			if r.lo <= ch && ch <= r.hi || r.lo <= up && up <= r.hi {
				return true
			}
		}
	}
	for _, c := range cs.cats {
		if catMatch(c, cs.uniCats, ch) {
			return true
		}
	}
	return false
}

func catMatch(c category, uni bool, ch rune) bool {
	var v bool
	switch c {
	case catDigit, catNotDigit:
		if uni {
			v = uniIsDigit(ch)
		} else {
			v = asciiIsDigit(ch)
		}
	case catSpace, catNotSpace:
		if uni {
			v = uniIsSpace(ch)
		} else {
			v = asciiIsSpace(ch)
		}
	case catWord, catNotWord:
		if uni {
			v = uniIsWord(ch)
		} else {
			v = asciiIsWord(ch)
		}
	}
	if c == catNotDigit || c == catNotSpace || c == catNotWord {
		return !v
	}
	return v
}

func (u *unit) match(ch rune) bool {
	switch u.kind {
	case uLit:
		return ch == u.c
	case uNotLit:
		return ch != u.c
	case uLitIgnore, uNotLitIgnore:
		var l rune
		if u.ascii {
			l = asciiLower(ch)
		} else {
			l = uniLower(ch)
		}
		return (l == u.c) == (u.kind == uLitIgnore)
	case uAny:
		return ch != '\n'
	case uAnyAll:
		return true
	case uSet:
		return u.set.match(ch)
	}
	return false
}

func (c *compiler) program(p *subPattern, flags Flag) *prog {
	pr := &prog{}
	c.emit(pr, p, flags)
	pr.insts = append(pr.insts, inst{op: iMatch})
	return pr
}

func (c *compiler) emit(pr *prog, p *subPattern, flags Flag) {
	add := func(in inst) int {
		pr.insts = append(pr.insts, in)
		return len(pr.insts) - 1
	}
	for _, it := range p.data {
		switch it.op {
		case opLiteral, opNotLiteral, opAny, opIn:
			add(inst{op: iUnit, u: c.unitOf(it, flags)})
		case opMinRepeat, opMaxRepeat, opPossessiveRepeat:
			if isSimple(it.sub) {
				op := iRepOne
				switch it.op {
				case opMinRepeat:
					op = iMinRepOne
				case opPossessiveRepeat:
					op = iPossRepOne
				}
				add(inst{op: op, u: c.simpleUnit(it.sub, flags), min: it.min, max: it.max})
				continue
			}
			if it.op == opPossessiveRepeat {
				add(inst{op: iPossRepeat, sub: c.program(it.sub, flags), min: it.min, max: it.max})
				continue
			}
			id := c.reps
			c.reps++
			r := add(inst{op: iRepeat, n: id})
			body := len(pr.insts)
			c.emit(pr, it.sub, flags)
			op := iMaxUntil
			if it.op == opMinRepeat {
				op = iMinUntil
			}
			u := add(inst{op: op, n: id, x: body, min: it.min, max: it.max})
			pr.insts[r].x = u
		case opSubpattern:
			if it.group > 0 {
				add(inst{op: iMark, n: (it.group - 1) * 2})
			}
			c.emit(pr, it.sub, combineFlags(flags, it.addFlags, it.delFlags))
			if it.group > 0 {
				add(inst{op: iMark, n: (it.group-1)*2 + 1})
			}
		case opAtomicGroup:
			add(inst{op: iAtomic, sub: c.program(it.sub, flags)})
		case opFailure:
			add(inst{op: iFail})
		case opAssert, opAssertNot:
			op := iAssert
			if it.op == opAssertNot {
				op = iAssertNot
			}
			in := inst{op: op, dir: it.dir}
			if it.dir < 0 {
				w := it.sub.getwidth()
				if w.lo > maxCode {
					panic(&Error{Msg: "looks too much behind", Pos: -1})
				}
				if w.lo != w.hi {
					panic(&Error{Msg: "look-behind requires fixed-width pattern", Pos: -1})
				}
				in.width = int(w.lo)
			}
			in.sub = c.program(it.sub, flags)
			add(in)
		case opAt:
			at := it.at
			if flags&MULTILINE != 0 {
				switch at {
				case atBeginning:
					at = atBeginningLine
				case atEnd:
					at = atEndLine
				}
			}
			add(inst{op: iAt, at: at, uni: flags&UNICODE != 0})
		case opBranch:
			var jumps []int
			for i, alt := range it.alts {
				if i < len(it.alts)-1 {
					s := add(inst{op: iSplit})
					c.emit(pr, alt, flags)
					jumps = append(jumps, add(inst{op: iJmp}))
					pr.insts[s].x = len(pr.insts)
				} else {
					c.emit(pr, alt, flags)
				}
			}
			for _, j := range jumps {
				pr.insts[j].x = len(pr.insts)
			}
		case opGroupref:
			ig := 0
			if flags&IGNORECASE != 0 {
				ig = 1
				if flags&UNICODE != 0 {
					ig = 2
				}
			}
			add(inst{op: iGroupref, n: it.group, ignore: ig})
		case opGroupRefExists:
			g := add(inst{op: iGroupExists, n: it.group})
			c.emit(pr, it.sub, flags)
			if it.no != nil {
				j := add(inst{op: iJmp})
				pr.insts[g].x = len(pr.insts)
				c.emit(pr, it.no, flags)
				pr.insts[j].x = len(pr.insts)
			} else {
				pr.insts[g].x = len(pr.insts)
			}
		}
	}
}
