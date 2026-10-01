package template

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// This file ports Jinja2's lexer and parser, and the checks its code
// generator makes while compiling, so that a template Jinja rejects is
// rejected here with Jinja's own TemplateSyntaxError: the same message,
// raised at the same point (the lexer runs lazily, ahead of the parser by
// one token, so a parse error early in a template wins over a lexing
// error later in it), with the same line number. understudy's own parser
// builds the tree it evaluates; this one only decides whether, and how, a
// template fails to compile.

// jinjaSyntaxError is a TemplateSyntaxError (or TemplateAssertionError).
type jinjaSyntaxError struct {
	msg  string
	line int
	// cause is str() of the exception the error was raised from (a
	// string literal's UnicodeDecodeError), "" for none.
	cause string
}

func jfail(msg string, line int) { panic(&jinjaSyntaxError{msg: msg, line: line}) }

type jtok struct {
	line int
	typ  string
	val  string
}

// Token types (jinja2.lexer TOKEN_*).
const (
	jtData          = "data"
	jtName          = "name"
	jtString        = "string"
	jtInteger       = "integer"
	jtFloat         = "float"
	jtEOF           = "eof"
	jtInitial       = "initial"
	jtBlockBegin    = "block_begin"
	jtBlockEnd      = "block_end"
	jtVariableBegin = "variable_begin"
	jtVariableEnd   = "variable_end"
	jtCommentBegin  = "comment_begin"
	jtCommentEnd    = "comment_end"
	jtComment       = "comment"
	jtRawBegin      = "raw_begin"
	jtRawEnd        = "raw_end"
	jtWhitespace    = "whitespace"
	jtOperator      = "operator"
)

// jOperators is jinja2.lexer.operators.
var jOperators = map[string]string{
	"+": "add", "-": "sub", "/": "div", "//": "floordiv", "*": "mul", "%": "mod",
	"**": "pow", "~": "tilde", "[": "lbracket", "]": "rbracket", "(": "lparen",
	")": "rparen", "{": "lbrace", "}": "rbrace", "==": "eq", "!=": "ne", ">": "gt",
	">=": "gteq", "<": "lt", "<=": "lteq", "=": "assign", ".": "dot", ":": "colon",
	"|": "pipe", ",": "comma", ";": "semicolon",
}

var jReverseOperators = func() map[string]string {
	m := make(map[string]string, len(jOperators))
	for k, v := range jOperators {
		m[v] = k
	}
	return m
}()

var jTokenDescriptions = map[string]string{
	jtCommentBegin: "begin of comment", jtCommentEnd: "end of comment", jtComment: "comment",
	"linecomment": "comment", jtBlockBegin: "begin of statement block",
	jtBlockEnd: "end of statement block", jtVariableBegin: "begin of print statement",
	jtVariableEnd: "end of print statement", "linestatement_begin": "begin of line statement",
	"linestatement_end": "end of line statement", jtData: "template data / text",
	jtEOF: "end of template",
}

func jDescribeType(typ string) string {
	if op, ok := jReverseOperators[typ]; ok {
		return op
	}
	if d, ok := jTokenDescriptions[typ]; ok {
		return d
	}
	return typ
}

func jDescribeToken(t jtok) string {
	if t.typ == jtName {
		return t.val
	}
	return jDescribeType(t.typ)
}

func jDescribeExpr(expr string) string {
	if typ, val, ok := strings.Cut(expr, ":"); ok {
		if typ == jtName {
			return val
		}
		return jDescribeType(typ)
	}
	return jDescribeType(expr)
}

func (t jtok) test(expr string) bool {
	if t.typ == expr {
		return true
	}
	if typ, val, ok := strings.Cut(expr, ":"); ok {
		return t.typ == typ && t.val == val
	}
	return false
}

func (t jtok) testAny(exprs ...string) bool {
	for _, e := range exprs {
		if t.test(e) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Lexer

// isPySpace is Python's str.isspace (what \s matches in a str pattern).
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

func isPyDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

// isPyWordRune is \w, with the extra identifier characters of Jinja's
// name pattern (combining marks and connectors).
func isPyWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) ||
		unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc) || r == '·'
}

// isPyIdentifier is str.isidentifier.
func isPyIdentifier(s []rune) bool {
	for i, r := range s {
		start := r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r)
		if i == 0 && !start {
			return false
		}
		if i > 0 && !start && !unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc) && r != '·' {
			return false
		}
	}
	return len(s) > 0
}

type jlexer struct {
	src  []rune
	line int

	bs, be, vs, ve, cs, ce []rune
	trim, lstrip           bool
	escapeBackslashes      bool

	toks []jtok
}

type jrootRule struct {
	delim []rune
	typ   string
}

func hasRunesAt(s []rune, i int, p []rune) bool {
	if i < 0 || i+len(p) > len(s) {
		return false
	}
	for j, r := range p {
		if s[i+j] != r {
			return false
		}
	}
	return true
}

func skipSpace(s []rune, i int) int {
	for i < len(s) && isPySpace(s[i]) {
		i++
	}
	return i
}

func countNL(s []rune) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

// matchEnd matches a block (or comment) end at i, as the rule
// (?:\+END|\-END\s*|END\n?) does (the \n only with trim_blocks).
func (l *jlexer) matchEnd(i int, end []rune) int {
	s := l.src
	if i < len(s) && s[i] == '+' && hasRunesAt(s, i+1, end) {
		return i + 1 + len(end)
	}
	if i < len(s) && s[i] == '-' && hasRunesAt(s, i+1, end) {
		return skipSpace(s, i+1+len(end))
	}
	if hasRunesAt(s, i, end) {
		j := i + len(end)
		if l.trim && j < len(s) && s[j] == '\n' {
			j++
		}
		return j
	}
	return -1
}

// matchRawBegin matches BS(\-|\+|)\s*raw\s*(?:\-BE\s*|BE) at i.
func (l *jlexer) matchRawBegin(i int) (end int, sign string) {
	s := l.src
	if !hasRunesAt(s, i, l.bs) {
		return -1, ""
	}
	j := i + len(l.bs)
	if j < len(s) && (s[j] == '-' || s[j] == '+') {
		sign = string(s[j])
		j++
	}
	j = skipSpace(s, j)
	if !hasRunesAt(s, j, []rune("raw")) {
		return -1, ""
	}
	j = skipSpace(s, j+3)
	if j < len(s) && s[j] == '-' && hasRunesAt(s, j+1, l.be) {
		return skipSpace(s, j+1+len(l.be)), sign
	}
	if hasRunesAt(s, j, l.be) {
		return j + len(l.be), sign
	}
	return -1, ""
}

// matchEndRaw matches (?:BS(\-|\+|))\s*endraw\s*(?:\+BE|\-BE\s*|BE\n?) at i.
func (l *jlexer) matchEndRaw(i int) (end int, sign string) {
	s := l.src
	if !hasRunesAt(s, i, l.bs) {
		return -1, ""
	}
	j := i + len(l.bs)
	if j < len(s) && (s[j] == '-' || s[j] == '+') {
		sign = string(s[j])
		j++
	}
	j = skipSpace(s, j)
	if !hasRunesAt(s, j, []rune("endraw")) {
		return -1, ""
	}
	j = skipSpace(s, j+6)
	if e := l.matchEnd(j, l.be); e >= 0 {
		return e, sign
	}
	return -1, ""
}

