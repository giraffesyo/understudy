// Package pyre is CPython's re module for str patterns: a port of
// re/_parser.py (syntax, errors and their positions), the observable
// rules of re/_compiler.py (scoped flags, case folding) and a
// backtracking matcher with _sre's semantics, plus re.sub's replacement
// templates. It is shared by the target-side modules and the
// control-side template filters. Standard library only: it compiles
// into the agent.
//
// Strings are Go (UTF-8) strings; matching is on code points and every
// index the API returns or takes is a byte offset into the string.
package pyre

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// Flag is a re flag value.
type Flag int

// The re module's flags.
const (
	IGNORECASE Flag = 2
	LOCALE     Flag = 4
	MULTILINE  Flag = 8
	DOTALL     Flag = 16
	UNICODE    Flag = 32
	VERBOSE    Flag = 64
	ASCII      Flag = 256
)

// Error is an exception raised by re: Exc is its Python class
// ("PatternError" — re.error — when empty; "OverflowError",
// "ValueError" or, from a replacement template, "IndexError").
type Error struct {
	Msg     string
	Pos     int // in code points; -1 when the error has none
	Exc     string
	pattern []rune
}

// Error is str() of the exception.
func (e *Error) Error() string {
	if e.pattern == nil || e.Pos < 0 {
		return e.Msg
	}
	msg := fmt.Sprintf("%s at position %d", e.Msg, e.Pos)
	nl := false
	line, col := 1, e.Pos+1
	for i, r := range e.pattern {
		if r != '\n' {
			continue
		}
		nl = true
		if i < e.Pos {
			line++
			col = e.Pos - i
		}
	}
	if nl {
		msg = fmt.Sprintf("%s (line %d, column %d)", msg, line, col)
	}
	return msg
}

// ExcName is the exception's Python class name.
func (e *Error) ExcName() string {
	if e.Exc == "" {
		return "PatternError"
	}
	return e.Exc
}

// Pattern is a compiled regular expression (re.Pattern).
type Pattern struct {
	pattern string
	flags   Flag
	groups  int
	names   []string
	index   map[string]int
	prog    *prog
	reps    int
}

type cacheKey struct {
	pattern string
	flags   Flag
}

var (
	cacheMu sync.Mutex
	cache   = map[cacheKey]*Pattern{}
)

const maxCache = 512

// Compile is re.compile(pattern, flags).
func Compile(pattern string, flags Flag) (*Pattern, error) {
	key := cacheKey{pattern, flags}
	cacheMu.Lock()
	p, ok := cache[key]
	cacheMu.Unlock()
	if ok {
		return p, nil
	}
	p, err := compile(pattern, flags)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	if len(cache) >= maxCache {
		clear(cache)
	}
	cache[key] = p
	cacheMu.Unlock()
	return p, nil
}

// MustCompile is Compile that panics on an error.
func MustCompile(pattern string, flags Flag) *Pattern {
	p, err := Compile(pattern, flags)
	if err != nil {
		panic(err)
	}
	return p
}

func compile(pattern string, flags Flag) (p *Pattern, err error) {
	sp, err := parse([]rune(pattern), flags)
	if err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			p, err = nil, e
		}
	}()
	st := sp.state
	c := &compiler{}
	pr := c.program(sp, st.flags)
	return &Pattern{
		pattern: pattern,
		flags:   st.flags,
		groups:  st.groups() - 1,
		names:   st.groupnames,
		index:   st.groupdict,
		prog:    pr,
		reps:    c.reps,
	}, nil
}

// String is the pattern's source (Pattern.pattern).
func (p *Pattern) String() string { return p.pattern }

// Pattern is the pattern's source.
func (p *Pattern) Pattern() string { return p.pattern }

// Flags is Pattern.flags (UNICODE included unless ASCII, inline flags
// merged).
func (p *Pattern) Flags() Flag { return p.flags }

// Groups is Pattern.groups: the number of capturing groups.
func (p *Pattern) Groups() int { return p.groups }

// NumSubexp is Groups (regexp's spelling).
func (p *Pattern) NumSubexp() int { return p.groups }

// GroupIndex is Pattern.groupindex.
func (p *Pattern) GroupIndex() map[string]int {
	out := make(map[string]int, len(p.index))
	for k, v := range p.index {
		out[k] = v
	}
	return out
}

