package pyre

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// A port of CPython's re/_parser.py (3.14): the same grammar, the same
// errors at the same positions.

const (
	maxRepeat = 4294967295 // _sre.MAXREPEAT
	maxGroups = 1073741823 // _sre.MAXGROUPS
	maxCode   = 4294967295 // (1 << 32) - 1
	maxWidth  = math.MaxUint64
)

type opcode int

const (
	opLiteral opcode = iota
	opNotLiteral
	opAny
	opIn
	opAt
	opBranch
	opSubpattern
	opAtomicGroup
	opMinRepeat
	opMaxRepeat
	opPossessiveRepeat
	opGroupref
	opGroupRefExists
	opAssert
	opAssertNot
	opFailure
)

// set items (inside opIn)
type setOp int

const (
	setLiteral setOp = iota
	setRange
	setCategory
	setNegate
)

type category int

const (
	catDigit category = iota
	catNotDigit
	catSpace
	catNotSpace
	catWord
	catNotWord
)

type atCode int

const (
	atBeginning atCode = iota
	atBeginningLine
	atBeginningString
	atBoundary
	atNonBoundary
	atEnd
	atEndLine
	atEndString
)

type setItem struct {
	op     setOp
	lo, hi rune // literal: lo; range: lo-hi
	cat    category
}

type item struct {
	op  opcode
	c   rune      // literal, not-literal
	at  atCode    // opAt
	set []setItem // opIn
	// opBranch
	alts []*subPattern
	// opSubpattern (group 0: none), opAtomicGroup / opAssert body in sub
	group              int
	addFlags, delFlags Flag
	sub                *subPattern
	// repeats: min, max, body in sub
	min, max uint64
	// opGroupRefExists: cond group, yes in sub, no in no
	no *subPattern
	// opAssert / opAssertNot: dir 1 ahead, -1 behind
	dir int
}

type width struct{ lo, hi uint64 }

type subPattern struct {
	state *parseState
	data  []item
	width *width
}

type parseState struct {
	flags            Flag
	groupdict        map[string]int
	groupnames       []string
	groupwidths      []*width // nil: open
	lookbehindgroups int      // -1: None
	grouprefpos      map[int]int
	grouprefOrder    []int
}

func (s *parseState) groups() int { return len(s.groupwidths) }

func (s *parseState) opengroup(name string, named bool) (int, string) {
	gid := s.groups()
	s.groupwidths = append(s.groupwidths, nil)
	s.groupnames = append(s.groupnames, "")
	if s.groups() > maxGroups {
		return gid, "too many groups"
	}
	if named {
		if ogid, ok := s.groupdict[name]; ok {
			return gid, fmt.Sprintf("redefinition of group name %s as group %d; was group %d", pyRepr(name), gid, ogid)
		}
		s.groupdict[name] = gid
		s.groupnames[gid] = name
	}
	return gid, ""
}

func (s *parseState) checkgroup(gid int) bool {
	return gid < s.groups() && s.groupwidths[gid] != nil
}

func (s *parseState) checklookbehindgroup(gid int, src *tokenizer) {
	if s.lookbehindgroups >= 0 {
		if !s.checkgroup(gid) {
			panic(src.error("cannot refer to an open group", 0))
		}
		if gid >= s.lookbehindgroups {
			panic(src.error("cannot refer to group defined in the same lookbehind subpattern", 0))
		}
	}
}

func addW(a, b uint64) uint64 {
	s, c := bits.Add64(a, b, 0)
	if c != 0 {
		return maxWidth
	}
	return s
}

func mulW(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return maxWidth
	}
	return lo
}

