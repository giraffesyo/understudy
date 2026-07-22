package template

import "fmt"

// Statement AST nodes. A parsed template is a []tmplNode; control-flow
// statements nest child node lists.

type ifNode struct {
	branches []ifBranch // if / elif chain
	els      []tmplNode // else body (nil if absent)
}

type ifBranch struct {
	cond Expr
	body []tmplNode
}

type forNode struct {
	loopVars []string // one name, or several for tuple unpacking
	iter     Expr
	cond     Expr // optional inline `if` clause
	body     []tmplNode
	els      []tmplNode // {% else %}: runs when the iterable is empty
}

type setNode struct {
	name string
	val  Expr
}

func (ifNode) tmplNode()  {}
func (forNode) tmplNode() {}
func (setNode) tmplNode() {}

// parseStatement parses one {% ... %} tag and, for block statements, its
// body through the matching end tag. Called with the parser positioned just
// after tokBlockStart.
func (p *parser) parseStatement() (tmplNode, error) {
	t := p.peek()
	if t.kind != tokName {
		return nil, p.errf("expected a statement keyword after '{%%', found %s", p.describe(t))
	}
	switch t.val {
	case "if":
		p.next()
		return p.parseIf()
	case "for":
		p.next()
		return p.parseFor()
	case "set":
		p.next()
		return p.parseSet()
	case "elif", "else", "endif", "endfor", "endraw":
		return nil, p.errf("unexpected '{%% %s %%}': no matching open block", t.val)
	case "macro", "call", "include", "import", "from", "extends", "block", "filter", "do", "with":
		return nil, p.errf("'{%% %s %%}' is not supported yet", t.val)
	}
	return nil, p.errf("unknown statement '{%% %s %%}'", t.val)
}

// blockTag reads the {% name %} tag at the cursor if it is one of names,
// consuming through %}. Returns "" if the cursor is not a matching tag.
func (p *parser) blockTag(names ...string) (string, error) {
	if p.kind() != tokBlockStart {
		return "", nil
	}
	save := p.pos
	p.next()
	t := p.peek()
	if t.kind != tokName {
		p.pos = save
		return "", nil
	}
	for _, name := range names {
		if t.val == name {
			p.next()
			if name == "else" || name == "endif" || name == "endfor" {
				if _, err := p.expect(tokBlockEnd); err != nil {
					return "", err
				}
			}
			return name, nil
		}
	}
	p.pos = save
	return "", nil
}

// parseBody parses template nodes until one of the terminator tags appears,
// leaving the parser positioned AT the tokBlockStart of the terminator. With
// no terminators, it parses to EOF (the template top level).
func (p *parser) parseBody(terminators ...string) ([]tmplNode, error) {
	var nodes []tmplNode
	for {
		t := p.peek()
		switch t.kind {
		case tokEOF:
			if len(terminators) == 0 {
				return nodes, nil
			}
			return nil, p.errf("missing '{%% %s %%}'", terminators[len(terminators)-1])
		case tokText:
			p.next()
			nodes = append(nodes, textNode{text: t.val})
		case tokVarStart:
			p.next()
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(tokVarEnd); err != nil {
				return nil, err
			}
			nodes = append(nodes, outputNode{expr: expr})
		case tokBlockStart:
			// Terminator?
			save := p.pos
			p.next()
			if kw := p.peek(); kw.kind == tokName {
				for _, term := range terminators {
					if kw.val == term {
						p.pos = save
						return nodes, nil
					}
				}
			}
			p.pos = save
			p.next() // consume tokBlockStart for a nested statement
			stmt, err := p.parseStatement()
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, stmt)
		default:
			return nil, p.errf("unexpected %s in template body", p.describe(t))
		}
	}
}

func (p *parser) parseIf() (tmplNode, error) {
	node := &ifNode{}
	for {
		cond, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokBlockEnd); err != nil {
			return nil, err
		}
		body, err := p.parseBody("elif", "else", "endif")
		if err != nil {
			return nil, err
		}
		node.branches = append(node.branches, ifBranch{cond: cond, body: body})

		tag, err := p.blockTag("elif", "else", "endif")
		if err != nil {
			return nil, err
		}
		switch tag {
		case "elif":
			continue
		case "else":
			els, err := p.parseBody("endif")
			if err != nil {
				return nil, err
			}
			node.els = els
			if tag, err := p.blockTag("endif"); err != nil {
				return nil, err
			} else if tag == "" {
				return nil, p.errf("missing '{%% endif %%}'")
			}
			return node, nil
		case "endif":
			return node, nil
		default:
			return nil, p.errf("internal error: unterminated if")
		}
	}
}

