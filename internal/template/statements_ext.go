package template

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Jinja statements beyond if/for/set: macros and call blocks, block-form
// set, filter and with blocks, and template composition (include, import,
// from-import, extends/block).

type macroNode struct {
	name     string
	params   []string
	defaults map[string]Expr
	body     []tmplNode
}

type callBlockNode struct {
	call   *callExpr
	params []string
	body   []tmplNode
}

type setBlockNode struct {
	name   string
	filter Expr // filter chain over blockPlaceholder, or nil
	body   []tmplNode
}

type filterBlockNode struct {
	filter Expr
	body   []tmplNode
}

type withNode struct {
	names []string
	vals  []Expr
	body  []tmplNode
}

type includeNode struct {
	names         Expr
	ignoreMissing bool
	withContext   bool
}

type importNode struct {
	name        Expr
	alias       string            // import 'x' as alias
	names       map[string]string // from 'x' import a as b
	order       []string
	withContext bool
}

type extendsNode struct{ name Expr }

type blockNode struct {
	name string
	body []tmplNode
}

func (macroNode) tmplNode()       {}
func (callBlockNode) tmplNode()   {}
func (setBlockNode) tmplNode()    {}
func (filterBlockNode) tmplNode() {}
func (withNode) tmplNode()        {}
func (includeNode) tmplNode()     {}
func (importNode) tmplNode()      {}
func (extendsNode) tmplNode()     {}
func (blockNode) tmplNode()       {}

// blockPlaceholder names the rendered body inside filter/set blocks.
const blockPlaceholder = "\x00block"

func (p *parser) endTag(name string) error {
	tag, err := p.blockTag(name)
	if err != nil {
		return err
	}
	if tag == "" {
		return p.errf("missing '{%% %s %%}'", name)
	}
	return nil
}

// parseParamList parses "(a, b=1, c='x')" for macro and call definitions.
func (p *parser) parseParamList() ([]string, map[string]Expr, error) {
	defaults := map[string]Expr{}
	var names []string
	if p.kind() != tokLParen {
		return nil, defaults, nil
	}
	p.next()
	for p.kind() != tokRParen {
		t, err := p.expect(tokName)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, t.val)
		if p.kind() == tokAssign {
			p.next()
			def, err := p.parseExpression()
			if err != nil {
				return nil, nil, err
			}
			defaults[t.val] = def
		}
		if p.kind() == tokComma {
			p.next()
			continue
		}
		if p.kind() != tokRParen {
			return nil, nil, p.errf("expected ',' or ')' in parameter list")
		}
	}
	p.next()
	return names, defaults, nil
}

func (p *parser) parseMacro() (tmplNode, error) {
	t, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	params, defaults, err := p.parseParamList()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("endmacro")
	if err != nil {
		return nil, err
	}
	if err := p.endTag("endmacro"); err != nil {
		return nil, err
	}
	return &macroNode{name: t.val, params: params, defaults: defaults, body: body}, nil
}