func (p *subPattern) getwidth() width {
	if p.width != nil {
		return *p.width
	}
	var lo, hi uint64
	for _, it := range p.data {
		switch it.op {
		case opBranch:
			i, j := uint64(maxWidth), uint64(0)
			for _, a := range it.alts {
				w := a.getwidth()
				i = min(i, w.lo)
				j = max(j, w.hi)
			}
			lo, hi = addW(lo, i), addW(hi, j)
		case opAtomicGroup, opSubpattern:
			w := it.sub.getwidth()
			lo, hi = addW(lo, w.lo), addW(hi, w.hi)
		case opMinRepeat, opMaxRepeat, opPossessiveRepeat:
			w := it.sub.getwidth()
			lo = addW(lo, mulW(w.lo, it.min))
			if it.max == maxRepeat && w.hi != 0 {
				hi = maxWidth
			} else {
				hi = addW(hi, mulW(w.hi, it.max))
			}
		case opAny, opIn, opLiteral, opNotLiteral:
			lo, hi = addW(lo, 1), addW(hi, 1)
		case opGroupref:
			w := p.state.groupwidths[it.group]
			lo, hi = addW(lo, w.lo), addW(hi, w.hi)
		case opGroupRefExists:
			w := it.sub.getwidth()
			i, j := w.lo, w.hi
			if it.no != nil {
				w2 := it.no.getwidth()
				i = min(i, w2.lo)
				j = max(j, w2.hi)
			} else {
				i = 0
			}
			lo, hi = addW(lo, i), addW(hi, j)
		}
	}
	p.width = &width{lo, hi}
	return *p.width
}

// tokenizer is _parser.Tokenizer over code points (a bytes pattern's
// bytes decoded as latin-1, as Tokenizer decodes them).
type tokenizer struct {
	str   []rune
	index int
	next  string
	has   bool // next is not None
	bytes bool // not istext: a bytes pattern
}

func newTokenizer(s []rune, bytes bool) *tokenizer {
	t := &tokenizer{str: s, bytes: bytes}
	t.advance()
	return t
}

func (t *tokenizer) advance() {
	index := t.index
	if index >= len(t.str) {
		t.has = false
		t.next = ""
		return
	}
	ch := string(t.str[index])
	if t.str[index] == '\\' {
		index++
		if index >= len(t.str) {
			panic(&Error{Msg: "bad escape (end of pattern)", pattern: t.str, Pos: len(t.str) - 1})
		}
		ch += string(t.str[index])
	}
	t.index = index + 1
	t.next = ch
	t.has = true
}

func (t *tokenizer) match(c string) bool {
	if t.has && c == t.next {
		t.advance()
		return true
	}
	return false
}

// get returns the next token ("" and false at the end).
func (t *tokenizer) get() (string, bool) {
	this, ok := t.next, t.has
	t.advance()
	return this, ok
}

func (t *tokenizer) nextIn(set string) bool {
	return t.has && len([]rune(t.next)) == 1 && strings.Contains(set, t.next)
}

func (t *tokenizer) getwhile(n int, set string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if !t.nextIn(set) {
			break
		}
		b.WriteString(t.next)
		t.advance()
	}
	return b.String()
}

func (t *tokenizer) getuntil(term, name string) string {
	var result strings.Builder
	n := 0
	for {
		c, ok := t.next, t.has
		t.advance()
		if !ok {
			if n == 0 {
				panic(t.error("missing "+name, 0))
			}
			panic(t.error(fmt.Sprintf("missing %s, unterminated name", term), n))
		}
		if c == term {
			if n == 0 {
				panic(t.error("missing "+name, 1))
			}
			break
		}
		result.WriteString(c)
		n += len([]rune(c))
	}
	return result.String()
}

func (t *tokenizer) tell() int {
	return t.index - len([]rune(t.next))
}

func (t *tokenizer) seek(index int) {
	t.index = index
	t.advance()
}

// error is Tokenizer.error: a bytes pattern's messages are ASCII, other
// characters backslash-escaped.
func (t *tokenizer) error(msg string, offset int) *Error {
	if t.bytes {
		msg = backslashReplace(msg)
	}
	return &Error{Msg: msg, pattern: t.str, Pos: t.tell() - offset}
}

// backslashReplace is s.encode('ascii', 'backslashreplace').
func backslashReplace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

func (t *tokenizer) checkgroupname(name string, offset int) {
	if t.bytes && strings.IndexFunc(name, func(r rune) bool { return r >= 0x80 }) >= 0 {
		// %a: the name's ascii() (backslashreplace of its repr).
		panic(t.error("bad character in group name "+pyRepr(name), len([]rune(name))+offset))
	}
	if !isIdentifier(name) {
		panic(t.error("bad character in group name "+pyRepr(name), len([]rune(name))+offset))
	}
}

