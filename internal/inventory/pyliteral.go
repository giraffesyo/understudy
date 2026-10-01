package inventory

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// shlexSplit is Python's shlex.split(s, comments=True) (POSIX mode), what
// the INI inventory tokenizes host lines with. Errors carry shlex's
// messages ("No closing quotation", "No escaped character").
func shlexSplit(s string) ([]string, error) {
	const whitespace = " \t\r\n"
	var out []string
	rs := []rune(s)
	i := 0
	for {
		var tok strings.Builder
		quoted := false
		state := ' '
		escaped := ' ' // the state an escape returns to
		done := false
	token:
		for {
			var c rune
			eof := i >= len(rs)
			if !eof {
				c = rs[i]
				i++
			}
			switch {
			case state == ' ':
				switch {
				case eof:
					done = true
					break token
				case strings.ContainsRune(whitespace, c):
					if tok.Len() > 0 || quoted {
						break token
					}
				case c == '#':
					// A comment runs to the end of the line.
					for i < len(rs) && rs[i] != '\n' {
						i++
					}
					if i < len(rs) {
						i++
					}
				case c == '\\':
					escaped, state = 'a', c
				case c == '"' || c == '\'':
					state = c
				default:
					tok.WriteRune(c)
					state = 'a'
				}
			case state == 'a':
				switch {
				case eof:
					done = true
					break token
				case strings.ContainsRune(whitespace, c):
					state = ' '
					if tok.Len() > 0 || quoted {
						break token
					}
				case c == '#':
					for i < len(rs) && rs[i] != '\n' {
						i++
					}
					if i < len(rs) {
						i++
					}
					state = ' '
					if tok.Len() > 0 || quoted {
						break token
					}
				case c == '"' || c == '\'':
					state = c
				case c == '\\':
					escaped, state = 'a', c
				default:
					tok.WriteRune(c)
				}
			case state == '"' || state == '\'':
				quoted = true
				switch {
				case eof:
					return nil, errors.New("No closing quotation")
				case c == state:
					state = 'a'
				case c == '\\' && state == '"':
					escaped, state = state, c
				default:
					tok.WriteRune(c)
				}
			case state == '\\':
				if eof {
					return nil, errors.New("No escaped character")
				}
				// Within quotes only the quote itself or the escape
				// character may be escaped.
				if (escaped == '"' || escaped == '\'') && c != '\\' && c != escaped {
					tok.WriteRune('\\')
				}
				tok.WriteRune(c)
				state = escaped
			}
		}
		if tok.Len() > 0 || quoted {
			out = append(out, tok.String())
		}
		if done {
			return out, nil
		}
	}
}

// literalEval is Python's ast.literal_eval as the INI inventory applies
// it to values: numbers, strings, True/False/None, lists, tuples, sets
// and dicts (tuples and sets become lists; complex numbers and ... their
// str()). ok is false when Python would raise, and the INI value then
// stays the raw string.
func literalEval(s string) (v any, ok bool) {
	p := &pyParser{src: []rune(strings.TrimLeft(s, " \t"))}
	defer func() {
		if recover() != nil {
			v, ok = nil, false
		}
	}()
	p.next()
	v = p.expr(true)
	if p.tok.kind != tEOF {
		p.fail()
	}
	return convertLiteral(v), true
}

type pyComplex struct{ re, im float64 }
type pyEllipsis struct{}
type pyTuple []any

// convertLiteral is the INI plugin's _parse_recursive_coerce_types_and_tag.
func convertLiteral(v any) any {
	switch t := v.(type) {
	case pyTuple:
		return convertLiteral([]any(t))
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = convertLiteral(e)
		}
		return out
	case *pyDict:
		m := yaml.NewOMap()
		for i, k := range t.keys {
			m.Set(template.PyStr(convertLiteral(k)), convertLiteral(t.vals[i]))
		}
		return m
	case pyEllipsis:
		return "..."
	case pyComplex:
		return complexStr(t)
	}
	return v
}