// SubexpIndex is the number of the group named name, or -1.
func (p *Pattern) SubexpIndex(name string) int {
	if g, ok := p.index[name]; ok {
		return g
	}
	return -1
}

// SubexpNames are the groups' names ("" for unnamed and for group 0).
func (p *Pattern) SubexpNames() []string {
	return append([]string(nil), p.names...)
}

// subject is a string decoded to code points with each one's byte offset.
type subject struct {
	s     string
	runes []rune
	offs  []int // len(runes)+1
}

func newSubject(s string) *subject {
	sub := &subject{s: s}
	sub.runes = make([]rune, 0, len(s))
	sub.offs = make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		sub.runes = append(sub.runes, r)
		sub.offs = append(sub.offs, i)
		i += w
	}
	sub.offs = append(sub.offs, len(s))
	return sub
}

// runeIndex is the index of the code point at (or containing) byte b.
func (sub *subject) runeIndex(b int) int {
	if b <= 0 {
		return 0
	}
	if b >= len(sub.s) {
		return len(sub.runes)
	}
	lo, hi := 0, len(sub.runes)
	for lo < hi {
		mid := (lo + hi) / 2
		if sub.offs[mid] < b {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func (p *Pattern) machine(sub *subject, end int) *machine {
	m := &machine{s: sub.runes, end: end}
	m.marks = make([]int, 2*p.groups)
	for i := range m.marks {
		m.marks[i] = -1
	}
	m.reps = make([]repState, p.reps)
	return m
}

func (m *machine) reset() {
	for i := range m.marks {
		m.marks[i] = -1
	}
	m.trail = m.trail[:0]
	m.stack = m.stack[:0]
}

// try matches at exactly pos (code points).
func (p *Pattern) try(m *machine, pos int) (int, bool) {
	m.reset()
	m.start = pos
	return m.run(p.prog, pos, true)
}

// search is SRE(search) from pos: must-advance applies at pos only.
func (p *Pattern) search(m *machine, pos int, mustAdvance bool) (int, int, bool) {
	for start := pos; start <= m.end; start++ {
		m.mustAdvance = mustAdvance && start == pos
		if e, ok := p.try(m, start); ok {
			return start, e, true
		}
	}
	return 0, 0, false
}

// spans converts the machine's marks to byte offsets.
func (p *Pattern) spans(sub *subject, m *machine, b, e int) []int {
	out := make([]int, 2*(p.groups+1))
	out[0], out[1] = sub.offs[b], sub.offs[e]
	for g := 1; g <= p.groups; g++ {
		s, t := m.marks[2*(g-1)], m.marks[2*(g-1)+1]
		if s < 0 || t < 0 {
			out[2*g], out[2*g+1] = -1, -1
			continue
		}
		out[2*g], out[2*g+1] = sub.offs[s], sub.offs[t]
	}
	return out
}

func (sub *subject) bounds(pos, endpos int) (int, int) {
	n := len(sub.runes)
	if endpos < 0 || endpos > len(sub.s) {
		endpos = len(sub.s)
	}
	if pos < 0 {
		pos = 0
	}
	return min(sub.runeIndex(pos), n), min(sub.runeIndex(endpos), n)
}

func (p *Pattern) exec(s string, pos, endpos int, mode int) []int {
	sub := newSubject(s)
	b, end := sub.bounds(pos, endpos)
	if b > end {
		return nil
	}
	m := p.machine(sub, end)
	switch mode {
	case 0: // search
		st, e, ok := p.search(m, b, false)
		if !ok {
			return nil
		}
		return p.spans(sub, m, st, e)
	default: // match, fullmatch
		m.matchAll = mode == 2
		e, ok := p.try(m, b)
		if !ok {
			return nil
		}
		return p.spans(sub, m, b, e)
	}
}

// Search is Pattern.search(s, pos, endpos) (endpos < 0: the end): the
// spans of the match and its groups (-1 for a group that did not
// participate), or nil.
func (p *Pattern) Search(s string, pos, endpos int) []int { return p.exec(s, pos, endpos, 0) }

// Match is Pattern.match(s, pos, endpos).
func (p *Pattern) Match(s string, pos, endpos int) []int { return p.exec(s, pos, endpos, 1) }

// FullMatch is Pattern.fullmatch(s, pos, endpos).
func (p *Pattern) FullMatch(s string, pos, endpos int) []int { return p.exec(s, pos, endpos, 2) }

// MatchString reports whether the pattern matches anywhere in s (search).
func (p *Pattern) MatchString(s string) bool { return p.Search(s, 0, -1) != nil }

// FindAllSubmatchIndex returns the successive matches as
// re.finditer/findall/sub iterate them (an empty match may follow a
// non-empty one; after an empty match the next one at the same
// position must not be empty). n < 0 (or 0) means all.
func (p *Pattern) FindAllSubmatchIndex(s string, n int) [][]int {
	sub := newSubject(s)
	return p.iterate(sub, n)
}

func (p *Pattern) iterate(sub *subject, n int) [][]int {
	var out [][]int
	m := p.machine(sub, len(sub.runes))
	pos, must := 0, false
	for n <= 0 || len(out) < n {
		if pos > m.end {
			break
		}
		st, e, ok := p.search(m, pos, must)
		if !ok {
			break
		}
		out = append(out, p.spans(sub, m, st, e))
		must = e == st
		pos = e
	}
	return out
}

// FindAll is re.findall: each element is the whole match (no groups),
// the group (one group) or a []string of the groups (unmatched: "").
func (p *Pattern) FindAll(s string) []any {
	var out []any
	for _, m := range p.FindAllSubmatchIndex(s, -1) {
		switch p.groups {
		case 0:
			out = append(out, s[m[0]:m[1]])
		case 1:
			out = append(out, groupStr(s, m, 1))
		default:
			gs := make([]string, p.groups)
			for g := 1; g <= p.groups; g++ {
				gs[g-1] = groupStr(s, m, g)
			}
			out = append(out, gs)
		}
	}
	return out
}

func groupStr(s string, m []int, g int) string {
	if m[2*g] < 0 {
		return ""
	}
	return s[m[2*g]:m[2*g+1]]
}

// Split is re.split(pattern, s, maxsplit): groups that did not
// participate are nil.
func (p *Pattern) Split(s string, maxsplit int) []*string {
	str := func(x string) *string { return &x }
	var out []*string
	last := 0
	for _, m := range p.FindAllSubmatchIndex(s, maxsplit) {
		out = append(out, str(s[last:m[0]]))
		for g := 1; g <= p.groups; g++ {
			if m[2*g] < 0 {
				out = append(out, nil)
			} else {
				out = append(out, str(s[m[2*g]:m[2*g+1]]))
			}
		}
		last = m[1]
	}
	return append(out, str(s[last:]))
}

// Sub is re.subn(pattern, repl, s, count) with a string replacement:
// the template is parsed before matching (a repl without a backslash is
// literal), count <= 0 replaces all.
func (p *Pattern) Sub(repl, s string, count int) (string, int, error) {
	var parts []TemplatePart
	if strings.IndexByte(repl, '\\') < 0 {
		parts = []TemplatePart{{Lit: repl, Group: -1}}
	} else {
		var err error
		if parts, err = ParseTemplate(p, repl); err != nil {
			return "", 0, err
		}
	}
	ms := p.FindAllSubmatchIndex(s, count)
	var b strings.Builder
	last := 0
	for _, m := range ms {
		b.WriteString(s[last:m[0]])
		b.WriteString(ExpandTemplate(parts, s, m))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), len(ms), nil
}

// ReplaceAllString is re.sub(pattern, repl, s) for a valid template.
func (p *Pattern) ReplaceAllString(s, repl string) (string, error) {
	out, _, err := p.Sub(repl, s, 0)
	return out, err
}

var specialChars = map[rune]bool{
	'(': true, ')': true, '[': true, ']': true, '{': true, '}': true, '?': true, '*': true,
	'+': true, '-': true, '|': true, '^': true, '$': true, '\\': true, '.': true, '&': true,
	'~': true, '#': true, ' ': true, '\t': true, '\n': true, '\r': true, '\v': true, '\f': true,
}

// Escape is re.escape.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if specialChars[r] {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