const (
	digits     = "0123456789"
	octdigits  = "01234567"
	hexdigits  = "0123456789abcdefABCDEF"
	asciiLets  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	whitespace = " \t\n\r\v\f"
	specials   = ".\\[{()*+?^$|"
	repeats    = "*+?{"
)

var escapes = map[string]rune{
	`\a`: '\a', `\b`: '\b', `\f`: '\f', `\n`: '\n', `\r`: '\r', `\t`: '\t', `\v`: '\v', `\\`: '\\',
}

type catEntry struct {
	isAt bool
	at   atCode
	cat  category
}

var categories = map[string]catEntry{
	`\A`: {isAt: true, at: atBeginningString},
	`\b`: {isAt: true, at: atBoundary},
	`\B`: {isAt: true, at: atNonBoundary},
	`\d`: {cat: catDigit},
	`\D`: {cat: catNotDigit},
	`\s`: {cat: catSpace},
	`\S`: {cat: catNotSpace},
	`\w`: {cat: catWord},
	`\W`: {cat: catNotWord},
	`\z`: {isAt: true, at: atEndString},
	`\Z`: {isAt: true, at: atEndString},
}

var flagChars = map[byte]Flag{
	'i': IGNORECASE, 'L': LOCALE, 'm': MULTILINE, 's': DOTALL, 'x': VERBOSE, 'a': ASCII, 'u': UNICODE,
}

const typeFlags = ASCII | LOCALE | UNICODE

func isSingle(s string) bool { return len([]rune(s)) == 1 }

func ch0(s string) rune { return []rune(s)[0] }

func parseHex(s string) rune {
	v, _ := strconv.ParseUint(s, 16, 32)
	return rune(v)
}

// escapeCode is the result of _escape / _class_escape: a literal, a
// category (an IN with one category), an AT, or a group reference.
type escapeCode struct {
	kind  int // 0 literal, 1 category, 2 at, 3 groupref
	c     rune
	cat   category
	at    atCode
	group int
}

func commonEscape(src *tokenizer, escape string) (escapeCode, bool) {
	c := escape[1]
	if src.bytes && (c == 'u' || c == 'U' || c == 'N') {
		return escapeCode{}, false // a bytes pattern's \u, \U and \N: bad escapes
	}
	switch c {
	case 'x':
		escape += src.getwhile(2, hexdigits)
		if len(escape) != 4 {
			panic(src.error("incomplete escape "+escape, len(escape)))
		}
		return escapeCode{c: parseHex(escape[2:])}, true
	case 'u':
		escape += src.getwhile(4, hexdigits)
		if len(escape) != 6 {
			panic(src.error("incomplete escape "+escape, len(escape)))
		}
		return escapeCode{c: parseHex(escape[2:])}, true
	case 'U':
		escape += src.getwhile(8, hexdigits)
		if len(escape) != 10 {
			panic(src.error("incomplete escape "+escape, len(escape)))
		}
		v, _ := strconv.ParseUint(escape[2:], 16, 64)
		if v > 0x10ffff {
			panic(src.error("bad escape "+escape, len(escape)))
		}
		return escapeCode{c: rune(v)}, true
	case 'N':
		if !src.match("{") {
			panic(src.error("missing {", 0))
		}
		name := src.getuntil("}", "character name")
		r, ok := lookupName(name)
		if !ok {
			panic(src.error("undefined character name "+pyRepr(name), len([]rune(name))+len(`\N{}`)))
		}
		return escapeCode{c: r}, true
	}
	return escapeCode{}, false
}

