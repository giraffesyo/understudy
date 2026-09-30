package template

import "strconv"

// parser implements Jinja's expression grammar with its exact precedence:
//
//	condexpr:  or ('if' or ('else' condexpr)?)?
//	or:        and ('or' and)*
//	and:       not ('and' not)*
//	not:       'not' not | comparison
//	compare:   math1 ((==|!=|<|<=|>|>=|in|'not in') math1)*   [chained]
//	math1:     concat (('+'|'-') concat)*
//	concat:    math2 ('~' math2)*
//	math2:     pow (('*'|'/'|'//'|'%') pow)*
//	pow:       unary ('**' unary)*
//	unary:     ('-'|'+') unary | primary postfix* filters*
//
// The two famous consequences, preserved: tests bind tighter than 'not'
// (`not x is defined` == `not (x is defined)`), and filters bind tighter
// than arithmetic (`a + b|int` == `a + (b|int)`).
type parser struct {
	tokens []token
	pos    int
	src    string
	tplPos Position
}

func (p *parser) peek() token   { return p.tokens[p.pos] }
func (p *parser) next() token   { t := p.tokens[p.pos]; p.pos++; return t }
func (p *parser) kind() tokKind { return p.tokens[p.pos].kind }

// isName reports whether the next token is the given keyword.
func (p *parser) isName(name string) bool {
	t := p.peek()
	return t.kind == tokName && t.val == name
}