func (p *parser) parseFor() (tmplNode, error) {
	node := &forNode{}
	for {
		t, err := p.expect(tokName)
		if err != nil {
			return nil, err
		}
		node.loopVars = append(node.loopVars, t.val)
		if p.kind() == tokComma {
			p.next()
			continue
		}
		break
	}
	if !p.acceptName("in") {
		return nil, p.errf("expected 'in' in for statement")
	}
	iter, err := p.parseOr() // Jinja: no bare conditional expr here
	if err != nil {
		return nil, err
	}
	node.iter = iter
	if p.isName("if") {
		p.next()
		cond, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.cond = cond
	}
	if p.isName("recursive") {
		return nil, p.errf("'recursive' for loops are not supported yet")
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("else", "endfor")
	if err != nil {
		return nil, err
	}
	node.body = body
	tag, err := p.blockTag("else", "endfor")
	if err != nil {
		return nil, err
	}
	if tag == "else" {
		els, err := p.parseBody("endfor")
		if err != nil {
			return nil, err
		}
		node.els = els
		if tag, err := p.blockTag("endfor"); err != nil {
			return nil, err
		} else if tag == "" {
			return nil, p.errf("missing '{%% endfor %%}'")
		}
	}
	return node, nil
}

func (p *parser) parseSet() (tmplNode, error) {
	t, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	if p.kind() != tokAssign {
		return nil, p.errf("expected '=' in set statement ({%% set x = value %%}); block-form set is not supported yet")
	}
	p.next()
	val, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	return &setNode{name: t.val, val: val}, nil
}

// renderOutput accumulates rendered template text.
type renderOutput struct {
	b   fmtBuilder
	pos Position
	src string
}

type fmtBuilder interface {
	WriteString(s string) (int, error)
}

func (o *renderOutput) writeText(s string) { o.b.WriteString(s) }

func (o *renderOutput) writeValue(v any) { o.b.WriteString(toStr(v)) }

// loopInfo is the `loop` variable inside {% for %}.
type loopInfo struct {
	index, length int
}

func (li loopInfo) toMapping() map[string]any {
	return map[string]any{
		"index":     int64(li.index + 1),
		"index0":    int64(li.index),
		"revindex":  int64(li.length - li.index),
		"revindex0": int64(li.length - li.index - 1),
		"first":     li.index == 0,
		"last":      li.index == li.length-1,
		"length":    int64(li.length),
	}
}

// execNodes renders a node list into the output collector.
func (ec *EvalCtx) execNodes(nodes []tmplNode, out *renderOutput) error {
	for _, n := range nodes {
		switch t := n.(type) {
		case textNode:
			out.writeText(t.text)
		case outputNode:
			v, err := ec.eval(t.expr)
			if err != nil {
				return err
			}
			if u, ok := v.(Undefined); ok {
				return &UndefinedError{Pos: ec.pos, Name: u.Name}
			}
			if _, ok := v.(Omit); ok {
				return &TemplateError{Pos: out.pos, Src: out.src,
					Msg: "'omit' can only be used as the entire value of a module argument"}
			}
			out.writeValue(v)
		case *ifNode:
			if err := ec.execIf(t, out); err != nil {
				return err
			}
		case *forNode:
			if err := ec.execFor(t, out); err != nil {
				return err
			}
		case *setNode:
			v, err := ec.eval(t.val)
			if err != nil {
				return err
			}
			ec.locals[t.name] = v
		default:
			return fmt.Errorf("internal error: unknown template node %T", n)
		}
	}
	return nil
}

func (ec *EvalCtx) execIf(node *ifNode, out *renderOutput) error {
	for _, br := range node.branches {
		cond, err := ec.eval(br.cond)
		if err != nil {
			return err
		}
		if u, ok := cond.(Undefined); ok {
			return &UndefinedError{Pos: ec.pos, Name: u.Name}
		}
		if truthy(cond) {
			return ec.execNodes(br.body, out)
		}
	}
	if node.els != nil {
		return ec.execNodes(node.els, out)
	}
	return nil
}

func (ec *EvalCtx) execFor(node *forNode, out *renderOutput) error {
	iterVal, err := ec.eval(node.iter)
	if err != nil {
		return err
	}
	if u, ok := iterVal.(Undefined); ok {
		return &UndefinedError{Pos: ec.pos, Name: u.Name}
	}
	items, err := iterate(iterVal)
	if err != nil {
		return fmt.Errorf("%s (in for loop)", err)
	}
	// Mapping iteration yields key/value pairs for tuple unpacking.
	if m, isMap := anyToMap(iterVal); isMap && len(node.loopVars) == 2 {
		items = items[:0]
		for _, k := range sortedKeys(m) {
			items = append(items, []any{k, m[k]})
		}
	}

	// Apply the inline `if` filter first so loop.length is correct.
	if node.cond != nil {
		var kept []any
		for _, item := range items {
			if err := ec.bindLoopVars(node.loopVars, item); err != nil {
				return err
			}
			cond, err := ec.eval(node.cond)
			if err != nil {
				return err
			}
			if truthy(cond) {
				kept = append(kept, item)
			}
		}
		items = kept
	}

	if len(items) == 0 {
		if node.els != nil {
			return ec.execNodes(node.els, out)
		}
		return nil
	}

	// Save and restore shadowed locals: Jinja loop vars are scoped.
	saved := map[string]any{}
	savedOK := map[string]bool{}
	shadowed := append([]string{"loop"}, node.loopVars...)
	for _, name := range shadowed {
		saved[name], savedOK[name] = ec.locals[name]
	}
	defer func() {
		for _, name := range shadowed {
			if savedOK[name] {
				ec.locals[name] = saved[name]
			} else {
				delete(ec.locals, name)
			}
		}
	}()

	for i, item := range items {
		ec.locals["loop"] = loopInfo{index: i, length: len(items)}.toMapping()
		if err := ec.bindLoopVars(node.loopVars, item); err != nil {
			return err
		}
		if err := ec.execNodes(node.body, out); err != nil {
			return err
		}
	}
	return nil
}

func (ec *EvalCtx) bindLoopVars(names []string, item any) error {
	if len(names) == 1 {
		ec.locals[names[0]] = item
		return nil
	}
	tuple, ok := item.([]any)
	if !ok || len(tuple) != len(names) {
		return fmt.Errorf("cannot unpack loop item into %d variables", len(names))
	}
	for i, name := range names {
		ec.locals[name] = tuple[i]
	}
	return nil
}