func classEscape(src *tokenizer, escape string) escapeCode {
	if r, ok := escapes[escape]; ok {
		return escapeCode{c: r}
	}
	if ce, ok := categories[escape]; ok && !ce.isAt {
		return escapeCode{kind: 1, cat: ce.cat}
	}
	if code, ok := commonEscape(src, escape); ok {
		return code
	}
	c := escape[1]
	if strings.IndexByte(octdigits, c) >= 0 {
		escape += src.getwhile(2, octdigits)
		v, _ := strconv.ParseUint(escape[1:], 8, 32)
		if v > 0o377 {
			panic(src.error(fmt.Sprintf("octal escape value %s outside of range 0-0o377", escape), len(escape)))
		}
		return escapeCode{c: rune(v)}
	}
	if strings.IndexByte(digits, c) >= 0 {
		panic(src.error("bad escape "+escape, len([]rune(escape))))
	}
	if isSingle(escape[1:]) {
		if strings.IndexByte(asciiLets, c) >= 0 {
			panic(src.error("bad escape "+escape, len([]rune(escape))))
		}
		return escapeCode{c: ch0(escape[1:])}
	}
	panic(src.error("bad escape "+escape, len([]rune(escape))))
}

func exprEscape(src *tokenizer, escape string, state *parseState) escapeCode {
	if ce, ok := categories[escape]; ok {
		if ce.isAt {
			return escapeCode{kind: 2, at: ce.at}
		}
		return escapeCode{kind: 1, cat: ce.cat}
	}
	if r, ok := escapes[escape]; ok {
		return escapeCode{c: r}
	}
	if code, ok := commonEscape(src, escape); ok {
		return code
	}
	c := escape[1]
	if c == '0' {
		escape += src.getwhile(2, octdigits)
		v, _ := strconv.ParseUint(escape[1:], 8, 32)
		return escapeCode{c: rune(v)}
	}
	if strings.IndexByte(digits, c) >= 0 {
		if src.nextIn(digits) {
			d, _ := src.get()
			escape += d
			if strings.IndexByte(octdigits, escape[1]) >= 0 && strings.IndexByte(octdigits, escape[2]) >= 0 && src.nextIn(octdigits) {
				d, _ := src.get()
				escape += d
				v, _ := strconv.ParseUint(escape[1:], 8, 32)
				if v > 0o377 {
					panic(src.error(fmt.Sprintf("octal escape value %s outside of range 0-0o377", escape), len(escape)))
				}
				return escapeCode{c: rune(v)}
			}
		}
		group, _ := strconv.Atoi(escape[1:])
		if group < state.groups() {
			if !state.checkgroup(group) {
				panic(src.error("cannot refer to an open group", len(escape)))
			}
			state.checklookbehindgroup(group, src)
			return escapeCode{kind: 3, group: group}
		}
		panic(src.error(fmt.Sprintf("invalid group reference %d", group), len(escape)-1))
	}
	if isSingle(escape[1:]) {
		if strings.IndexByte(asciiLets, c) >= 0 {
			panic(src.error("bad escape "+escape, len([]rune(escape))))
		}
		return escapeCode{c: ch0(escape[1:])}
	}
	panic(src.error("bad escape "+escape, len([]rune(escape))))
}

func itemEqual(a, b item) bool {
	if a.op != b.op {
		return false
	}
	switch a.op {
	case opLiteral, opNotLiteral:
		return a.c == b.c
	case opAny:
		return true
	case opAt:
		return a.at == b.at
	case opGroupref:
		return a.group == b.group
	case opFailure:
		return true
	case opIn:
		if len(a.set) != len(b.set) {
			return false
		}
		for i := range a.set {
			if a.set[i] != b.set[i] {
				return false
			}
		}
		return true
	}
	// Items holding SubPatterns compare by identity in Python: the
	// tuples are rebuilt per alternative, so never equal.
	return false
}