func (p *parser) acceptName(name string) bool {
	if p.isName(name) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expect(kind tokKind) (token, error) {
	if p.kind() != kind {
		return token{}, p.errf("expected %s, found %s", kind, p.describe(p.peek()))
	}
	return p.next(), nil
}

func (p *parser) describe(t token) string {
	switch t.kind {
	case tokName, tokInt, tokFloat:
		return "'" + t.val + "'"
	case tokString:
		return "string literal"
	}
	return t.kind.String()
}

func (p *parser) errf(format string, args ...any) error {
	return &TemplateError{Pos: p.tplPos, Msg: sprintf(format, args...), Src: p.src, Off: p.peek().off}
}

func (p *parser) parseExpression() (Expr, error) {
	return p.parseCondExpr()
}

func (p *parser) parseCondExpr() (Expr, error) {
	val, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.isName("if") {
		return val, nil
	}
	off := p.next().off
	cond, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	var els Expr
	if p.acceptName("else") {
		els, err = p.parseCondExpr()
		if err != nil {
			return nil, err
		}
	}
	return &condExpr{off: off, val: val, cond: cond, els: els}, nil
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isName("or") {
		off := p.next().off
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: off, op: tokName, opNm: "or", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.isName("and") {
		off := p.next().off
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: off, op: tokName, opNm: "and", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.isName("not") {
		off := p.next().off
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &notExpr{off: off, x: x}, nil
	}
	return p.parseCompare()
}

func (p *parser) parseCompare() (Expr, error) {
	first, err := p.parseMath1()
	if err != nil {
		return nil, err
	}
	var ops []string
	var rest []Expr
	off := p.peek().off
	for {
		var op string
		switch {
		case p.kind() == tokEq:
			op = "=="
		case p.kind() == tokNe:
			op = "!="
		case p.kind() == tokLt:
			op = "<"
		case p.kind() == tokLe:
			op = "<="
		case p.kind() == tokGt:
			op = ">"
		case p.kind() == tokGe:
			op = ">="
		case p.isName("in"):
			op = "in"
		case p.isName("not") && p.tokens[p.pos+1].kind == tokName && p.tokens[p.pos+1].val == "in":
			p.next() // 'not'
			op = "not in"
		default:
			if ops == nil {
				return first, nil
			}
			return &compareExpr{off: off, first: first, ops: ops, rest: rest}, nil
		}
		p.next()
		right, err := p.parseMath1()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
		rest = append(rest, right)
	}
}

func (p *parser) parseMath1() (Expr, error) {
	left, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for p.kind() == tokAdd || p.kind() == tokSub {
		t := p.next()
		right, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: t.off, op: t.kind, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseConcat() (Expr, error) {
	left, err := p.parseMath2()
	if err != nil {
		return nil, err
	}
	for p.kind() == tokTilde {
		t := p.next()
		right, err := p.parseMath2()
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: t.off, op: tokTilde, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseMath2() (Expr, error) {
	left, err := p.parsePow()
	if err != nil {
		return nil, err
	}
	for p.kind() == tokMul || p.kind() == tokDiv || p.kind() == tokFloorDiv || p.kind() == tokMod {
		t := p.next()
		right, err := p.parsePow()
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: t.off, op: t.kind, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parsePow() (Expr, error) {
	left, err := p.parseUnary(true)
	if err != nil {
		return nil, err
	}
	for p.kind() == tokPow {
		t := p.next()
		right, err := p.parseUnary(true)
		if err != nil {
			return nil, err
		}
		left = &binExpr{off: t.off, op: tokPow, l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseUnary(withFilter bool) (Expr, error) {
	var node Expr
	var err error
	switch p.kind() {
	case tokSub:
		off := p.next().off
		x, err := p.parseUnary(false)
		if err != nil {
			return nil, err
		}
		node = &negExpr{off: off, neg: true, x: x}
	case tokAdd:
		off := p.next().off
		x, err := p.parseUnary(false)
		if err != nil {
			return nil, err
		}
		node = &negExpr{off: off, neg: false, x: x}
	default:
		node, err = p.parsePrimary()
		if err != nil {
			return nil, err
		}
	}
	node, err = p.parsePostfix(node)
	if err != nil {
		return nil, err
	}
	if withFilter {
		return p.parseFilterExpr(node)
	}
	return node, nil
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokName:
		switch t.val {
		case "true", "True":
			p.next()
			return &literalExpr{off: t.off, val: true}, nil
		case "false", "False":
			p.next()
			return &literalExpr{off: t.off, val: false}, nil
		case "none", "None", "null":
			p.next()
			return &literalExpr{off: t.off, val: nil}, nil
		}
		p.next()
		return &nameExpr{off: t.off, name: t.val}, nil
	case tokInt:
		p.next()
		n, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, p.errf("invalid integer literal %q", t.val)
		}
		return &literalExpr{off: t.off, val: n}, nil
	case tokFloat:
		p.next()
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, p.errf("invalid float literal %q", t.val)
		}
		return &literalExpr{off: t.off, val: f}, nil
	case tokString:
		p.next()
		// Adjacent string literals concatenate, like Python.
		s := t.val
		for p.kind() == tokString {
			s += p.next().val
		}
		return &literalExpr{off: t.off, val: s}, nil
	case tokLParen:
		p.next()
		if p.kind() == tokRParen { // () is the empty tuple
			p.next()
			return &listExpr{off: t.off}, nil
		}
		inner, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if p.kind() == tokComma {
			// A tuple literal: (a, b) or (a,). Tuples evaluate as lists,
			// which behave the same for in / iteration / indexing.
			tuple := &listExpr{off: t.off, items: []Expr{inner}}
			for p.kind() == tokComma {
				p.next()
				if p.kind() == tokRParen {
					break
				}
				item, err := p.parseExpression()
				if err != nil {
					return nil, err
				}
				tuple.items = append(tuple.items, item)
			}
			if _, err := p.expect(tokRParen); err != nil {
				return nil, err
			}
			return tuple, nil
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return inner, nil
	case tokLBracket:
		return p.parseList()
	case tokLBrace:
		return p.parseDict()
	}
	return nil, p.errf("unexpected %s while parsing expression", p.describe(t))
}

func (p *parser) parseList() (Expr, error) {
	open, _ := p.expect(tokLBracket)
	node := &listExpr{off: open.off}
	for p.kind() != tokRBracket {
		if len(node.items) > 0 {
			if _, err := p.expect(tokComma); err != nil {
				return nil, err
			}
			if p.kind() == tokRBracket {
				break // trailing comma
			}
		}
		item, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.items = append(node.items, item)
	}
	p.next() // ']'
	return node, nil
}

func (p *parser) parseDict() (Expr, error) {
	open, _ := p.expect(tokLBrace)
	node := &dictExpr{off: open.off}
	for p.kind() != tokRBrace {
		if len(node.keys) > 0 {
			if _, err := p.expect(tokComma); err != nil {
				return nil, err
			}
			if p.kind() == tokRBrace {
				break
			}
		}
		key, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokColon); err != nil {
			return nil, err
		}
		val, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.keys = append(node.keys, key)
		node.vals = append(node.vals, val)
	}
	p.next() // '}'
	return node, nil
}

// parsePostfix handles `.attr`, `[index]`, slices, and calls.
func (p *parser) parsePostfix(node Expr) (Expr, error) {
	for {
		switch p.kind() {
		case tokDot:
			p.next()
			t := p.peek()
			switch t.kind {
			case tokName:
				p.next()
				node = &getAttrExpr{off: t.off, x: node, name: t.val}
			case tokInt:
				// Jinja allows tuple access a.0; harmless to support.
				p.next()
				n, _ := strconv.ParseInt(t.val, 10, 64)
				node = &getItemExpr{off: t.off, x: node, index: &literalExpr{off: t.off, val: n}}
			default:
				return nil, p.errf("expected an attribute name after '.', found %s", p.describe(t))
			}
		case tokLBracket:
			open := p.next()
			var err error
			node, err = p.parseSubscript(node, open.off)
			if err != nil {
				return nil, err
			}
		case tokLParen:
			open := p.next()
			args, kwargs, err := p.parseCallArgs()
			if err != nil {
				return nil, err
			}
			node = &callExpr{off: open.off, fn: node, args: args, kwargs: kwargs}
		default:
			return node, nil
		}
	}
}

// parseSubscript parses a[i], a[i:j], a[i:j:k], a[:j], etc.
func (p *parser) parseSubscript(x Expr, off int) (Expr, error) {
	var parts [3]Expr
	idx := 0
	sliced := false
	for {
		if p.kind() == tokColon {
			p.next()
			sliced = true
			idx++
			if idx > 2 {
				return nil, p.errf("too many colons in subscript")
			}
			continue
		}
		if p.kind() == tokRBracket {
			break
		}
		e, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		parts[idx] = e
		if p.kind() == tokColon || p.kind() == tokRBracket {
			continue
		}
		return nil, p.errf("expected ':' or ']' in subscript, found %s", p.describe(p.peek()))
	}
	p.next() // ']'
	if !sliced {
		if parts[0] == nil {
			return nil, p.errf("empty subscript")
		}
		return &getItemExpr{off: off, x: x, index: parts[0]}, nil
	}
	return &sliceExpr{off: off, x: x, lo: parts[0], hi: parts[1], st: parts[2]}, nil
}

// parseCallArgs parses positional and keyword arguments up to ')'.
func (p *parser) parseCallArgs() ([]Expr, []kwarg, error) {
	var args []Expr
	var kwargs []kwarg
	for p.kind() != tokRParen {
		if len(args)+len(kwargs) > 0 {
			if _, err := p.expect(tokComma); err != nil {
				return nil, nil, err
			}
			if p.kind() == tokRParen {
				break
			}
		}
		// kwarg? name '=' (but not name '==')
		if p.kind() == tokName && p.tokens[p.pos+1].kind == tokAssign {
			name := p.next().val
			p.next() // '='
			val, err := p.parseExpression()
			if err != nil {
				return nil, nil, err
			}
			kwargs = append(kwargs, kwarg{name: name, val: val})
			continue
		}
		if len(kwargs) > 0 {
			return nil, nil, p.errf("positional argument after keyword argument")
		}
		arg, err := p.parseExpression()
		if err != nil {
			return nil, nil, err
		}
		args = append(args, arg)
	}
	p.next() // ')'
	return args, kwargs, nil
}

// parseFilterExpr applies |filter and `is test` chains.
func (p *parser) parseFilterExpr(node Expr) (Expr, error) {
	for {
		switch {
		case p.kind() == tokPipe:
			p.next()
			t, err := p.expect(tokName)
			if err != nil {
				return nil, err
			}
			name := t.val
			// Dotted filter names (ansible.builtin.combine) collapse to the
			// final segment.
			for p.kind() == tokDot {
				p.next()
				seg, err := p.expect(tokName)
				if err != nil {
					return nil, err
				}
				name = seg.val
			}
			var args []Expr
			var kwargs []kwarg
			if p.kind() == tokLParen {
				p.next()
				args, kwargs, err = p.parseCallArgs()
				if err != nil {
					return nil, err
				}
			}
			node = &filterExpr{off: t.off, x: node, name: name, args: args, kwargs: kwargs}
		case p.isName("is"):
			p.next()
			negated := p.acceptName("not")
			t, err := p.expect(tokName)
			if err != nil {
				return nil, err
			}
			name := t.val
			// Dotted test names (ansible.builtin.version) collapse to the
			// final segment, like filters.
			for p.kind() == tokDot {
				p.next()
				seg, err := p.expect(tokName)
				if err != nil {
					return nil, err
				}
				name = seg.val
			}
			var args []Expr
			if p.kind() == tokLParen {
				p.next()
				args, _, err = p.parseCallArgs()
				if err != nil {
					return nil, err
				}
			} else if p.canStartImplicitTestArg() {
				// One unparenthesized argument: `x is divisibleby 3`.
				arg, err := p.parseUnary(false)
				if err != nil {
					return nil, err
				}
				args = []Expr{arg}
			}
			node = &testExpr{off: t.off, x: node, name: name, args: args, negated: negated}
		default:
			return node, nil
		}
	}
}

// canStartImplicitTestArg mirrors Jinja: a test may take one bare argument
// unless the next token is a keyword that continues the expression.
func (p *parser) canStartImplicitTestArg() bool {
	t := p.peek()
	switch t.kind {
	case tokInt, tokFloat, tokString, tokLBracket, tokLBrace:
		return true
	case tokName:
		switch t.val {
		case "else", "or", "and", "not", "is", "in", "if":
			return false
		}
		return true
	}
	return false
}