func (p *parser) parseCallBlock() (tmplNode, error) {
	params, _, err := p.parseParamList()
	if err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	call, ok := expr.(*callExpr)
	if !ok {
		return nil, p.errf("expected a macro call in '{%% call %%}'")
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("endcall")
	if err != nil {
		return nil, err
	}
	if err := p.endTag("endcall"); err != nil {
		return nil, err
	}
	return &callBlockNode{call: call, params: params, body: body}, nil
}

// parseFilterChain parses "name(args) | other" applied to the block body.
func (p *parser) parseFilterChain() (Expr, error) {
	placeholder := &nameExpr{name: blockPlaceholder}
	// parseFilterExpr expects to sit on the '|' token; synthesize the
	// first pipe by parsing a filter name directly.
	chain, err := p.parseFilterExpr(placeholder)
	if err != nil {
		return nil, err
	}
	return chain, nil
}

func (p *parser) parseFilterBlock() (tmplNode, error) {
	// Rewind so the filter name is preceded by a synthetic pipe: the filter
	// grammar is "x | name(args) | ...".
	p.pos--
	p.tokens[p.pos] = token{kind: tokPipe, off: p.tokens[p.pos].off}
	filter, err := p.parseFilterChain()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("endfilter")
	if err != nil {
		return nil, err
	}
	if err := p.endTag("endfilter"); err != nil {
		return nil, err
	}
	return &filterBlockNode{filter: filter, body: body}, nil
}

func (p *parser) parseSetBlock(name string) (tmplNode, error) {
	node := &setBlockNode{name: name}
	if p.kind() == tokPipe {
		filter, err := p.parseFilterChain()
		if err != nil {
			return nil, err
		}
		node.filter = filter
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("endset")
	if err != nil {
		return nil, err
	}
	if err := p.endTag("endset"); err != nil {
		return nil, err
	}
	node.body = body
	return node, nil
}

func (p *parser) parseWith() (tmplNode, error) {
	node := &withNode{}
	for p.kind() != tokBlockEnd {
		t, err := p.expect(tokName)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokAssign); err != nil {
			return nil, err
		}
		val, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		node.names = append(node.names, t.val)
		node.vals = append(node.vals, val)
		if p.kind() == tokComma {
			p.next()
		}
	}
	p.next()
	body, err := p.parseBody("endwith")
	if err != nil {
		return nil, err
	}
	if err := p.endTag("endwith"); err != nil {
		return nil, err
	}
	node.body = body
	return node, nil
}

// parseContextModifier reads an optional "with context" / "without context".
func (p *parser) parseContextModifier(def bool) bool {
	if (p.isName("with") || p.isName("without")) && p.pos+1 < len(p.tokens) &&
		p.tokens[p.pos+1].kind == tokName && p.tokens[p.pos+1].val == "context" {
		with := p.isName("with")
		p.next()
		p.next()
		return with
	}
	return def
}

func (p *parser) parseInclude() (tmplNode, error) {
	names, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	node := &includeNode{names: names, withContext: true}
	if p.isName("ignore") {
		p.next()
		if !p.acceptName("missing") {
			return nil, p.errf("expected 'missing' after 'ignore'")
		}
		node.ignoreMissing = true
	}
	node.withContext = p.parseContextModifier(true)
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	return node, nil
}

func (p *parser) parseImport() (tmplNode, error) {
	name, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.acceptName("as") {
		return nil, p.errf("expected 'as' in import statement")
	}
	alias, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	node := &importNode{name: name, alias: alias.val}
	node.withContext = p.parseContextModifier(false)
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	return node, nil
}

func (p *parser) parseFromImport() (tmplNode, error) {
	name, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.acceptName("import") {
		return nil, p.errf("expected 'import' in from statement")
	}
	node := &importNode{name: name, names: map[string]string{}}
	for {
		if p.isName("with") || p.isName("without") {
			break
		}
		t, err := p.expect(tokName)
		if err != nil {
			return nil, err
		}
		as := t.val
		if p.acceptName("as") {
			a, err := p.expect(tokName)
			if err != nil {
				return nil, err
			}
			as = a.val
		}
		node.names[as] = t.val
		node.order = append(node.order, as)
		if p.kind() != tokComma {
			break
		}
		p.next()
	}
	node.withContext = p.parseContextModifier(false)
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	return node, nil
}

func (p *parser) parseExtends() (tmplNode, error) {
	name, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	return &extendsNode{name: name}, nil
}

func (p *parser) parseBlockStmt() (tmplNode, error) {
	t, err := p.expect(tokName)
	if err != nil {
		return nil, err
	}
	if p.isName("scoped") || p.isName("required") {
		p.next()
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return nil, err
	}
	body, err := p.parseBody("endblock")
	if err != nil {
		return nil, err
	}
	// "{% endblock name %}" may repeat the name.
	tag, err := p.blockTagNamed("endblock")
	if err != nil {
		return nil, err
	}
	if !tag {
		return nil, p.errf("missing '{%% endblock %%}'")
	}
	return &blockNode{name: t.val, body: body}, nil
}

// blockTagNamed consumes "{% name [ident] %}".
func (p *parser) blockTagNamed(name string) (bool, error) {
	if p.kind() != tokBlockStart {
		return false, nil
	}
	save := p.pos
	p.next()
	if !p.isName(name) {
		p.pos = save
		return false, nil
	}
	p.next()
	if p.kind() == tokName {
		p.next()
	}
	if _, err := p.expect(tokBlockEnd); err != nil {
		return false, err
	}
	return true, nil
}

// ---- execution ----

// macroValue is a callable macro bound to the scope it was defined in.
type macroValue struct {
	node  *macroNode
	scope *EvalCtx
}

func (m *macroValue) call(caller *EvalCtx, args []any, kwargs map[string]any) (any, error) {
	frame := m.scope.child()
	n := m.node
	var varargs []any
	for i, a := range args {
		if i < len(n.params) {
			frame.locals[n.params[i]] = a
		} else {
			varargs = append(varargs, a)
		}
	}
	extra := map[string]any{}
	for k, v := range kwargs {
		if k == "caller" {
			frame.locals["caller"] = v
			continue
		}
		if containsStr(n.params, k) {
			frame.locals[k] = v
		} else {
			extra[k] = v
		}
	}
	for _, name := range n.params {
		if _, ok := frame.locals[name]; ok {
			continue
		}
		if def, ok := n.defaults[name]; ok {
			v, err := frame.eval(def)
			if err != nil {
				return nil, err
			}
			frame.locals[name] = v
		} else {
			frame.locals[name] = Undefined{Name: name}
		}
	}
	if varargs == nil {
		varargs = []any{}
	}
	frame.locals["varargs"] = varargs
	frame.locals["kwargs"] = extra
	var b strings.Builder
	out := &renderOutput{b: &b, pos: frame.pos, src: frame.src}
	if err := frame.execNodes(n.body, out); err != nil {
		return nil, err
	}
	return b.String(), nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// child returns a nested scope: new locals that fall back to ec's.
func (ec *EvalCtx) child() *EvalCtx {
	c := *ec
	c.locals = map[string]any{}
	c.parent = ec
	return &c
}

// renderBody renders nodes in a child scope and returns the text.
func (ec *EvalCtx) renderBody(nodes []tmplNode) (string, *EvalCtx, error) {
	frame := ec.child()
	var b strings.Builder
	out := &renderOutput{b: &b, pos: ec.pos, src: ec.src}
	if err := frame.execNodes(nodes, out); err != nil {
		return "", frame, err
	}
	return b.String(), frame, nil
}

func (ec *EvalCtx) applyBlockFilter(filter Expr, text string) (any, error) {
	if filter == nil {
		return text, nil
	}
	frame := ec.child()
	frame.locals[blockPlaceholder] = text
	return frame.eval(filter)
}

func (ec *EvalCtx) execExt(n tmplNode, out *renderOutput) (bool, error) {
	switch t := n.(type) {
	case *macroNode:
		ec.locals[t.name] = &macroValue{node: t, scope: ec}
	case *callBlockNode:
		body := t.body
		params := t.params
		caller := globalFunc(func(_ *EvalCtx, args []any, kwargs map[string]any) (any, error) {
			frame := ec.child()
			for i, name := range params {
				if i < len(args) {
					frame.locals[name] = args[i]
				} else if v, ok := kwargs[name]; ok {
					frame.locals[name] = v
				}
			}
			var b strings.Builder
			o := &renderOutput{b: &b, pos: ec.pos, src: ec.src}
			if err := frame.execNodes(body, o); err != nil {
				return nil, err
			}
			return b.String(), nil
		})
		call := *t.call
		call.kwargs = append(append([]kwarg{}, t.call.kwargs...), kwarg{name: "caller", val: &literalExpr{val: caller}})
		v, err := ec.eval(&call)
		if err != nil {
			return true, err
		}
		out.writeValue(v)
	case *setBlockNode:
		text, _, err := ec.renderBody(t.body)
		if err != nil {
			return true, err
		}
		v, err := ec.applyBlockFilter(t.filter, text)
		if err != nil {
			return true, err
		}
		ec.locals[t.name] = v
	case *filterBlockNode:
		text, _, err := ec.renderBody(t.body)
		if err != nil {
			return true, err
		}
		v, err := ec.applyBlockFilter(t.filter, text)
		if err != nil {
			return true, err
		}
		out.writeValue(v)
	case *withNode:
		frame := ec.child()
		for i, name := range t.names {
			v, err := ec.eval(t.vals[i])
			if err != nil {
				return true, err
			}
			frame.locals[name] = v
		}
		if err := frame.execNodes(t.body, out); err != nil {
			return true, err
		}
	case *includeNode:
		return true, ec.execInclude(t, out)
	case *importNode:
		return true, ec.execImport(t)
	case *blockNode:
		body := t.body
		if override, ok := ec.blocks[t.name]; ok && len(override) > 0 {
			body = override[0]
		}
		frame := ec.child()
		frame.blockName, frame.blockLevel = t.name, 0
		if err := frame.execNodes(body, out); err != nil {
			return true, err
		}
	case *extendsNode:
		return true, fmt.Errorf("internal error: extends outside template root")
	default:
		return false, nil
	}
	return true, nil
}

// ---- template loading ----

// loadTemplate resolves a template name against the search path (the
// including template's directory first, then the configured path),
// returning its parsed nodes. Like Jinja's environment (and unlike the
// top-level template, whose newline Ansible restores), a single trailing
// newline is dropped.
func (ec *EvalCtx) loadTemplate(name string) ([]tmplNode, string, error) {
	var tried []string
	for _, dir := range ec.searchPath {
		path := name
		if !filepath.IsAbs(name) {
			path = filepath.Join(dir, name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			tried = append(tried, path)
			continue
		}
		src := strings.TrimSuffix(string(data), "\n")
		nodes, err := ec.engine.parseTemplate(src, Position{File: path, Line: 1, Col: 1})
		if err != nil {
			return nil, "", err
		}
		return nodes, path, nil
	}
	return nil, "", &templateNotFound{name: name}
}

type templateNotFound struct{ name string }

func (e *templateNotFound) Error() string { return "template not found: " + e.name }

func (ec *EvalCtx) execInclude(t *includeNode, out *renderOutput) error {
	v, err := ec.eval(t.names)
	if err != nil {
		return err
	}
	var names []string
	switch n := v.(type) {
	case []any:
		for _, x := range n {
			names = append(names, toStr(x))
		}
	default:
		names = []string{toStr(v)}
	}
	for _, name := range names {
		nodes, _, err := ec.loadTemplate(name)
		if _, missing := err.(*templateNotFound); missing {
			continue
		}
		if err != nil {
			return err
		}
		frame := ec.child()
		if !t.withContext {
			frame = ec.isolated()
		}
		return frame.execTemplate(nodes, out)
	}
	if t.ignoreMissing {
		return nil
	}
	return ec.errf(0, "template not found: %s", strings.Join(names, ", "))
}

// isolated is a scope with the template vars but none of the locals.
func (ec *EvalCtx) isolated() *EvalCtx {
	c := *ec
	c.locals = map[string]any{}
	c.parent = nil
	return &c
}

// moduleValue is an imported template's exported names (macros and
// top-level sets).
type moduleValue map[string]any

func (m moduleValue) GetItem(k string) (any, bool) { v, ok := m[k]; return v, ok }
func (m moduleValue) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func (m moduleValue) Len() int { return len(m) }

func (ec *EvalCtx) execImport(t *importNode) error {
	v, err := ec.eval(t.name)
	if err != nil {
		return err
	}
	nodes, _, err := ec.loadTemplate(toStr(v))
	if err != nil {
		return ec.errf(0, "%s", err)
	}
	frame := ec.isolated()
	if t.withContext {
		frame = ec.child()
	}
	var sink strings.Builder
	if err := frame.execTemplate(nodes, &renderOutput{b: &sink, pos: ec.pos}); err != nil {
		return err
	}
	exports := moduleValue{}
	for k, v := range frame.locals {
		if !strings.HasPrefix(k, "_") {
			exports[k] = v
		}
	}
	if t.alias != "" {
		ec.locals[t.alias] = exports
		return nil
	}
	for _, as := range t.order {
		v, ok := exports[t.names[as]]
		if !ok {
			return ec.errf(0, "the template %q does not export the requested name %q", toStr(v), t.names[as])
		}
		ec.locals[as] = v
	}
	return nil
}

// execTemplate runs a template's top level, resolving {% extends %}: the
// child's blocks override the parent's, and super() renders the parent's.
func (ec *EvalCtx) execTemplate(nodes []tmplNode, out *renderOutput) error {
	var parent *extendsNode
	for _, n := range nodes {
		if e, ok := n.(*extendsNode); ok {
			parent = e
			break
		}
	}
	if parent == nil {
		return ec.execNodes(nodes, out)
	}
	// A child template renders only its parent; its own top-level nodes
	// run for their side effects (sets, macros, imports), its blocks
	// register as overrides.
	blocks := map[string][][]tmplNode{}
	for k, v := range ec.blocks {
		blocks[k] = v
	}
	var sink strings.Builder
	for _, n := range nodes {
		switch t := n.(type) {
		case *blockNode:
			blocks[t.name] = append([][]tmplNode{t.body}, blocks[t.name]...)
		case *extendsNode:
		default:
			if err := ec.execNodes([]tmplNode{n}, &renderOutput{b: &sink, pos: out.pos}); err != nil {
				return err
			}
		}
	}
	v, err := ec.eval(parent.name)
	if err != nil {
		return err
	}
	pnodes, _, err := ec.loadTemplate(toStr(v))
	if err != nil {
		return ec.errf(0, "%s", err)
	}
	// Parent blocks sit below the child's overrides.
	for _, n := range pnodes {
		if b, ok := n.(*blockNode); ok {
			blocks[b.name] = append(blocks[b.name], b.body)
		}
	}
	frame := *ec
	frame.blocks = blocks
	return frame.execTemplate(pnodes, out)
}

// superFunc renders the next-outer definition of the current block.
func (ec *EvalCtx) superFunc() globalFunc {
	return func(_ *EvalCtx, _ []any, _ map[string]any) (any, error) {
		chain := ec.blocks[ec.blockName]
		next := ec.blockLevel + 1
		if next >= len(chain) {
			return nil, fmt.Errorf("super(): no parent block %q", ec.blockName)
		}
		frame := ec.child()
		frame.blockLevel = next
		var b strings.Builder
		if err := frame.execNodes(chain[next], &renderOutput{b: &b, pos: ec.pos, src: ec.src}); err != nil {
			return nil, err
		}
		return b.String(), nil
	}
}

// namespaceValue is Jinja's namespace(): a mutable attribute bag that
// survives loop scopes ({% set ns.x = ... %}).
type namespaceValue struct{ attrs map[string]any }

func (n *namespaceValue) GetItem(k string) (any, bool) { v, ok := n.attrs[k]; return v, ok }
func (n *namespaceValue) Keys() []string {
	keys := make([]string, 0, len(n.attrs))
	for k := range n.attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func (n *namespaceValue) Len() int { return len(n.attrs) }

// loopValue is Jinja's `loop` object: attributes, cycle()/changed(), and
// — in a recursive loop — callable as loop(children).
type loopValue struct {
	index   int
	items   []any
	depth   int
	node    *forNode
	ec      *EvalCtx
	changed *loopChanged
}

type loopChanged struct {
	set  bool
	last []any
}

func (l *loopValue) GetItem(k string) (any, bool) {
	n := len(l.items)
	switch k {
	case "index":
		return int64(l.index + 1), true
	case "index0":
		return int64(l.index), true
	case "revindex":
		return int64(n - l.index), true
	case "revindex0":
		return int64(n - l.index - 1), true
	case "first":
		return l.index == 0, true
	case "last":
		return l.index == n-1, true
	case "length":
		return int64(n), true
	case "depth":
		return int64(l.depth + 1), true
	case "depth0":
		return int64(l.depth), true
	case "previtem":
		if l.index == 0 {
			return Undefined{Name: "loop.previtem"}, true
		}
		return l.items[l.index-1], true
	case "nextitem":
		if l.index == n-1 {
			return Undefined{Name: "loop.nextitem"}, true
		}
		return l.items[l.index+1], true
	case "cycle":
		return boundMethod(func(_ *EvalCtx, args []any, _ map[string]any) (any, error) {
			if len(args) == 0 {
				return nil, fmt.Errorf("no items for cycling given")
			}
			return args[l.index%len(args)], nil
		}), true
	case "changed":
		return boundMethod(func(_ *EvalCtx, args []any, _ map[string]any) (any, error) {
			st := l.changed
			if st.set && equalValues(st.last, args) {
				return false, nil
			}
			st.set, st.last = true, args
			return true, nil
		}), true
	}
	return nil, false
}

func (l *loopValue) Keys() []string {
	return []string{"index", "index0", "revindex", "revindex0", "first", "last", "length", "depth", "depth0"}
}
func (l *loopValue) Len() int { return len(l.Keys()) }

// recurse renders the loop body over children one level deeper.
func (l *loopValue) recurse(children any) (any, error) {
	if !l.node.recursive {
		return nil, fmt.Errorf("loop() can only be called in a recursive for loop")
	}
	var b strings.Builder
	out := &renderOutput{b: &b, pos: l.ec.pos, src: l.ec.src}
	if err := l.ec.runLoop(l.node, children, l.depth+1, out); err != nil {
		return nil, err
	}
	return b.String(), nil
}

func equalValues(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if toStr(a[i]) != toStr(b[i]) || typeName(a[i]) != typeName(b[i]) {
			return false
		}
	}
	return true
}