func uniqSet(set []setItem) []setItem {
	var out []setItem
	for _, s := range set {
		dup := false
		for _, o := range out {
			if o == s {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

func parseSub(src *tokenizer, state *parseState, verbose bool, nested int) *subPattern {
	var items []*subPattern
	for {
		items = append(items, parseSeq(src, state, verbose, nested+1, nested == 0 && len(items) == 0))
		if !src.match("|") {
			break
		}
		if nested == 0 {
			verbose = state.flags&VERBOSE != 0
		}
	}
	if len(items) == 1 {
		return items[0]
	}
	sp := &subPattern{state: state}
	// common prefix
	for {
		var prefix *item
		all := true
		for _, it := range items {
			if len(it.data) == 0 {
				all = false
				break
			}
			if prefix == nil {
				p := it.data[0]
				prefix = &p
			} else if !itemEqual(it.data[0], *prefix) {
				all = false
				break
			}
		}
		if !all {
			break
		}
		for _, it := range items {
			it.data = it.data[1:]
		}
		sp.data = append(sp.data, *prefix)
	}
	// a branch of single literals/sets becomes a set
	var set []setItem
	ok := true
	for _, it := range items {
		if len(it.data) != 1 {
			ok = false
			break
		}
		x := it.data[0]
		if x.op == opLiteral {
			set = append(set, setItem{op: setLiteral, lo: x.c})
		} else if x.op == opIn && x.set[0].op != setNegate {
			set = append(set, x.set...)
		} else {
			ok = false
			break
		}
	}
	if ok {
		sp.data = append(sp.data, item{op: opIn, set: uniqSet(set)})
		return sp
	}
	sp.data = append(sp.data, item{op: opBranch, alts: items})
	return sp
}

func parseSeq(src *tokenizer, state *parseState, verbose bool, nested int, first bool) *subPattern {
	sp := &subPattern{state: state}
	for {
		if !src.has {
			break
		}
		this := src.next
		if this == "|" || this == ")" {
			break
		}
		src.advance()
		if verbose {
			if isSingle(this) && strings.Contains(whitespace, this) {
				continue
			}
			if this == "#" {
				for {
					t, ok := src.get()
					if !ok || t == "\n" {
						break
					}
				}
				continue
			}
		}
		switch {
		case this[0] == '\\':
			code := exprEscape(src, this, state)
			switch code.kind {
			case 0:
				sp.data = append(sp.data, item{op: opLiteral, c: code.c})
			case 1:
				sp.data = append(sp.data, item{op: opIn, set: []setItem{{op: setCategory, cat: code.cat}}})
			case 2:
				sp.data = append(sp.data, item{op: opAt, at: code.at})
			case 3:
				sp.data = append(sp.data, item{op: opGroupref, group: code.group})
			}
		case !strings.Contains(specials, this):
			sp.data = append(sp.data, item{op: opLiteral, c: ch0(this)})
		case this == "[":
			here := src.tell() - 1
			var set []setItem
			negate := src.match("^")
			for {
				this, ok := src.get()
				if !ok {
					panic(src.error("unterminated character set", src.tell()-here))
				}
				if this == "]" && len(set) > 0 {
					break
				}
				var code1 escapeCode
				if this[0] == '\\' {
					code1 = classEscape(src, this)
				} else {
					code1 = escapeCode{c: ch0(this)}
				}
				if src.match("-") {
					that, ok := src.get()
					if !ok {
						panic(src.error("unterminated character set", src.tell()-here))
					}
					if that == "]" {
						set = append(set, codeToSet(code1), setItem{op: setLiteral, lo: '-'})
						break
					}
					var code2 escapeCode
					if that[0] == '\\' {
						code2 = classEscape(src, that)
					} else {
						code2 = escapeCode{c: ch0(that)}
					}
					n := len([]rune(this)) + 1 + len([]rune(that))
					if code1.kind != 0 || code2.kind != 0 {
						panic(src.error(fmt.Sprintf("bad character range %s-%s", this, that), n))
					}
					if code2.c < code1.c {
						panic(src.error(fmt.Sprintf("bad character range %s-%s", this, that), n))
					}
					set = append(set, setItem{op: setRange, lo: code1.c, hi: code2.c})
				} else {
					set = append(set, codeToSet(code1))
				}
			}
			set = uniqSet(set)
			if len(set) == 1 && set[0].op == setLiteral {
				if negate {
					sp.data = append(sp.data, item{op: opNotLiteral, c: set[0].lo})
				} else {
					sp.data = append(sp.data, item{op: opLiteral, c: set[0].lo})
				}
			} else {
				if negate {
					set = append([]setItem{{op: setNegate}}, set...)
				}
				sp.data = append(sp.data, item{op: opIn, set: set})
			}
		case strings.Contains(repeats, this):
			here := src.tell()
			var lo, hi uint64
			switch this {
			case "?":
				lo, hi = 0, 1
			case "*":
				lo, hi = 0, maxRepeat
			case "+":
				lo, hi = 1, maxRepeat
			case "{":
				if src.has && src.next == "}" {
					sp.data = append(sp.data, item{op: opLiteral, c: '{'})
					continue
				}
				lo, hi = 0, maxRepeat
				los, his := "", ""
				for src.nextIn(digits) {
					d, _ := src.get()
					los += d
				}
				if src.match(",") {
					for src.nextIn(digits) {
						d, _ := src.get()
						his += d
					}
				} else {
					his = los
				}
				if !src.match("}") {
					sp.data = append(sp.data, item{op: opLiteral, c: '{'})
					src.seek(here)
					continue
				}
				if los != "" {
					v, err := strconv.ParseUint(los, 10, 64)
					if err != nil || v >= maxRepeat {
						panic(&Error{Msg: "the repetition number is too large", Pos: -1, Exc: "OverflowError"})
					}
					lo = v
				}
				if his != "" {
					v, err := strconv.ParseUint(his, 10, 64)
					if err != nil || v >= maxRepeat {
						panic(&Error{Msg: "the repetition number is too large", Pos: -1, Exc: "OverflowError"})
					}
					hi = v
					if hi < lo {
						panic(src.error("min repeat greater than max repeat", src.tell()-here))
					}
				}
			}
			n := len(sp.data)
			if n == 0 || sp.data[n-1].op == opAt {
				panic(src.error("nothing to repeat", src.tell()-here+len(this)))
			}
			last := sp.data[n-1]
			if last.op == opMinRepeat || last.op == opMaxRepeat || last.op == opPossessiveRepeat {
				panic(src.error("multiple repeat", src.tell()-here+len(this)))
			}
			body := &subPattern{state: state, data: []item{last}}
			if last.op == opSubpattern && last.group == 0 && last.addFlags == 0 && last.delFlags == 0 {
				body = last.sub
			}
			op := opMaxRepeat
			if src.match("?") {
				op = opMinRepeat
			} else if src.match("+") {
				op = opPossessiveRepeat
			}
			sp.data[n-1] = item{op: op, min: lo, max: hi, sub: body}
		case this == ".":
			sp.data = append(sp.data, item{op: opAny})
		case this == "(":
			start := src.tell() - 1
			capture := true
			atomic := false
			name, named := "", false
			var addFlags, delFlags Flag
			if src.match("?") {
				char, ok := src.get()
				if !ok {
					panic(src.error("unexpected end of pattern", 0))
				}
				switch {
				case char == "P":
					if src.match("<") {
						name = src.getuntil(">", "group name")
						named = true
						src.checkgroupname(name, 1)
					} else if src.match("=") {
						name = src.getuntil(")", "group name")
						src.checkgroupname(name, 1)
						gid, ok := state.groupdict[name]
						if !ok {
							panic(src.error("unknown group name "+pyRepr(name), len([]rune(name))+1))
						}
						if !state.checkgroup(gid) {
							panic(src.error("cannot refer to an open group", len([]rune(name))+1))
						}
						state.checklookbehindgroup(gid, src)
						sp.data = append(sp.data, item{op: opGroupref, group: gid})
						continue
					} else {
						char, ok := src.get()
						if !ok {
							panic(src.error("unexpected end of pattern", 0))
						}
						panic(src.error("unknown extension ?P"+char, len([]rune(char))+2))
					}
				case char == ":":
					capture = false
				case char == "#":
					for {
						if !src.has {
							panic(src.error("missing ), unterminated comment", src.tell()-start))
						}
						if t, _ := src.get(); t == ")" {
							break
						}
					}
					continue
				case char == "=" || char == "!" || char == "<":
					dir := 1
					saved := -2
					if char == "<" {
						c2, ok := src.get()
						if !ok {
							panic(src.error("unexpected end of pattern", 0))
						}
						if c2 != "=" && c2 != "!" {
							panic(src.error("unknown extension ?<"+c2, len([]rune(c2))+2))
						}
						char = c2
						dir = -1
						saved = state.lookbehindgroups
						if saved < 0 {
							state.lookbehindgroups = state.groups()
						}
					}
					p := parseSub(src, state, verbose, nested+1)
					if dir < 0 && saved < 0 {
						state.lookbehindgroups = -1
					}
					if !src.match(")") {
						panic(src.error("missing ), unterminated subpattern", src.tell()-start))
					}
					if char == "=" {
						sp.data = append(sp.data, item{op: opAssert, dir: dir, sub: p})
					} else if len(p.data) > 0 {
						sp.data = append(sp.data, item{op: opAssertNot, dir: dir, sub: p})
					} else {
						sp.data = append(sp.data, item{op: opFailure})
					}
					continue
				case char == "(":
					condname := src.getuntil(")", "group name")
					var condgroup int
					if !isASCIIDecimal(condname) {
						src.checkgroupname(condname, 1)
						g, ok := state.groupdict[condname]
						if !ok {
							panic(src.error("unknown group name "+pyRepr(condname), len([]rune(condname))+1))
						}
						condgroup = g
					} else {
						v, err := strconv.ParseUint(condname, 10, 64)
						if err == nil && v == 0 {
							panic(src.error("bad group number", len(condname)+1))
						}
						if err != nil || v >= maxGroups {
							panic(src.error("invalid group reference "+trimZeros(condname), len(condname)+1))
						}
						condgroup = int(v)
						if _, ok := state.grouprefpos[condgroup]; !ok {
							state.grouprefpos[condgroup] = src.tell() - len(condname) - 1
							state.grouprefOrder = append(state.grouprefOrder, condgroup)
						}
					}
					state.checklookbehindgroup(condgroup, src)
					yes := parseSeq(src, state, verbose, nested+1, false)
					var no *subPattern
					if src.match("|") {
						no = parseSeq(src, state, verbose, nested+1, false)
						if src.has && src.next == "|" {
							panic(src.error("conditional backref with more than two branches", 0))
						}
					}
					if !src.match(")") {
						panic(src.error("missing ), unterminated subpattern", src.tell()-start))
					}
					sp.data = append(sp.data, item{op: opGroupRefExists, group: condgroup, sub: yes, no: no})
					continue
				case char == ">":
					capture = false
					atomic = true
				case isSingle(char) && (char == "-" || flagChars[char[0]] != 0):
					add, del, global := parseFlags(src, state, char)
					if global {
						if !first || len(sp.data) > 0 {
							panic(src.error("global flags not at the start of the expression", src.tell()-start))
						}
						verbose = state.flags&VERBOSE != 0
						continue
					}
					addFlags, delFlags = add, del
					capture = false
				default:
					panic(src.error("unknown extension ?"+char, len([]rune(char))+1))
				}
			}
			group := 0
			if capture {
				g, msg := state.opengroup(name, named)
				if msg != "" {
					panic(src.error(msg, len([]rune(name))+1))
				}
				group = g
			}
			subVerbose := (verbose || addFlags&VERBOSE != 0) && delFlags&VERBOSE == 0
			p := parseSub(src, state, subVerbose, nested+1)
			if !src.match(")") {
				panic(src.error("missing ), unterminated subpattern", src.tell()-start))
			}
			if group != 0 {
				w := p.getwidth()
				state.groupwidths[group] = &w
			}
			if atomic {
				sp.data = append(sp.data, item{op: opAtomicGroup, sub: p})
			} else {
				sp.data = append(sp.data, item{op: opSubpattern, group: group, addFlags: addFlags, delFlags: delFlags, sub: p})
			}
		case this == "^":
			sp.data = append(sp.data, item{op: opAt, at: atBeginning})
		case this == "$":
			sp.data = append(sp.data, item{op: opAt, at: atEnd})
		}
	}
	// unpack non-capturing groups
	for i := len(sp.data) - 1; i >= 0; i-- {
		it := sp.data[i]
		if it.op == opSubpattern && it.group == 0 && it.addFlags == 0 && it.delFlags == 0 {
			rest := append([]item{}, sp.data[i+1:]...)
			sp.data = append(append(sp.data[:i], it.sub.data...), rest...)
		}
	}
	return sp
}

func codeToSet(c escapeCode) setItem {
	if c.kind == 1 {
		return setItem{op: setCategory, cat: c.cat}
	}
	return setItem{op: setLiteral, lo: c.c}
}

// isAlphaStr is str.isalpha() of a token.
func isAlphaStr(s string) bool {
	for _, r := range s {
		if !inTab(tabAlpha, r) {
			return false
		}
	}
	return s != ""
}

func parseFlags(src *tokenizer, state *parseState, char string) (add, del Flag, global bool) {
	notFlag := func(c string) bool { return len(c) != 1 || flagChars[c[0]] == 0 }
	if char != "-" {
		for {
			flag := flagChars[char[0]]
			if char == "L" && !src.bytes {
				panic(src.error("bad inline flags: cannot use 'L' flag with a str pattern", 0))
			}
			if char == "u" && src.bytes {
				panic(src.error("bad inline flags: cannot use 'u' flag with a bytes pattern", 0))
			}
			add |= flag
			if flag&typeFlags != 0 && add&typeFlags != flag {
				panic(src.error("bad inline flags: flags 'a', 'u' and 'L' are incompatible", 0))
			}
			c, ok := src.get()
			if !ok {
				panic(src.error("missing -, : or )", 0))
			}
			char = c
			if char == ")" || char == "-" || char == ":" {
				break
			}
			if notFlag(char) {
				msg := "missing -, : or )"
				if isAlphaStr(char) {
					msg = "unknown flag"
				}
				panic(src.error(msg, len([]rune(char))))
			}
		}
	}
	if char == ")" {
		state.flags |= add
		return 0, 0, true
	}
	if char == "-" {
		c, ok := src.get()
		if !ok {
			panic(src.error("missing flag", 0))
		}
		char = c
		if notFlag(char) {
			msg := "missing flag"
			if isAlphaStr(char) {
				msg = "unknown flag"
			}
			panic(src.error(msg, len([]rune(char))))
		}
		for {
			flag := flagChars[char[0]]
			if flag&typeFlags != 0 {
				panic(src.error("bad inline flags: cannot turn off flags 'a', 'u' and 'L'", 0))
			}
			del |= flag
			c, ok := src.get()
			if !ok {
				panic(src.error("missing :", 0))
			}
			char = c
			if char == ":" {
				break
			}
			if notFlag(char) {
				msg := "missing :"
				if isAlphaStr(char) {
					msg = "unknown flag"
				}
				panic(src.error(msg, len([]rune(char))))
			}
		}
	}
	if add&del != 0 {
		panic(src.error("bad inline flags: flag turned on and off", 1))
	}
	return add, del, false
}

// parse is _parser.parse for a str pattern.
func parse(pattern []rune, flags Flag, bytes bool) (p *subPattern, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			p, err = nil, e
		}
	}()
	src := newTokenizer(pattern, bytes)
	state := &parseState{
		flags:            flags,
		groupdict:        map[string]int{},
		groupwidths:      []*width{nil},
		groupnames:       []string{""},
		lookbehindgroups: -1,
		grouprefpos:      map[int]int{},
	}
	p = parseSub(src, state, flags&VERBOSE != 0, 0)
	// fix_flags
	f := state.flags
	switch {
	case bytes && f&UNICODE != 0:
		return nil, &Error{Msg: "cannot use UNICODE flag with a bytes pattern", Pos: -1, Exc: "ValueError"}
	case bytes && f&LOCALE != 0 && f&ASCII != 0:
		return nil, &Error{Msg: "ASCII and LOCALE flags are incompatible", Pos: -1, Exc: "ValueError"}
	case bytes:
	case f&LOCALE != 0:
		return nil, &Error{Msg: "cannot use LOCALE flag with a str pattern", Pos: -1, Exc: "ValueError"}
	case f&ASCII == 0:
		f |= UNICODE
	case f&UNICODE != 0:
		return nil, &Error{Msg: "ASCII and UNICODE flags are incompatible", Pos: -1, Exc: "ValueError"}
	}
	state.flags = f
	if src.has {
		panic(src.error("unbalanced parenthesis", 0))
	}
	for _, g := range state.grouprefOrder {
		if g >= state.groups() {
			return nil, &Error{Msg: fmt.Sprintf("invalid group reference %d", g), pattern: pattern, Pos: state.grouprefpos[g]}
		}
	}
	return p, nil
}