func complexStr(c pyComplex) string {
	f := func(x float64) string {
		return strings.TrimSuffix(template.PyRepr(x), ".0")
	}
	if c.re == 0 && !math.Signbit(c.re) {
		return f(c.im) + "j"
	}
	sign := "+"
	if c.im < 0 || (c.im == 0 && math.Signbit(c.im)) {
		sign = "-"
	}
	return "(" + f(c.re) + sign + f(math.Abs(c.im)) + "j)"
}

type pyDict struct {
	keys, vals []any
}

type pyTokKind int

const (
	tEOF pyTokKind = iota
	tNum
	tStr
	tName
	tOp
)

type pyTok struct {
	kind  pyTokKind
	text  string
	value any
	bytes bool // a bytes literal
}

type pyParser struct {
	src []rune
	pos int
	tok pyTok
}

func (p *pyParser) fail() { panic("literal_eval") }

func (p *pyParser) next() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\f') {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == '#' {
		p.pos = len(p.src)
	}
	if p.pos >= len(p.src) {
		p.tok = pyTok{kind: tEOF}
		return
	}
	c := p.src[p.pos]
	switch {
	case c >= '0' && c <= '9' || c == '.' && p.pos+1 < len(p.src) && p.src[p.pos+1] >= '0' && p.src[p.pos+1] <= '9':
		p.number()
	case c == '.' && p.pos+2 < len(p.src) && p.src[p.pos+1] == '.' && p.src[p.pos+2] == '.':
		p.pos += 3
		p.tok = pyTok{kind: tName, text: "..."}
	case c == '_' || unicode.IsLetter(c):
		start := p.pos
		for p.pos < len(p.src) && (p.src[p.pos] == '_' || unicode.IsLetter(p.src[p.pos]) || unicode.IsDigit(p.src[p.pos])) {
			p.pos++
		}
		name := string(p.src[start:p.pos])
		if p.pos < len(p.src) && (p.src[p.pos] == '\'' || p.src[p.pos] == '"') && isStrPrefix(name) {
			p.str(strings.ToLower(name))
			return
		}
		p.tok = pyTok{kind: tName, text: name}
	case c == '\'' || c == '"':
		p.str("")
	default:
		p.pos++
		p.tok = pyTok{kind: tOp, text: string(c)}
	}
}

func isStrPrefix(s string) bool {
	switch strings.ToLower(s) {
	case "r", "u", "b", "br", "rb":
		return true
	}
	return false
}

func (p *pyParser) number() {
	start := p.pos
	digits := func(ok func(rune) bool) {
		// Digits with single underscores between them.
		if p.pos >= len(p.src) || !ok(p.src[p.pos]) {
			p.fail()
		}
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if ok(c) {
				p.pos++
			} else if c == '_' && p.pos+1 < len(p.src) && ok(p.src[p.pos+1]) {
				p.pos++
			} else {
				break
			}
		}
	}
	dec := func(c rune) bool { return c >= '0' && c <= '9' }
	src := p.src
	if src[p.pos] == '0' && p.pos+1 < len(src) && strings.ContainsRune("xXoObB", src[p.pos+1]) {
		base := map[rune]int{'x': 16, 'o': 8, 'b': 2}[unicode.ToLower(src[p.pos+1])]
		p.pos += 2
		if p.pos < len(src) && src[p.pos] == '_' {
			p.pos++
		}
		digits(func(c rune) bool {
			d := strings.IndexRune("0123456789abcdef", unicode.ToLower(c))
			return d >= 0 && d < base
		})
		text := strings.ReplaceAll(string(src[start+2:p.pos]), "_", "")
		n, err := strconv.ParseInt(text, base, 64)
		if err != nil {
			p.fail()
		}
		p.endNumber()
		p.tok = pyTok{kind: tNum, value: n}
		return
	}
	isFloat := false
	if src[p.pos] != '.' {
		digits(dec)
	}
	if p.pos < len(src) && src[p.pos] == '.' {
		isFloat = true
		p.pos++
		if p.pos < len(src) && dec(src[p.pos]) {
			digits(dec)
		}
	}
	if p.pos < len(src) && (src[p.pos] == 'e' || src[p.pos] == 'E') {
		isFloat = true
		p.pos++
		if p.pos < len(src) && (src[p.pos] == '+' || src[p.pos] == '-') {
			p.pos++
		}
		digits(dec)
	}
	text := strings.ReplaceAll(string(src[start:p.pos]), "_", "")
	if p.pos < len(src) && (src[p.pos] == 'j' || src[p.pos] == 'J') {
		p.pos++
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			p.fail()
		}
		p.endNumber()
		p.tok = pyTok{kind: tNum, value: pyComplex{im: f}}
		return
	}
	p.endNumber()
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			p.fail()
		}
		p.tok = pyTok{kind: tNum, value: f}
		return
	}
	// Leading zeros are a syntax error in a non-zero decimal literal.
	if len(text) > 1 && text[0] == '0' && strings.Trim(text, "0") != "" {
		p.fail()
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		p.fail() // beyond int64: kept as the string
	}
	p.tok = pyTok{kind: tNum, value: n}
}

