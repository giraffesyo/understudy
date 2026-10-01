package template

// ansible-core runs a template or expression only from a trusted source:
// text written in a playbook, role, inventory or vars file. A value a
// template computes is not trusted (nor a module's result); one passed
// through variables unchanged ("{{ x }}", "{{ d.k }}") keeps the trust
// of where it was written. A conditional whose template yields text to
// evaluate, and debug's var= given a template, refuse untrusted text.

// UntrustedHelp is the help shown with an UntrustedError.
const UntrustedHelp = "Templates and expressions must be defined by trusted sources such as playbooks or roles, not untrusted sources such as module results."

// UntrustedError is ansible-core's TemplateTrustCheckFailedError: text
// from an untrusted source used as a template or expression, at that
// text's origin (zero: unknown).
type UntrustedError struct{ Pos Position }

func (e *UntrustedError) Error() string { return "Encountered untrusted template or expression." }

// TrustSource is a VarGetter that knows which of its variables hold
// untrusted values (a module's result, a gathered fact).
type TrustSource interface {
	UntrustedVar(name string) bool
}

// TemplateTrust reports whether the text the template src (written at
// pos) renders comes from a trusted source, and else where that text
// came from (zero: unknown).
func (e *Engine) TemplateTrust(src string, vars VarGetter, pos Position) (bool, Position) {
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src}
	return ec.trustOf(OriginRef{Raw: src, HasRaw: true, Pos: pos}, 0)
}

// RefTrust reports whether the value the raw value ref holds templates
// to is trusted.
func (e *Engine) RefTrust(ref OriginRef, vars VarGetter) bool {
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: ref.Pos}
	trusted, _ := ec.trustOf(ref, 0)
	return trusted
}

func (ec *EvalCtx) trustOf(ref OriginRef, depth int) (bool, Position) {
	if !ref.HasRaw {
		// An item of a file's or option's values, or a value whose text
		// is gone: trusted unless it was computed.
		return !ref.Untrusted, ref.Pos
	}
	s, ok := Undeprecate(ref.Raw).(string)
	if !ok || !HasTemplate(s) {
		return true, Position{}
	}
	if depth > maxOriginDepth {
		return false, ref.Pos
	}
	nodes, err := ec.engine.parseTemplateEscaping(s, ref.Pos, true)
	if err != nil {
		return false, ref.Pos
	}
	single, sets := singleOutput(nodes)
	if single == nil || len(sets) > 0 {
		return false, ref.Pos
	}
	if inner, ok := ec.originOfExpr(single.expr, depth+1); ok {
		trusted, at := ec.trustOf(inner, depth+1)
		if !trusted && at.File == "" {
			// Untrusted text of no origin (a module's result) is where
			// this template passed it along.
			at = ref.Pos
		}
		return trusted, at
	}
	if varsLookup(single.expr) {
		// The vars lookup gives the variable's value as it is.
		return true, Position{}
	}
	root, isRef := refRoot(single.expr)
	if !isRef {
		return false, ref.Pos
	}
	if ts, ok := ec.vars.(TrustSource); ok && ts.UntrustedVar(root) {
		return false, Position{}
	}
	return true, Position{}
}

// refRoot is the variable a reference expression (a name, its
// attributes and items, a default) reads.
func refRoot(e Expr) (string, bool) {
	switch t := e.(type) {
	case *nameExpr:
		return t.name, true
	case *getAttrExpr:
		return refRoot(t.x)
	case *getItemExpr:
		return refRoot(t.x)
	case *filterExpr:
		if (t.name == "default" || t.name == "d") && builtinPlugin(t.full) {
			return refRoot(t.x)
		}
	}
	return "", false
}

// varsLookup reports a call of the vars lookup (lookup('vars', ...),
// query or q), which passes variables' values along.
func varsLookup(e Expr) bool {
	c, ok := e.(*callExpr)
	if !ok || len(c.args) == 0 {
		return false
	}
	fn, ok := c.fn.(*nameExpr)
	if !ok || (fn.name != "lookup" && fn.name != "query" && fn.name != "q") {
		return false
	}
	lit, ok := c.args[0].(*literalExpr)
	if !ok {
		return false
	}
	name, _ := lit.val.(string)
	return name == "vars" || name == "ansible.builtin.vars"
}