func (l *jlexer) emit(line int, typ, val string) {
	l.toks = append(l.toks, jtok{line, typ, val})
}

// lstripText applies an OptionalLStrip rule's whitespace control to the
// text before a tag.
func (l *jlexer) lstripText(text []rune, sign string, isVariable, lineStarting bool) ([]rune, int) {
	if sign == "-" {
		k := len(text)
		for k > 0 && isPySpace(text[k-1]) {
			k--
		}
		return text[:k], countNL(text[k:])
	}
	if sign != "+" && l.lstrip && !isVariable {
		lpos := 0
		for k := len(text) - 1; k >= 0; k-- {
			if text[k] == '\n' {
				lpos = k + 1
				break
			}
		}
		if lpos > 0 || lineStarting {
			rest := text[lpos:]
			all := len(rest) > 0
			for _, r := range rest {
				if !isPySpace(r) {
					all = false
					break
				}
			}
			if all {
				return text[:lpos], 0
			}
		}
	}
	return text, 0
}

// tokeniter is Lexer.tokeniter: every raw token, then the error that
// stopped the lexer (nil at the end of the source).
func (l *jlexer) tokeniter(state string) (err *jinjaSyntaxError) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*jinjaSyntaxError)
			if !ok {
				panic(r)
			}
			err = e
		}
	}()
	s := l.src
	pos := 0
	l.line = 1
	stack := []string{"root"}
	if state == "variable" || state == "block" {
		stack = append(stack, state+"_begin")
	}
	var balancing []rune
	newlinesStripped := 0
	lineStarting := true

	rules := []jrootRule{{l.cs, jtCommentBegin}, {l.bs, jtBlockBegin}, {l.vs, jtVariableBegin}}
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].delim) != len(rules[j].delim) {
			return len(rules[i].delim) > len(rules[j].delim)
		}
		return rules[i].typ > rules[j].typ
	})

	for {
		switch top := stack[len(stack)-1]; top {
		case "root":
			found := false
			for i := pos; i < len(s) && !found; i++ {
				key, sign, end := "", "", -1
				if e, sg := l.matchRawBegin(i); e >= 0 {
					key, sign, end = jtRawBegin, sg, e
				} else {
					for _, r := range rules {
						if hasRunesAt(s, i, r.delim) {
							end = i + len(r.delim)
							if end < len(s) && (s[end] == '-' || s[end] == '+') {
								sign = string(s[end])
								end++
							}
							key = r.typ
							break
						}
					}
				}
				if end < 0 {
					continue
				}
				found = true
				text, stripped := l.lstripText(s[pos:i], sign, key == jtVariableBegin, lineStarting)
				newlinesStripped = stripped
				if len(text) > 0 {
					l.emit(l.line, jtData, string(text))
				}
				l.line += countNL(text) + newlinesStripped
				newlinesStripped = 0
				value := s[i:end]
				l.emit(l.line, key, string(value))
				l.line += countNL(value)
				lineStarting = value[len(value)-1] == '\n'
				stack = append(stack, key)
				pos = end
			}
			if found {
				continue
			}
			if pos < len(s) {
				rest := s[pos:]
				l.emit(l.line, jtData, string(rest))
				l.line += countNL(rest)
				lineStarting = rest[len(rest)-1] == '\n'
				pos = len(s)
				continue
			}
			return nil

		case jtCommentBegin:
			found := false
			for i := pos; i < len(s); i++ {
				if e := l.matchEnd(i, l.ce); e >= 0 {
					text, value := s[pos:i], s[i:e]
					l.line += countNL(text) + countNL(value)
					lineStarting = value[len(value)-1] == '\n'
					stack = stack[:len(stack)-1]
					pos = e
					found = true
					break
				}
			}
			if found {
				continue
			}
			if pos < len(s) {
				jfail("Missing end of comment tag", l.line)
			}
			return nil

		case jtRawBegin:
			found := false
			for i := pos; i < len(s); i++ {
				e, sign := l.matchEndRaw(i)
				if e < 0 {
					continue
				}
				text, stripped := l.lstripText(s[pos:i], sign, false, lineStarting)
				newlinesStripped = stripped
				if len(text) > 0 {
					l.emit(l.line, jtData, string(text))
				}
				l.line += countNL(text) + newlinesStripped
				newlinesStripped = 0
				value := s[i:e]
				l.emit(l.line, jtRawEnd, string(value))
				l.line += countNL(value)
				lineStarting = value[len(value)-1] == '\n'
				stack = stack[:len(stack)-1]
				pos = e
				found = true
				break
			}
			if found {
				continue
			}
			if pos < len(s) {
				jfail("Missing end of raw directive", l.line)
			}
			return nil

		default: // block_begin, variable_begin
			if pos >= len(s) {
				return nil
			}
			end := -1
			if len(balancing) == 0 {
				if top == jtBlockBegin {
					end = l.matchEnd(pos, l.be)
				} else if pos < len(s) && s[pos] == '-' && hasRunesAt(s, pos+1, l.ve) {
					end = skipSpace(s, pos+1+len(l.ve))
				} else if hasRunesAt(s, pos, l.ve) {
					end = pos + len(l.ve)
				}
			}
			if end >= 0 {
				value := s[pos:end]
				typ := jtBlockEnd
				if top == jtVariableBegin {
					typ = jtVariableEnd
				}
				l.emit(l.line, typ, string(value))
				l.line += countNL(value)
				lineStarting = value[len(value)-1] == '\n'
				stack = stack[:len(stack)-1]
				pos = end
				continue
			}
			typ, end := l.matchTagToken(pos)
			if end < 0 {
				jfail(fmt.Sprintf("unexpected char %s at %d", pyStrRepr(string(s[pos])), pos), l.line)
			}
			value := s[pos:end]
			if typ == jtOperator {
				switch d := string(value); d {
				case "{":
					balancing = append(balancing, '}')
				case "(":
					balancing = append(balancing, ')')
				case "[":
					balancing = append(balancing, ']')
				case "}", ")", "]":
					if len(balancing) == 0 {
						jfail(fmt.Sprintf("unexpected '%s'", d), l.line)
					}
					want := balancing[len(balancing)-1]
					balancing = balancing[:len(balancing)-1]
					if string(want) != d {
						jfail(fmt.Sprintf("unexpected '%s', expected '%s'", d, string(want)), l.line)
					}
				}
			}
			l.emit(l.line, typ, string(value))
			l.line += countNL(value)
			lineStarting = value[len(value)-1] == '\n'
			pos = end
		}
	}
}

// matchDigits matches (\d+_)*\d+ at i.
func matchDigits(s []rune, i int) int {
	j := i
	end := -1
	for {
		k := j
		for k < len(s) && isPyDigit(s[k]) {
			k++
		}
		if k == j {
			return end
		}
		end = k
		if k < len(s) && s[k] == '_' && k+1 < len(s) && isPyDigit(s[k+1]) {
			j = k + 1
			continue
		}
		return end
	}
}

func (l *jlexer) matchFloat(i int) int {
	s := l.src
	if i > 0 && s[i-1] == '.' {
		return -1
	}
	d := matchDigits(s, i)
	if d < 0 {
		return -1
	}
	exponent := func(j int) int {
		if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
			j++
			if j < len(s) && (s[j] == '+' || s[j] == '-') {
				j++
			}
			return matchDigits(s, j)
		}
		return -1
	}
	if d < len(s) && s[d] == '.' {
		if f := matchDigits(s, d+1); f >= 0 {
			if e := exponent(f); e >= 0 {
				return e
			}
		}
	}
	if e := exponent(d); e >= 0 {
		return e
	}
	if d < len(s) && s[d] == '.' {
		if f := matchDigits(s, d+1); f >= 0 {
			return f
		}
	}
	return -1
}