// endNumber rejects a number run into a name ("1x" is a syntax error).
func (p *pyParser) endNumber() {
	if p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) || c == '.' {
			p.fail()
		}
	}
}

func (p *pyParser) str(prefix string) {
	raw := strings.Contains(prefix, "r")
	isBytes := strings.Contains(prefix, "b")
	q := p.src[p.pos]
	triple := p.pos+2 < len(p.src) && p.src[p.pos+1] == q && p.src[p.pos+2] == q
	if triple {
		p.pos += 3
	} else {
		p.pos++
	}
	var b strings.Builder
	for {
		if p.pos >= len(p.src) {
			p.fail()
		}
		c := p.src[p.pos]
		if c == q && (!triple || p.pos+2 < len(p.src) && p.src[p.pos+1] == q && p.src[p.pos+2] == q) {
			if triple {
				p.pos += 3
			} else {
				p.pos++
			}
			break
		}
		if !triple && c == '\n' {
			p.fail()
		}
		if isBytes && c > 127 {
			p.fail()
		}
		if c != '\\' {
			b.WriteRune(c)
			p.pos++
			continue
		}
		if p.pos+1 >= len(p.src) {
			p.fail()
		}
		e := p.src[p.pos+1]
		if raw {
			b.WriteRune('\\')
			b.WriteRune(e)
			p.pos += 2
			continue
		}
		p.pos += 2
		switch e {
		case '\n':
		case '\\', '\'', '"':
			b.WriteRune(e)
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			n := int(e - '0')
			for k := 0; k < 2 && p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '7'; k++ {
				n = n*8 + int(p.src[p.pos]-'0')
				p.pos++
			}
			b.WriteRune(rune(n))
		case 'x', 'u', 'U':
			width := map[rune]int{'x': 2, 'u': 4, 'U': 8}[e]
			if isBytes && e != 'x' {
				b.WriteRune('\\')
				b.WriteRune(e)
				continue
			}
			if p.pos+width > len(p.src) {
				p.fail()
			}
			n, err := strconv.ParseUint(string(p.src[p.pos:p.pos+width]), 16, 32)
			if err != nil || n > unicode.MaxRune {
				p.fail()
			}
			p.pos += width
			b.WriteRune(rune(n))
		default:
			// An unknown escape is kept as written.
			b.WriteRune('\\')
			b.WriteRune(e)
		}
	}
	s := b.String()
	if isBytes && !utf8.ValidString(s) {
		p.fail()
	}
	p.tok = pyTok{kind: tStr, value: s, bytes: isBytes}
}

func (p *pyParser) isOp(s string) bool { return p.tok.kind == tOp && p.tok.text == s }

func (p *pyParser) expect(s string) {
	if !p.isOp(s) {
		p.fail()
	}
	p.next()
}

