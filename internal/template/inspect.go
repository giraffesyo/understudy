package template

// Refs lists the plugins a template references, found statically.
type Refs struct {
	Filters []string
	Tests   []string
	Lookups []string // lookup()/query()/q() plugin names given as literals
}

// Inspect parses src without evaluating it and reports the filters, tests
// and literal lookup plugins it references. A parse error (bad syntax, or a
// statement the engine does not implement) is returned as-is. Used by
// tools/argscan to find template gaps across a content tree.
func (e *Engine) Inspect(src string) (Refs, error) {
	nodes, err := e.parseTemplate(src, Position{})
	if err != nil {
		return Refs{}, err
	}
	w := &refWalker{}
	w.nodes(nodes)
	return w.refs, nil
}

// InspectExpr is Inspect for a bare expression (a when: condition).
func (e *Engine) InspectExpr(src string) (Refs, error) {
	return e.Inspect("{{ " + src + " }}")
}

type refWalker struct{ refs Refs }

func (w *refWalker) nodes(ns []tmplNode) {
	for _, n := range ns {
		w.node(n)
	}
}

func (w *refWalker) node(n tmplNode) {
	switch t := n.(type) {
	case outputNode:
		w.expr(t.expr)
	case *ifNode:
		for _, b := range t.branches {
			w.expr(b.cond)
			w.nodes(b.body)
		}
		w.nodes(t.els)
	case *forNode:
		w.expr(t.iter)
		w.expr(t.cond)
		w.nodes(t.body)
		w.nodes(t.els)
	case *setNode:
		w.expr(t.val)
	case *macroNode:
		for _, d := range t.defaults {
			w.expr(d)
		}
		w.nodes(t.body)
	case *callBlockNode:
		w.expr(t.call)
		w.nodes(t.body)
	case *setBlockNode:
		w.expr(t.filter)
		w.nodes(t.body)
	case *filterBlockNode:
		w.expr(t.filter)
		w.nodes(t.body)
	case *withNode:
		for _, v := range t.vals {
			w.expr(v)
		}
		w.nodes(t.body)
	case *includeNode:
		w.expr(t.names)
	case *importNode:
		w.expr(t.name)
	case *extendsNode:
		w.expr(t.name)
	case *blockNode:
		w.nodes(t.body)
	}
}

func (w *refWalker) exprs(es []Expr) {
	for _, e := range es {
		w.expr(e)
	}
}

func (w *refWalker) kwargs(ks []kwarg) {
	for _, k := range ks {
		w.expr(k.val)
	}
}

func (w *refWalker) expr(e Expr) {
	switch t := e.(type) {
	case nil:
	case *getAttrExpr:
		w.expr(t.x)
	case *getItemExpr:
		w.expr(t.x)
		w.expr(t.index)
	case *sliceExpr:
		w.expr(t.x)
		w.expr(t.lo)
		w.expr(t.hi)
		w.expr(t.st)
	case *callExpr:
		if n, ok := t.fn.(*nameExpr); ok && (n.name == "lookup" || n.name == "query" || n.name == "q") && len(t.args) > 0 {
			if l, ok := t.args[0].(*literalExpr); ok {
				if s, ok := l.val.(string); ok {
					w.refs.Lookups = append(w.refs.Lookups, s)
				}
			}
		}
		w.expr(t.fn)
		w.exprs(t.args)
		w.kwargs(t.kwargs)
	case *filterExpr:
		w.refs.Filters = append(w.refs.Filters, t.name)
		w.expr(t.x)
		w.exprs(t.args)
		w.kwargs(t.kwargs)
	case *testExpr:
		w.refs.Tests = append(w.refs.Tests, t.name)
		w.expr(t.x)
		w.exprs(t.args)
	case *binExpr:
		w.expr(t.l)
		w.expr(t.r)
	case *notExpr:
		w.expr(t.x)
	case *negExpr:
		w.expr(t.x)
	case *condExpr:
		w.expr(t.val)
		w.expr(t.cond)
		w.expr(t.els)
	case *listExpr:
		w.exprs(t.items)
	case *dictExpr:
		w.exprs(t.keys)
		w.exprs(t.vals)
	case *compareExpr:
		w.expr(t.first)
		w.exprs(t.rest)
	}
}