func (l *jlexer) matchInteger(i int) int {
	s := l.src
	radix := func(j int, ok func(rune) bool) int {
		end := -1
		for {
			k := j
			if k < len(s) && s[k] == '_' {
				k++
			}
			if k < len(s) && ok(s[k]) {
				j = k + 1
				end = j
				continue
			}
			return end
		}
	}
	if i+1 < len(s) && s[i] == '0' {
		var ok func(rune) bool
		switch s[i+1] {
		case 'b', 'B':
			ok = func(r rune) bool { return r == '0' || r == '1' }
		case 'o', 'O':
			ok = func(r rune) bool { return r >= '0' && r <= '7' }
		case 'x', 'X':
			ok = func(r rune) bool { return isPyDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') }
		}
		if ok != nil {
			if e := radix(i+2, ok); e >= 0 {
				return e
			}
		}
	}
	if i < len(s) && isPyDigit(s[i]) && s[i] != '0' {
		return max(radix(i+1, isPyDigit), i+1)
	}
	if i < len(s) && s[i] == '0' {
		return max(radix(i+1, func(r rune) bool { return r == '0' }), i+1)
	}
	return -1
}

func (l *jlexer) matchTagToken(i int) (string, int) {
	s := l.src
	if isPySpace(s[i]) {
		return jtWhitespace, skipSpace(s, i)
	}
	if e := l.matchFloat(i); e >= 0 {
		return jtFloat, e
	}
	if e := l.matchInteger(i); e >= 0 {
		return jtInteger, e
	}
	if isPyWordRune(s[i]) {
		j := i
		for j < len(s) && isPyWordRune(s[j]) {
			j++
		}
		return jtName, j
	}
	if q := s[i]; q == '\'' || q == '"' {
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case q:
				return jtString, j + 1
			}
		}
	}
	if i+1 < len(s) {
		if _, ok := jOperators[string(s[i:i+2])]; ok {
			return jtOperator, i + 2
		}
	}
	if _, ok := jOperators[string(s[i])]; ok {
		return jtOperator, i + 1
	}
	return "", -1
}

// jinjaTokens lexes src as Jinja's Lexer.tokenize does (tokeniter, the
// AnsibleLexer backslash escaping, then wrap): the parser's tokens, and
// the error to raise once they run out.
func jinjaTokens(src string, o Options, state string, escapeBackslashes bool) ([]jtok, *jinjaSyntaxError) {
	bs, be, vs, ve, cs, ce := o.delims()
	l := &jlexer{bs: []rune(bs), be: []rune(be), vs: []rune(vs), ve: []rune(ve), cs: []rune(cs), ce: []rune(ce),
		trim: o.TrimBlocks, lstrip: o.LstripBlocks, escapeBackslashes: escapeBackslashes}
	// newline_re.split(source)[::2], joined with "\n".
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	if !o.KeepTrailingNewline {
		src = strings.TrimSuffix(src, "\n")
	}
	l.src = []rune(src)
	lexErr := l.tokeniter(state)

	nl := o.NewlineSequence
	if nl == "" {
		nl = "\n"
	}
	out := make([]jtok, 0, len(l.toks))
	inVariable := false
	for _, t := range l.toks {
		switch t.typ {
		case jtVariableBegin:
			inVariable = true
		case jtVariableEnd:
			inVariable = false
		}
		switch t.typ {
		case jtCommentBegin, jtComment, jtCommentEnd, jtWhitespace, jtRawBegin, jtRawEnd:
			continue
		case jtName:
			if !isPyIdentifier([]rune(t.val)) {
				return out, &jinjaSyntaxError{msg: "Invalid character in identifier", line: t.line}
			}
		case jtString:
			if !(escapeBackslashes && inVariable) {
				body := []rune(t.val)
				val := normalizeNewlines(string(body[1:len(body)-1]), nl)
				if msg, full := unicodeEscapeError(val); msg != "" {
					return out, &jinjaSyntaxError{msg: msg, line: t.line, cause: full}
				}
			}
		case jtOperator:
			t.typ = jOperators[t.val]
		}
		out = append(out, t)
	}
	return out, lexErr
}