// expr parses a literal; top allows a bare tuple ("1, 2").
func (p *pyParser) expr(top bool) any {
	v := p.sum()
	if top && p.isOp(",") {
		items := pyTuple{v}
		for p.isOp(",") {
			p.next()
			if p.tok.kind == tEOF {
				break
			}
			items = append(items, p.sum())
		}
		return items
	}
	return v
}

// sum is a signed number plus or minus a complex one (1+2j), else a single
// atom.
func (p *pyParser) sum() any {
	left := p.signed()
	if p.isOp("+") || p.isOp("-") {
		neg := p.isOp("-")
		p.next()
		right, ok := p.atom().(pyComplex)
		if !ok {
			p.fail()
		}
		var re float64
		switch l := left.(type) {
		case int64:
			re = float64(l)
		case float64:
			re = l
		default:
			p.fail()
		}
		if neg {
			right.im = -right.im
		}
		return pyComplex{re: re, im: right.im}
	}
	return left
}

func (p *pyParser) signed() any {
	if p.isOp("+") || p.isOp("-") {
		neg := p.isOp("-")
		p.next()
		if p.tok.kind != tNum {
			p.fail()
		}
		v := p.atom()
		if !neg {
			return v
		}
		switch n := v.(type) {
		case int64:
			return -n
		case float64:
			return -n
		case pyComplex:
			return pyComplex{re: -n.re, im: -n.im}
		}
		p.fail()
	}
	return p.atom()
}

func (p *pyParser) atom() any {
	switch p.tok.kind {
	case tNum:
		v := p.tok.value
		p.next()
		return v
	case tStr:
		s, isBytes := p.tok.value.(string), p.tok.bytes
		p.next()
		// Adjacent literals concatenate (not str with bytes).
		for p.tok.kind == tStr {
			if p.tok.bytes != isBytes {
				p.fail()
			}
			s += p.tok.value.(string)
			p.next()
		}
		return s
	case tName:
		name := p.tok.text
		p.next()
		switch name {
		case "True":
			return true
		case "False":
			return false
		case "None":
			return nil
		case "...":
			return pyEllipsis{}
		case "set":
			p.expect("(")
			p.expect(")")
			return []any{}
		}
		p.fail()
	case tOp:
		switch p.tok.text {
		case "[":
			p.next()
			items := []any{}
			for !p.isOp("]") {
				items = append(items, p.sum())
				if !p.isOp(",") {
					break
				}
				p.next()
			}
			p.expect("]")
			return items
		case "(":
			p.next()
			if p.isOp(")") {
				p.next()
				return pyTuple{}
			}
			first := p.sum()
			if p.isOp(")") {
				p.next()
				return first
			}
			items := pyTuple{first}
			for p.isOp(",") {
				p.next()
				if p.isOp(")") {
					break
				}
				items = append(items, p.sum())
			}
			p.expect(")")
			return items
		case "{":
			p.next()
			if p.isOp("}") {
				p.next()
				return &pyDict{}
			}
			first := p.sum()
			if p.isOp(":") {
				d := &pyDict{}
				p.next()
				d.keys, d.vals = append(d.keys, first), append(d.vals, p.sum())
				for p.isOp(",") {
					p.next()
					if p.isOp("}") {
						break
					}
					k := p.sum()
					p.expect(":")
					d.keys, d.vals = append(d.keys, k), append(d.vals, p.sum())
				}
				p.expect("}")
				for _, k := range d.keys {
					if !hashable(k) {
						p.fail()
					}
				}
				return d
			}
			// A set literal: a list of its unique members.
			items := []any{first}
			for p.isOp(",") {
				p.next()
				if p.isOp("}") {
					break
				}
				items = append(items, p.sum())
			}
			p.expect("}")
			for _, k := range items {
				if !hashable(k) {
					p.fail()
				}
			}
			return items
		}
	}
	p.fail()
	return nil
}

func hashable(v any) bool {
	switch t := v.(type) {
	case []any, *pyDict:
		return false
	case pyTuple:
		for _, e := range t {
			if !hashable(e) {
				return false
			}
		}
	}
	return true
}