// unicodeEscapeError is the error, if any, of
// value.encode("ascii", "backslashreplace").decode("unicode-escape"): the
// message Jinja keeps (the text after the last colon) and str() of the
// UnicodeDecodeError.
func unicodeEscapeError(val string) (string, string) {
	if !strings.Contains(val, `\`) {
		return "", ""
	}
	var b []byte
	for _, r := range val {
		switch {
		case r < 0x80:
			b = append(b, byte(r))
		case r <= 0xff:
			b = append(b, fmt.Sprintf(`\x%02x`, r)...)
		case r <= 0xffff:
			b = append(b, fmt.Sprintf(`\u%04x`, r)...)
		default:
			b = append(b, fmt.Sprintf(`\U%08x`, r)...)
		}
	}
	isHex := func(c byte) bool {
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	fail := func(msg string, start, end int) (string, string) {
		var full string
		if end-start == 1 {
			full = fmt.Sprintf("'unicodeescape' codec can't decode byte 0x%02x in position %d: %s", b[start], start, msg)
		} else {
			full = fmt.Sprintf("'unicodeescape' codec can't decode bytes in position %d-%d: %s", start, end-1, msg)
		}
		return msg, full
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		start := i
		i++
		if i >= len(b) {
			return fail(`\ at end of string`, start, len(b))
		}
		count, msg := 0, ""
		switch b[i] {
		case 'x':
			count, msg = 2, `truncated \xXX escape`
		case 'u':
			count, msg = 4, `truncated \uXXXX escape`
		case 'U':
			count, msg = 8, `truncated \UXXXXXXXX escape`
		case 'N':
			msg = `malformed \N character escape`
			j := i + 1
			if j >= len(b) || b[j] != '{' {
				return fail(msg, start, min(j, len(b)))
			}
			k := j + 1
			for k < len(b) && b[k] != '}' {
				k++
			}
			if k >= len(b) {
				return fail(msg, start, len(b))
			}
			if k == j+1 {
				return fail(msg, start, k)
			}
			i = k
			continue
		default:
			continue
		}
		var ch uint64
		j := i + 1
		for ; count > 0; count-- {
			if j >= len(b) {
				return fail(msg, start, len(b))
			}
			if !isHex(b[j]) {
				return fail(msg, start, j)
			}
			var d byte
			switch c := b[j]; {
			case c >= '0' && c <= '9':
				d = c - '0'
			case c >= 'a' && c <= 'f':
				d = c - 'a' + 10
			default:
				d = c - 'A' + 10
			}
			ch = ch<<4 | uint64(d)
			j++
		}
		if ch > unicode.MaxRune {
			return fail("illegal Unicode character", start, j)
		}
		i = j - 1
	}
	return "", ""
}

// ---------------------------------------------------------------------
// Token stream

type jstream struct {
	toks    []jtok
	next_   int
	lexErr  *jinjaSyntaxError
	current jtok
	pushed  []jtok
}

func newJStream(toks []jtok, lexErr *jinjaSyntaxError) *jstream {
	s := &jstream{toks: toks, lexErr: lexErr, current: jtok{1, jtInitial, ""}}
	s.next()
	return s
}

func (s *jstream) next() jtok {
	rv := s.current
	if len(s.pushed) > 0 {
		s.current = s.pushed[0]
		s.pushed = s.pushed[1:]
	} else if s.current.typ != jtEOF {
		if s.next_ < len(s.toks) {
			s.current = s.toks[s.next_]
			s.next_++
		} else if s.lexErr != nil {
			panic(s.lexErr)
		} else {
			s.current = jtok{s.current.line, jtEOF, ""}
		}
	}
	return rv
}

func (s *jstream) eos() bool { return len(s.pushed) == 0 && s.current.typ == jtEOF }

func (s *jstream) look() jtok {
	old := s.next()
	result := s.current
	s.pushed = append(s.pushed, result)
	s.current = old
	return result
}

func (s *jstream) skip(n int) {
	for range n {
		s.next()
	}
}

func (s *jstream) skipIf(expr string) bool {
	if s.current.test(expr) {
		s.next()
		return true
	}
	return false
}

func (s *jstream) expect(expr string) jtok {
	if !s.current.test(expr) {
		d := jDescribeExpr(expr)
		if s.current.typ == jtEOF {
			jfail(fmt.Sprintf("unexpected end of template, expected %s.", pyStrRepr(d)), s.current.line)
		}
		jfail(fmt.Sprintf("expected token %s, got %s", pyStrRepr(d), pyStrRepr(jDescribeToken(s.current))), s.current.line)
	}
	return s.next()
}

// ---------------------------------------------------------------------
// Parser

// jnode is a jinja2.nodes node, with only what the checks need.
type jnode struct {
	kind string // the node class name
	line int
	name string
	ctx  string
	data string // TemplateData text

	node             *jnode // Getattr/Getitem/Filter/Test/Call/unary operand
	left, right      *jnode
	items            []*jnode // List/Tuple items, Output nodes, Concat, Compare operands, Dict keys and values
	args, kwargs     []*jnode
	dynArgs, dynKwgs *jnode
	test             *jnode
	target           *jnode
	iter             *jnode
	body, else_      []*jnode
	elifs            []*jnode
	defaults         []*jnode
	call, filter     *jnode
	template         *jnode
	expr1, expr2     *jnode
	required         bool
}

func (n *jnode) canAssign() bool {
	switch n.kind {
	case "Name":
		switch n.name {
		case "true", "false", "none", "True", "False", "None":
			return false
		}
		return true
	case "NSRef":
		return true
	case "Tuple":
		for _, it := range n.items {
			if !it.canAssign() {
				return false
			}
		}
		return true
	}
	return false
}

func (n *jnode) setCtx(ctx string) {
	switch n.kind {
	case "Name", "Tuple", "List", "Getattr", "Getitem":
		n.ctx = ctx
	}
	if n.kind == "Tuple" {
		for _, it := range n.items {
			it.setCtx(ctx)
		}
	}
}

var jStatementKeywords = map[string]bool{"for": true, "if": true, "block": true, "extends": true,
	"print": true, "macro": true, "include": true, "from": true, "import": true, "set": true,
	"with": true, "autoescape": true}

var jCompareOps = map[string]bool{"eq": true, "ne": true, "lt": true, "lteq": true, "gt": true, "gteq": true}

var jMathNodes = map[string]string{"add": "Add", "sub": "Sub", "mul": "Mul", "div": "Div", "floordiv": "FloorDiv", "mod": "Mod"}

type jparser struct {
	s           *jstream
	tagStack    []string
	endTokStack [][]string
}

func (p *jparser) fail(msg string, line int) { jfail(msg, line) }

func (p *jparser) failCur(msg string) { jfail(msg, p.s.current.line) }

func (p *jparser) failUtEOF(name string, haveName bool, stack [][]string, line int) {
	expected := map[string]bool{}
	for _, exprs := range stack {
		for _, e := range exprs {
			expected[jDescribeExpr(e)] = true
		}
	}
	currently := ""
	if len(stack) > 0 {
		var parts []string
		for _, e := range stack[len(stack)-1] {
			parts = append(parts, pyStrRepr(jDescribeExpr(e)))
		}
		currently = strings.Join(parts, " or ")
	}
	var msg []string
	if !haveName {
		msg = append(msg, "Unexpected end of template.")
	} else {
		msg = append(msg, fmt.Sprintf("Encountered unknown tag %s.", pyStrRepr(name)))
	}
	if currently != "" {
		if haveName && expected[name] {
			msg = append(msg, "You probably made a nesting mistake. Jinja is expecting this tag,"+
				fmt.Sprintf(" but currently looking for %s.", currently))
		} else {
			msg = append(msg, fmt.Sprintf("Jinja was looking for the following tags: %s.", currently))
		}
	}
	if len(p.tagStack) > 0 {
		msg = append(msg, fmt.Sprintf("The innermost block that needs to be closed is %s.", pyStrRepr(p.tagStack[len(p.tagStack)-1])))
	}
	p.fail(strings.Join(msg, " "), line)
}

func (p *jparser) failEOF(endTokens []string, line int) {
	stack := append([][]string(nil), p.endTokStack...)
	if endTokens != nil {
		stack = append(stack, endTokens)
	}
	p.failUtEOF("", false, stack, line)
}

func (p *jparser) isTupleEnd(extra []string) bool {
	switch p.s.current.typ {
	case jtVariableEnd, jtBlockEnd, "rparen":
		return true
	}
	return extra != nil && p.s.current.testAny(extra...)
}

func (p *jparser) parseStatement() []*jnode {
	tok := p.s.current
	if tok.typ != jtName {
		p.fail("tag name expected", tok.line)
	}
	p.tagStack = append(p.tagStack, tok.val)
	defer func() { p.tagStack = p.tagStack[:len(p.tagStack)-1] }()
	switch tok.val {
	case "for":
		return []*jnode{p.parseFor()}
	case "if":
		return []*jnode{p.parseIf()}
	case "block":
		return []*jnode{p.parseBlock()}
	case "extends":
		n := &jnode{kind: "Extends", line: p.s.next().line}
		n.template = p.parseExpression(true)
		return []*jnode{n}
	case "print":
		n := &jnode{kind: "Output", line: p.s.next().line}
		for p.s.current.typ != jtBlockEnd {
			if len(n.items) > 0 {
				p.s.expect("comma")
			}
			n.items = append(n.items, p.parseExpression(true))
		}
		return []*jnode{n}
	case "macro":
		n := &jnode{kind: "Macro", line: p.s.next().line}
		n.name = p.parseAssignTarget(true, true, nil, false).name
		p.parseSignature(n)
		n.body = p.parseStatements([]string{"name:endmacro"}, true)
		return []*jnode{n}
	case "include":
		n := &jnode{kind: "Include", line: p.s.next().line}
		n.template = p.parseExpression(true)
		if p.s.current.test("name:ignore") && p.s.look().test("name:missing") {
			p.s.skip(2)
		}
		p.parseImportContext()
		return []*jnode{n}
	case "from":
		return []*jnode{p.parseFrom()}
	case "import":
		n := &jnode{kind: "Import", line: p.s.next().line}
		n.template = p.parseExpression(true)
		p.s.expect("name:as")
		p.parseAssignTarget(true, true, nil, false)
		p.parseImportContext()
		return []*jnode{n}
	case "set":
		return []*jnode{p.parseSet()}
	case "with":
		return []*jnode{p.parseWith()}
	case "autoescape":
		n := &jnode{kind: "ScopedEvalContextModifier", line: p.s.next().line}
		n.items = []*jnode{p.parseExpression(true)}
		n.body = p.parseStatements([]string{"name:endautoescape"}, true)
		return []*jnode{{kind: "Scope", line: n.line, body: []*jnode{n}}}
	case "call":
		return []*jnode{p.parseCallBlock()}
	case "filter":
		n := &jnode{kind: "FilterBlock", line: p.s.next().line}
		n.filter = p.parseFilter(nil, true)
		n.body = p.parseStatements([]string{"name:endfilter"}, true)
		return []*jnode{n}
	}
	// Unknown: the tag leaves the stack before the error is worded.
	p.tagStack = p.tagStack[:len(p.tagStack)-1]
	defer func() { p.tagStack = append(p.tagStack, "") }()
	p.failUtEOF(tok.val, true, p.endTokStack, tok.line)
	return nil
}

func (p *jparser) parseStatements(endTokens []string, dropNeedle bool) []*jnode {
	p.s.skipIf("colon")
	p.s.expect(jtBlockEnd)
	result := p.subparse(endTokens)
	if p.s.current.typ == jtEOF {
		p.failEOF(endTokens, p.s.current.line)
	}
	if dropNeedle {
		p.s.next()
	}
	return result
}

func (p *jparser) parseSet() *jnode {
	line := p.s.next().line
	target := p.parseAssignTarget(true, false, nil, true)
	if p.s.skipIf("assign") {
		return &jnode{kind: "Assign", line: line, target: target, node: p.parseTuple(false, true, nil, false, false)}
	}
	n := &jnode{kind: "AssignBlock", line: line, target: target}
	n.filter = p.parseFilter(nil, false)
	n.body = p.parseStatements([]string{"name:endset"}, true)
	return n
}

func (p *jparser) parseFor() *jnode {
	n := &jnode{kind: "For", line: p.s.expect("name:for").line}
	n.target = p.parseAssignTarget(true, false, []string{"name:in"}, false)
	p.s.expect("name:in")
	n.iter = p.parseTuple(false, false, []string{"name:recursive"}, false, false)
	if p.s.skipIf("name:if") {
		n.test = p.parseExpression(true)
	}
	p.s.skipIf("name:recursive")
	n.body = p.parseStatements([]string{"name:endfor", "name:else"}, false)
	if p.s.next().val != "endfor" {
		n.else_ = p.parseStatements([]string{"name:endfor"}, true)
	}
	return n
}

func (p *jparser) parseIf() *jnode {
	result := &jnode{kind: "If", line: p.s.expect("name:if").line}
	node := result
	for {
		node.test = p.parseTuple(false, false, nil, false, false)
		node.body = p.parseStatements([]string{"name:elif", "name:else", "name:endif"}, false)
		tok := p.s.next()
		if tok.test("name:elif") {
			node = &jnode{kind: "If", line: p.s.current.line}
			result.elifs = append(result.elifs, node)
			continue
		} else if tok.test("name:else") {
			result.else_ = p.parseStatements([]string{"name:endif"}, true)
		}
		break
	}
	return result
}

func (p *jparser) parseWith() *jnode {
	n := &jnode{kind: "With", line: p.s.next().line}
	for p.s.current.typ != jtBlockEnd {
		if len(n.args) > 0 {
			p.s.expect("comma")
		}
		target := p.parseAssignTarget(true, false, nil, false)
		target.setCtx("param")
		n.args = append(n.args, target)
		p.s.expect("assign")
		n.items = append(n.items, p.parseExpression(true))
	}
	n.body = p.parseStatements([]string{"name:endwith"}, true)
	return n
}

func (p *jparser) parseBlock() *jnode {
	n := &jnode{kind: "Block", line: p.s.next().line}
	n.name = p.s.expect(jtName).val
	p.s.skipIf("name:scoped")
	n.required = p.s.skipIf("name:required")
	if p.s.current.typ == "sub" {
		p.failCur("Block names in Jinja have to be valid Python identifiers and may not" +
			" contain hyphens, use an underscore instead.")
	}
	n.body = p.parseStatements([]string{"name:endblock"}, true)
	if n.required {
		for _, b := range n.body {
			if b.kind != "Output" {
				p.failCur("Required blocks can only contain comments or whitespace")
			}
			for _, o := range b.items {
				if o.kind != "TemplateData" || strings.TrimFunc(o.data, isPySpace) != "" || o.data == "" {
					p.failCur("Required blocks can only contain comments or whitespace")
				}
			}
		}
	}
	p.s.skipIf("name:" + n.name)
	return n
}

func (p *jparser) parseImportContext() {
	if p.s.current.testAny("name:with", "name:without") && p.s.look().test("name:context") {
		p.s.next()
		p.s.skip(1)
	}
}

func (p *jparser) parseFrom() *jnode {
	n := &jnode{kind: "FromImport", line: p.s.next().line}
	n.template = p.parseExpression(true)
	p.s.expect("name:import")
	parseContext := func() bool {
		if (p.s.current.val == "with" || p.s.current.val == "without") && p.s.look().test("name:context") {
			p.s.next()
			p.s.skip(1)
			return true
		}
		return false
	}
	names := 0
	for {
		if names > 0 {
			p.s.expect("comma")
		}
		if p.s.current.typ == jtName {
			if parseContext() {
				break
			}
			target := p.parseAssignTarget(true, true, nil, false)
			if strings.HasPrefix(target.name, "_") {
				p.fail("names starting with an underline can not be imported", target.line)
			}
			if p.s.skipIf("name:as") {
				p.parseAssignTarget(true, true, nil, false)
			}
			names++
			if parseContext() || p.s.current.typ != "comma" {
				break
			}
		} else {
			p.s.expect(jtName)
		}
	}
	return n
}

func (p *jparser) parseSignature(n *jnode) {
	p.s.expect("lparen")
	for p.s.current.typ != "rparen" {
		if len(n.args) > 0 {
			p.s.expect("comma")
		}
		arg := p.parseAssignTarget(true, true, nil, false)
		arg.setCtx("param")
		if p.s.skipIf("assign") {
			n.defaults = append(n.defaults, p.parseExpression(true))
		} else if len(n.defaults) > 0 {
			p.failCur("non-default argument follows default argument")
		}
		n.args = append(n.args, arg)
	}
	p.s.expect("rparen")
}

func (p *jparser) parseCallBlock() *jnode {
	n := &jnode{kind: "CallBlock", line: p.s.next().line}
	if p.s.current.typ == "lparen" {
		p.parseSignature(n)
	}
	call := p.parseExpression(true)
	if call.kind != "Call" {
		p.fail("expected call", n.line)
	}
	n.call = call
	n.body = p.parseStatements([]string{"name:endcall"}, true)
	return n
}

func (p *jparser) parseAssignTarget(withTuple, nameOnly bool, extraEnd []string, withNamespace bool) *jnode {
	var target *jnode
	if nameOnly {
		tok := p.s.expect(jtName)
		target = &jnode{kind: "Name", name: tok.val, ctx: "store", line: tok.line}
	} else {
		if withTuple {
			target = p.parseTuple(true, true, extraEnd, false, withNamespace)
		} else {
			target = p.parsePrimary(withNamespace)
		}
		target.setCtx("store")
	}
	if !target.canAssign() {
		p.fail(fmt.Sprintf("can't assign to %s", pyStrRepr(strings.ToLower(target.kind))), target.line)
	}
	return target
}

func (p *jparser) parseExpression(withCondexpr bool) *jnode {
	if withCondexpr {
		return p.parseCondexpr()
	}
	return p.parseOr()
}

func (p *jparser) parseCondexpr() *jnode {
	line := p.s.current.line
	expr1 := p.parseOr()
	for p.s.skipIf("name:if") {
		test := p.parseOr()
		var expr3 *jnode
		if p.s.skipIf("name:else") {
			expr3 = p.parseCondexpr()
		}
		expr1 = &jnode{kind: "CondExpr", line: line, test: test, expr1: expr1, expr2: expr3}
		line = p.s.current.line
	}
	return expr1
}

func (p *jparser) parseOr() *jnode {
	line := p.s.current.line
	left := p.parseAnd()
	for p.s.skipIf("name:or") {
		right := p.parseAnd()
		left = &jnode{kind: "Or", line: line, left: left, right: right}
		line = p.s.current.line
	}
	return left
}

func (p *jparser) parseAnd() *jnode {
	line := p.s.current.line
	left := p.parseNot()
	for p.s.skipIf("name:and") {
		right := p.parseNot()
		left = &jnode{kind: "And", line: line, left: left, right: right}
		line = p.s.current.line
	}
	return left
}

func (p *jparser) parseNot() *jnode {
	if p.s.current.test("name:not") {
		line := p.s.next().line
		return &jnode{kind: "Not", line: line, node: p.parseNot()}
	}
	return p.parseCompare()
}

func (p *jparser) parseCompare() *jnode {
	line := p.s.current.line
	expr := p.parseMath1()
	var ops []*jnode
	for {
		typ := p.s.current.typ
		if jCompareOps[typ] {
			p.s.next()
			ops = append(ops, p.parseMath1())
		} else if p.s.skipIf("name:in") {
			ops = append(ops, p.parseMath1())
		} else if p.s.current.test("name:not") && p.s.look().test("name:in") {
			p.s.skip(2)
			ops = append(ops, p.parseMath1())
		} else {
			break
		}
		line = p.s.current.line
	}
	if len(ops) == 0 {
		return expr
	}
	return &jnode{kind: "Compare", line: line, node: expr, items: ops}
}

func (p *jparser) parseMath1() *jnode {
	line := p.s.current.line
	left := p.parseConcat()
	for p.s.current.typ == "add" || p.s.current.typ == "sub" {
		kind := jMathNodes[p.s.current.typ]
		p.s.next()
		right := p.parseConcat()
		left = &jnode{kind: kind, line: line, left: left, right: right}
		line = p.s.current.line
	}
	return left
}

func (p *jparser) parseConcat() *jnode {
	line := p.s.current.line
	args := []*jnode{p.parseMath2()}
	for p.s.current.typ == "tilde" {
		p.s.next()
		args = append(args, p.parseMath2())
	}
	if len(args) == 1 {
		return args[0]
	}
	return &jnode{kind: "Concat", line: line, items: args}
}

func (p *jparser) parseMath2() *jnode {
	line := p.s.current.line
	left := p.parsePow()
	for {
		switch p.s.current.typ {
		case "mul", "div", "floordiv", "mod":
		default:
			return left
		}
		kind := jMathNodes[p.s.current.typ]
		p.s.next()
		right := p.parsePow()
		left = &jnode{kind: kind, line: line, left: left, right: right}
		line = p.s.current.line
	}
}

func (p *jparser) parsePow() *jnode {
	line := p.s.current.line
	left := p.parseUnary(true)
	for p.s.current.typ == "pow" {
		p.s.next()
		right := p.parseUnary(true)
		left = &jnode{kind: "Pow", line: line, left: left, right: right}
		line = p.s.current.line
	}
	return left
}

func (p *jparser) parseUnary(withFilter bool) *jnode {
	typ := p.s.current.typ
	line := p.s.current.line
	var node *jnode
	switch typ {
	case "sub":
		p.s.next()
		node = &jnode{kind: "Neg", line: line, node: p.parseUnary(false)}
	case "add":
		p.s.next()
		node = &jnode{kind: "Pos", line: line, node: p.parseUnary(false)}
	default:
		node = p.parsePrimary(false)
	}
	node = p.parsePostfix(node)
	if withFilter {
		node = p.parseFilterExpr(node)
	}
	return node
}

func (p *jparser) parsePrimary(withNamespace bool) *jnode {
	tok := p.s.current
	switch tok.typ {
	case jtName:
		p.s.next()
		switch tok.val {
		case "true", "false", "True", "False", "none", "None":
			return &jnode{kind: "Const", line: tok.line}
		}
		if withNamespace && p.s.current.typ == "dot" {
			p.s.next()
			attr := p.s.expect(jtName)
			return &jnode{kind: "NSRef", line: tok.line, name: tok.val + "." + attr.val}
		}
		return &jnode{kind: "Name", line: tok.line, name: tok.val, ctx: "load"}
	case jtString:
		p.s.next()
		for p.s.current.typ == jtString {
			p.s.next()
		}
		return &jnode{kind: "Const", line: tok.line}
	case jtInteger, jtFloat:
		p.s.next()
		return &jnode{kind: "Const", line: tok.line}
	case "lparen":
		p.s.next()
		node := p.parseTuple(false, true, nil, true, false)
		p.s.expect("rparen")
		return node
	case "lbracket":
		return p.parseList()
	case "lbrace":
		return p.parseDict()
	}
	p.fail(fmt.Sprintf("unexpected %s", pyStrRepr(jDescribeToken(tok))), tok.line)
	return nil
}

func (p *jparser) parseTuple(simplified, withCondexpr bool, extraEnd []string, explicitParens, withNamespace bool) *jnode {
	line := p.s.current.line
	parse := func() *jnode {
		if simplified {
			return p.parsePrimary(withNamespace)
		}
		return p.parseExpression(withCondexpr)
	}
	var args []*jnode
	isTuple := false
	for {
		if len(args) > 0 {
			p.s.expect("comma")
		}
		if p.isTupleEnd(extraEnd) {
			break
		}
		args = append(args, parse())
		if p.s.current.typ == "comma" {
			isTuple = true
		} else {
			break
		}
		line = p.s.current.line
	}
	if !isTuple {
		if len(args) > 0 {
			return args[0]
		}
		if !explicitParens {
			p.failCur(fmt.Sprintf("Expected an expression, got %s", pyStrRepr(jDescribeToken(p.s.current))))
		}
	}
	return &jnode{kind: "Tuple", line: line, items: args, ctx: "load"}
}

func (p *jparser) parseList() *jnode {
	tok := p.s.expect("lbracket")
	n := &jnode{kind: "List", line: tok.line}
	for p.s.current.typ != "rbracket" {
		if len(n.items) > 0 {
			p.s.expect("comma")
		}
		if p.s.current.typ == "rbracket" {
			break
		}
		n.items = append(n.items, p.parseExpression(true))
	}
	p.s.expect("rbracket")
	return n
}

func (p *jparser) parseDict() *jnode {
	tok := p.s.expect("lbrace")
	n := &jnode{kind: "Dict", line: tok.line}
	for p.s.current.typ != "rbrace" {
		if len(n.items) > 0 {
			p.s.expect("comma")
		}
		if p.s.current.typ == "rbrace" {
			break
		}
		key := p.parseExpression(true)
		p.s.expect("colon")
		value := p.parseExpression(true)
		n.items = append(n.items, key, value)
	}
	p.s.expect("rbrace")
	return n
}

func (p *jparser) parsePostfix(node *jnode) *jnode {
	for {
		switch p.s.current.typ {
		case "dot", "lbracket":
			node = p.parseSubscript(node)
		case "lparen":
			node = p.parseCall(node)
		default:
			return node
		}
	}
}

func (p *jparser) parseFilterExpr(node *jnode) *jnode {
	for {
		switch {
		case p.s.current.typ == "pipe":
			node = p.parseFilter(node, false)
		case p.s.current.typ == jtName && p.s.current.val == "is":
			node = p.parseTest(node)
		case p.s.current.typ == "lparen":
			node = p.parseCall(node)
		default:
			return node
		}
	}
}

func (p *jparser) parseSubscript(node *jnode) *jnode {
	tok := p.s.next()
	if tok.typ == "dot" {
		attr := p.s.current
		p.s.next()
		if attr.typ == jtName {
			return &jnode{kind: "Getattr", line: tok.line, node: node, name: attr.val, ctx: "load"}
		} else if attr.typ != jtInteger {
			p.fail("expected name or number", attr.line)
		}
		return &jnode{kind: "Getitem", line: tok.line, node: node, ctx: "load", args: []*jnode{{kind: "Const", line: attr.line}}}
	}
	if tok.typ == "lbracket" {
		var args []*jnode
		for p.s.current.typ != "rbracket" {
			if len(args) > 0 {
				p.s.expect("comma")
			}
			args = append(args, p.parseSubscribed())
		}
		p.s.expect("rbracket")
		arg := &jnode{kind: "Tuple", line: tok.line, items: args, ctx: "load"}
		if len(args) == 1 {
			arg = args[0]
		}
		return &jnode{kind: "Getitem", line: tok.line, node: node, ctx: "load", args: []*jnode{arg}}
	}
	p.fail("expected subscript expression", tok.line)
	return nil
}

func (p *jparser) parseSubscribed() *jnode {
	line := p.s.current.line
	var parts []*jnode
	if p.s.current.typ == "colon" {
		p.s.next()
	} else {
		node := p.parseExpression(true)
		if p.s.current.typ != "colon" {
			return node
		}
		p.s.next()
		parts = append(parts, node)
	}
	switch p.s.current.typ {
	case "colon", "rbracket", "comma":
	default:
		parts = append(parts, p.parseExpression(true))
	}
	if p.s.current.typ == "colon" {
		p.s.next()
		if t := p.s.current.typ; t != "rbracket" && t != "comma" {
			parts = append(parts, p.parseExpression(true))
		}
	}
	return &jnode{kind: "Slice", line: line, items: parts}
}

func (p *jparser) parseCallArgs(n *jnode) {
	tok := p.s.expect("lparen")
	ensure := func(ok bool) {
		if !ok {
			p.fail("invalid syntax for function call expression", tok.line)
		}
	}
	requireComma := false
	for p.s.current.typ != "rparen" {
		if requireComma {
			p.s.expect("comma")
			if p.s.current.typ == "rparen" {
				break
			}
		}
		switch {
		case p.s.current.typ == "mul":
			ensure(n.dynArgs == nil && n.dynKwgs == nil)
			p.s.next()
			n.dynArgs = p.parseExpression(true)
		case p.s.current.typ == "pow":
			ensure(n.dynKwgs == nil)
			p.s.next()
			n.dynKwgs = p.parseExpression(true)
		case p.s.current.typ == jtName && p.s.look().typ == "assign":
			ensure(n.dynKwgs == nil)
			p.s.skip(2)
			n.kwargs = append(n.kwargs, p.parseExpression(true))
		default:
			ensure(n.dynArgs == nil && n.dynKwgs == nil && len(n.kwargs) == 0)
			n.args = append(n.args, p.parseExpression(true))
		}
		requireComma = true
	}
	p.s.expect("rparen")
}

func (p *jparser) parseCall(node *jnode) *jnode {
	tok := p.s.current
	n := &jnode{kind: "Call", line: tok.line, node: node}
	p.parseCallArgs(n)
	return n
}

func (p *jparser) parseFilter(node *jnode, startInline bool) *jnode {
	for p.s.current.typ == "pipe" || startInline {
		if !startInline {
			p.s.next()
		}
		tok := p.s.expect(jtName)
		name := tok.val
		for p.s.current.typ == "dot" {
			p.s.next()
			name += "." + p.s.expect(jtName).val
		}
		n := &jnode{kind: "Filter", line: tok.line, node: node, name: name}
		if p.s.current.typ == "lparen" {
			p.parseCallArgs(n)
		}
		node = n
		startInline = false
	}
	return node
}

func (p *jparser) parseTest(node *jnode) *jnode {
	tok := p.s.next()
	negated := false
	if p.s.current.test("name:not") {
		p.s.next()
		negated = true
	}
	name := p.s.expect(jtName).val
	for p.s.current.typ == "dot" {
		p.s.next()
		name += "." + p.s.expect(jtName).val
	}
	n := &jnode{kind: "Test", line: tok.line, node: node, name: name}
	switch cur := p.s.current; {
	case cur.typ == "lparen":
		p.parseCallArgs(n)
	case (cur.typ == jtName || cur.typ == jtString || cur.typ == jtInteger || cur.typ == jtFloat ||
		cur.typ == "lparen" || cur.typ == "lbracket" || cur.typ == "lbrace") &&
		!cur.testAny("name:else", "name:or", "name:and"):
		if cur.test("name:is") {
			p.failCur("You cannot chain multiple tests with is")
		}
		arg := p.parsePostfix(p.parsePrimary(false))
		n.args = []*jnode{arg}
	}
	if negated {
		return &jnode{kind: "Not", line: tok.line, node: n}
	}
	return n
}

func (p *jparser) subparse(endTokens []string) []*jnode {
	var body []*jnode
	var data []*jnode
	if endTokens != nil {
		p.endTokStack = append(p.endTokStack, endTokens)
		defer func() { p.endTokStack = p.endTokStack[:len(p.endTokStack)-1] }()
	}
	flush := func() {
		if len(data) > 0 {
			body = append(body, &jnode{kind: "Output", line: data[0].line, items: data})
			data = nil
		}
	}
	for !p.s.eos() {
		tok := p.s.current
		switch tok.typ {
		case jtData:
			if tok.val != "" {
				data = append(data, &jnode{kind: "TemplateData", line: tok.line, data: tok.val})
			}
			p.s.next()
		case jtVariableBegin:
			p.s.next()
			data = append(data, p.parseTuple(false, true, nil, false, false))
			p.s.expect(jtVariableEnd)
		case jtBlockBegin:
			flush()
			p.s.next()
			if endTokens != nil && p.s.current.testAny(endTokens...) {
				return body
			}
			body = append(body, p.parseStatement()...)
			p.s.expect(jtBlockEnd)
		default:
			panic("internal parsing error")
		}
	}
	flush()
	return body
}

// ---------------------------------------------------------------------
// Code generator checks

// jframe is the part of a compiler Frame the checks read.
type jframe struct {
	soft     bool // an if-statement or conditional expression
	toplevel bool
}

func (f jframe) inner() jframe { return jframe{} }
func (f jframe) softened() jframe {
	f.soft = true
	return f
}

type jcompiler struct {
	hasFilter func(name string) bool
	hasTest   func(name string) bool
}

func jfindAll(nodes []*jnode, kind string, out *[]*jnode) {
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.kind == kind {
			*out = append(*out, n)
		}
		kids := []*jnode{n.node, n.left, n.right, n.dynArgs, n.dynKwgs, n.test, n.target, n.iter,
			n.call, n.filter, n.template, n.expr1, n.expr2}
		kids = append(kids, n.items...)
		kids = append(kids, n.args...)
		kids = append(kids, n.kwargs...)
		kids = append(kids, n.defaults...)
		kids = append(kids, n.body...)
		kids = append(kids, n.elifs...)
		kids = append(kids, n.else_...)
		jfindAll(kids, kind, out)
	}
}

func (c *jcompiler) template(body []*jnode) {
	var blocks []*jnode
	jfindAll(body, "Block", &blocks)
	seen := map[string]bool{}
	for _, b := range blocks {
		if seen[b.name] {
			jfail(fmt.Sprintf("block %s defined twice", pyStrRepr(b.name)), b.line)
		}
		seen[b.name] = true
	}
	c.visitAll(body, jframe{toplevel: true})
	for _, b := range blocks {
		c.visitAll(b.body, jframe{})
	}
}

func (c *jcompiler) visitAll(nodes []*jnode, f jframe) {
	for _, n := range nodes {
		c.visit(n, f)
	}
}

func (c *jcompiler) signature(n *jnode, f jframe) {
	c.visitAll(n.args, f)
	c.visitAll(n.kwargs, f)
	c.visit(n.dynArgs, f)
	c.visit(n.dynKwgs, f)
}

func (c *jcompiler) visit(n *jnode, f jframe) {
	if n == nil {
		return
	}
	switch n.kind {
	case "Filter", "Test":
		if !f.soft {
			known, what := c.hasFilter, "filter"
			if n.kind == "Test" {
				known, what = c.hasTest, "test"
			}
			if known != nil && !known(n.name) {
				jfail(fmt.Sprintf("No %s named %s.", what, pyStrRepr(n.name)), n.line)
			}
		}
		c.visit(n.node, f)
		c.signature(n, f)
	case "Call":
		c.visit(n.node, f)
		c.signature(n, f)
	case "CondExpr":
		f = f.softened()
		c.visit(n.expr1, f)
		c.visit(n.test, f)
		c.visit(n.expr2, f)
	case "If":
		f = f.softened()
		c.visit(n.test, f)
		c.visitAll(n.body, f)
		for _, e := range n.elifs {
			c.visit(e.test, f)
			c.visitAll(e.body, f)
		}
		c.visitAll(n.else_, f)
	case "For":
		c.visit(n.test, f.inner())
		var names []*jnode
		jfindAll([]*jnode{n}, "Name", &names)
		for _, name := range names {
			if name.ctx == "store" && name.name == "loop" {
				jfail("Can't assign to special loop variable in for-loop target", name.line)
			}
		}
		c.visit(n.iter, f)
		c.visitAll(n.body, f.inner())
		c.visitAll(n.else_, f.inner())
	case "Extends":
		if !f.toplevel {
			jfail("cannot use extend from a non top-level scope", n.line)
		}
		c.visit(n.template, f)
	case "Macro":
		c.visitAll(n.defaults, f.inner())
		c.visitAll(n.body, f.inner())
	case "CallBlock":
		c.visitAll(n.defaults, f.inner())
		c.visitAll(n.body, f.inner())
		c.visit(n.call, f)
	case "FilterBlock", "AssignBlock":
		c.visitAll(n.body, f.inner())
		c.visit(n.filter, f.inner())
	case "With":
		c.visitAll(n.items, f)
		c.visitAll(n.body, f.inner())
	case "Scope":
		c.visitAll(n.body, f.inner())
	case "ScopedEvalContextModifier":
		c.visitAll(n.items, f)
		c.visitAll(n.body, f)
	case "Block":
		// compiled on its own, after the template body
	case "Assign":
		c.visit(n.node, f)
	case "Getitem":
		c.visit(n.node, f)
		c.visitAll(n.args, f)
	default:
		c.visit(n.node, f)
		c.visit(n.left, f)
		c.visit(n.right, f)
		c.visit(n.template, f)
		c.visitAll(n.items, f)
	}
}

// ---------------------------------------------------------------------
// Entry points

type jinjaCheckKey struct {
	src        string
	opts       Options
	expression bool
	escape     bool
}

var jinjaCheckCache sync.Map // jinjaCheckKey -> *jinjaSyntaxError (nil: compiles)
var jinjaCheckCacheSize int64
var jinjaCheckCacheMu sync.Mutex

// jinjaCheck compiles src as ansible-core's Templar would (a template,
// or with expression set, Environment.compile_expression) and returns the
// TemplateSyntaxError Jinja raises, if any. escapeBackslashes is the
// AnsibleLexer's escaping of string literals in {{ }}.
func (e *Engine) jinjaCheck(src string, o Options, expression, escapeBackslashes bool) *jinjaSyntaxError {
	key := jinjaCheckKey{src, o, expression, escapeBackslashes}
	if v, ok := jinjaCheckCache.Load(key); ok {
		return v.(*jinjaSyntaxError)
	}
	err := e.jinjaCompile(src, o, expression, escapeBackslashes)
	jinjaCheckCacheMu.Lock()
	if jinjaCheckCacheSize > 20000 {
		jinjaCheckCache.Clear()
		jinjaCheckCacheSize = 0
	}
	jinjaCheckCacheSize++
	jinjaCheckCacheMu.Unlock()
	jinjaCheckCache.Store(key, err)
	return err
}

func (e *Engine) jinjaCompile(src string, o Options, expression, escapeBackslashes bool) (err *jinjaSyntaxError) {
	defer func() {
		if r := recover(); r != nil {
			je, ok := r.(*jinjaSyntaxError)
			if !ok {
				panic(r)
			}
			err = je
		}
	}()
	state := ""
	if expression {
		state = "variable"
	}
	toks, lexErr := jinjaTokens(src, o, state, escapeBackslashes)
	p := &jparser{s: newJStream(toks, lexErr)}
	var body []*jnode
	if expression {
		expr := p.parseExpression(true)
		if !p.s.eos() {
			jfail("chunk after expression", p.s.current.line)
		}
		body = []*jnode{{kind: "Assign", line: 1, node: expr}}
	} else {
		body = p.subparse(nil)
	}
	c := &jcompiler{hasFilter: e.knownFilter, hasTest: e.knownTest}
	c.template(body)
	return nil
}

// knownFilter reports whether ansible-core would resolve a filter name:
// understudy's filters, by short name or as ansible.builtin/ansible.legacy
// (or another collection's) fully qualified name.
func (e *Engine) knownFilter(name string) bool {
	_, ok := e.Filters[pluginShortName(name)]
	return ok
}

func (e *Engine) knownTest(name string) bool {
	_, ok := e.Tests[pluginShortName(name)]
	return ok
}

func pluginShortName(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
