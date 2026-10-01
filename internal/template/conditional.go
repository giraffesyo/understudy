package template

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Conditionals (when, until, changed_when, failed_when, assert's that,
// break_when) must have a boolean result, as ansible-core 2.19+'s
// TemplateEngine.evaluate_conditional requires: anything else is a broken
// conditional ("Conditional result (True) was derived from value of type
// 'str' at 'vars.yml:3:8'. Conditionals must have a boolean result."),
// naming where the result's value came from. The ALLOW_BROKEN_CONDITIONALS
// option turns the error into a deprecation warning and the result's
// truthiness.

const (
	brokenHelp    = "Broken conditionals can be temporarily allowed with the `ALLOW_BROKEN_CONDITIONALS` configuration option."
	brokenAllowed = "Broken conditionals are currently allowed because the `ALLOW_BROKEN_CONDITIONALS` configuration option is enabled."
	brokenVersion = "2.23"
)

// BrokenConditionalError is AnsibleBrokenConditionalError: a conditional
// that is not a string, is empty, or has a result that is not a boolean.
type BrokenConditionalError struct {
	Msg  string
	Pos  Position // the conditional's origin
	Help string
}

func (e *BrokenConditionalError) Error() string { return e.Msg }

// nonStringCond marks a conditional that was not a string (when: 1): the
// playbook encodes it as one, with its Python type and truthiness.
const nonStringCond = "\x00nonstr\x00"

// NonStringConditional encodes a conditional written as a value that is
// not a string (nor a boolean): its type and truthiness.
func NonStringConditional(v any) string {
	t := "0"
	if truthy(v) {
		t = "1"
	}
	return nonStringCond + pyTypeName(v) + "\x00" + t
}

// IsNonStringConditional reports whether cond encodes a value that was
// not a string.
func IsNonStringConditional(cond string) bool { return strings.HasPrefix(cond, nonStringCond) }

// ConditionalOrigin is ansible-core's str(Origin): path:line:col,
// omitting what is unknown, a description standing in for a path.
func ConditionalOrigin(p Position) string {
	switch {
	case p.File == "":
		return "<unknown>"
	case p.Line <= 0:
		return p.File
	case p.Col <= 0:
		return fmt.Sprintf("%s:%d", p.File, p.Line)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// EvalConditional evaluates one conditional at pos (its origin) to its
// boolean result.
func (e *Engine) EvalConditional(cond string, vars VarGetter, pos Position) (bool, error) {
	if IsNonStringConditional(cond) {
		parts := strings.Split(strings.TrimPrefix(cond, nonStringCond), "\x00")
		typ, truth := parts[0], len(parts) > 1 && parts[1] == "1"
		if e.AllowBrokenConditionals {
			// The value templated is still not a bool.
			e.brokenResult(truth, typ, ConditionalOrigin(pos), pos)
			return truth, nil
		}
		return false, &BrokenConditionalError{Msg: "Conditional expressions must be strings.", Pos: pos, Help: brokenHelp}
	}
	expr := strings.TrimSpace(cond)
	if expr == "" {
		return e.emptyConditional(pos)
	}
	var result any
	var resultOrigin string
	exprPos := pos
	if isAllTemplate(expr) {
		// Indirection of an expression: the template's string result is
		// evaluated; anything else is the result (deprecated).
		v, origin, originPos, err := e.renderConditionalTemplate(expr, vars, pos)
		if err != nil {
			return false, err
		}
		exprPos = originPos
		if s, isStr := v.(string); isStr {
			if strings.TrimSpace(s) == "" {
				ok, err := e.emptyConditional(pos)
				if err != nil {
					return false, err
				}
				e.deprecate(pos, Deprecated{Msg: "Conditionals should not be surrounded by templating delimiters such as {{ }} or {% %}.", Version: brokenVersion})
				return ok, nil
			}
			expr = strings.TrimSpace(s)
		} else {
			e.deprecate(pos, Deprecated{Msg: "Conditionals should not be surrounded by templating delimiters such as {{ }} or {% %}.", Version: brokenVersion})
			result, resultOrigin = v, origin
			if result == nil {
				resultOrigin = "<unknown>"
			}
			goto check
		}
	}
	{
		v, ec, expression, err := e.evalExpressionAST(expr, vars, pos)
		var ue *UndefinedError
		if err != nil && exprPos != pos && errors.As(err, &ue) {
			// The expression a template made fails where its text came
			// from.
			return false, &IndirectConditionalError{Err: err, Pos: exprPos}
		}
		if err != nil {
			return false, err
		}
		if b, ok := v.(bool); ok {
			return b, nil
		}
		result = v
		resultOrigin = ec.resultOrigin(expression, v, exprPos)
	}
check:
	if b, ok := result.(bool); ok {
		return b, nil
	}
	truth := truthy(result)
	typ := NativeTypeName(result)
	if e.AllowBrokenConditionals {
		e.brokenResult(truth, typ, resultOrigin, pos)
		return truth, nil
	}
	return false, &BrokenConditionalError{Msg: brokenMessage(truth, typ, resultOrigin), Pos: pos, Help: brokenHelp}
}

func brokenMessage(truth bool, typ, origin string) string {
	return fmt.Sprintf("Conditional result (%s) was derived from value of type %s at %s. Conditionals must have a boolean result.",
		pyBool(truth), pyStrRepr(typ), pyStrRepr(origin))
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func (e *Engine) brokenResult(truth bool, typ, origin string, pos Position) {
	e.deprecate(pos, Deprecated{Msg: brokenMessage(truth, typ, origin), Help: brokenAllowed, Version: brokenVersion})
}

func (e *Engine) emptyConditional(pos Position) (bool, error) {
	if e.AllowBrokenConditionals {
		e.deprecate(pos, Deprecated{Msg: "Empty conditional expression was evaluated as True.", Help: brokenAllowed, Version: brokenVersion})
		return true, nil
	}
	return false, &BrokenConditionalError{Msg: "Empty conditional expressions are not allowed.", Pos: pos, Help: brokenHelp}
}

func (e *Engine) deprecate(pos Position, d Deprecated) {
	if e.Deprecation != nil {
		e.Deprecation(pos, d)
	}
}

// renderConditionalTemplate renders a conditional that is all template,
// with where its result's value came from.
func (e *Engine) renderConditionalTemplate(src string, vars VarGetter, pos Position) (any, string, Position, error) {
	v, err := e.RenderTemplate(src, vars, pos)
	if err != nil {
		return nil, "", pos, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src}
	originPos := pos
	if ref, ok := ec.templateRef(OriginRef{Raw: src, HasRaw: true, Pos: pos}, 0); ok && ref.Pos.File != "" {
		originPos = ref.Pos
	}
	return v, ConditionalOrigin(originPos), originPos, nil
}

// IndirectConditionalError is a conditional's expression, made by a
// template, failing at the origin of its text.
type IndirectConditionalError struct {
	Err error
	Pos Position
}

func (e *IndirectConditionalError) Error() string { return e.Err.Error() }
func (e *IndirectConditionalError) Unwrap() error { return e.Err }

// evalExpressionAST evaluates a bare expression, returning its AST too.
func (e *Engine) evalExpressionAST(src string, vars VarGetter, pos Position) (any, *EvalCtx, Expr, error) {
	if err := e.syntaxError(src, pos, true, false); err != nil {
		return nil, nil, nil, err
	}
	toks, err := lex("{{ "+src+" }}", e.Opts.exprOpts(), pos)
	if err != nil {
		return nil, nil, nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	if _, err := p.expect(tokVarStart); err != nil {
		return nil, nil, nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := p.expect(tokVarEnd); err != nil {
		return nil, nil, nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src, own: newOwnership(src)}
	v, err := ec.eval(expr)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := tripMarkers(v, pos); err != nil {
		return nil, nil, nil, err
	}
	return v, ec, expr, nil
}

func isAllTemplate(s string) bool {
	return (strings.HasPrefix(s, "{{") || strings.HasPrefix(s, "{%") || strings.HasPrefix(s, "{#")) &&
		(strings.HasSuffix(s, "}}") || strings.HasSuffix(s, "%}") || strings.HasSuffix(s, "#}"))
}

// OriginRef is a value as a variable holds it before templating, with
// where it came from: following attribute and item lookups through raw
// values finds the origin of what they read, as ansible-core's tagged
// values carry theirs.
type OriginRef struct {
	Raw    any
	HasRaw bool
	Pos    Position // zero: unknown
	// Inherit: items come from Pos too (a JSON file's values, a CLI
	// option's).
	Inherit bool
}

// OriginSource is a VarGetter that knows where its variables' values
// came from.
type OriginSource interface {
	VarOrigin(name string) (OriginRef, bool)
}

// resultOrigin is where a conditional's result value came from: the
// value read through variables, else (a value the expression made) the
// expression; None has none.
func (ec *EvalCtx) resultOrigin(e Expr, v any, pos Position) string {
	if Undeprecate(v) == nil {
		return "<unknown>"
	}
	if ref, ok := ec.originOfExpr(e, 0); ok {
		if ref, ok = ec.templateRef(ref, 0); ok && ref.Pos.File != "" {
			return ConditionalOrigin(ref.Pos)
		}
	}
	if s, ok := Undeprecate(v).(string); ok {
		if file, line, col, ok := yaml.Origin(s); ok {
			return ConditionalOrigin(Position{File: file, Line: line, Col: col})
		}
	}
	return ConditionalOrigin(pos)
}

const maxOriginDepth = 20

// originOfExpr follows an expression reading a variable (or an item of
// one) to the raw value it reads.
func (ec *EvalCtx) originOfExpr(e Expr, depth int) (OriginRef, bool) {
	if depth > maxOriginDepth {
		return OriginRef{}, false
	}
	switch t := e.(type) {
	case *nameExpr:
		for s := ec; s != nil; s = s.parent {
			if _, ok := s.locals[t.name]; ok {
				return OriginRef{}, false
			}
		}
		src, ok := ec.vars.(OriginSource)
		if !ok {
			return OriginRef{}, false
		}
		return src.VarOrigin(t.name)
	case *getAttrExpr:
		ref, ok := ec.originOfExpr(t.x, depth+1)
		if !ok {
			return OriginRef{}, false
		}
		return ec.childRef(ref, t.name, depth)
	case *getItemExpr:
		ref, ok := ec.originOfExpr(t.x, depth+1)
		if !ok {
			return OriginRef{}, false
		}
		idx, err := ec.eval(t.index)
		if err != nil {
			return OriginRef{}, false
		}
		switch k := Undeprecate(idx).(type) {
		case string:
			return ec.childRef(ref, k, depth)
		case int64:
			return ec.childRef(ref, strconv.FormatInt(k, 10), depth)
		}
	case *filterExpr:
		if (t.name == "default" || t.name == "d") && builtinPlugin(t.full) {
			in, err := ec.eval(t.x)
			if err != nil {
				return OriginRef{}, false
			}
			if !isUndefined(in) && (len(t.args) < 2 || truthy(in)) {
				return ec.originOfExpr(t.x, depth+1)
			}
			if len(t.args) > 0 {
				return ec.originOfExpr(t.args[0], depth+1)
			}
		}
	}
	return OriginRef{}, false
}

// templateRef resolves a raw value that is a template: one passing a
// value through ("{{ other }}") has that value's origin, any other
// template's result its own.
func (ec *EvalCtx) templateRef(ref OriginRef, depth int) (OriginRef, bool) {
	s, ok := ref.Raw.(string)
	if !ref.HasRaw || !ok || !HasTemplate(s) || depth > maxOriginDepth {
		return ref, true
	}
	own := OriginRef{Pos: ref.Pos}
	nodes, err := ec.engine.parseTemplateEscaping(s, ref.Pos, true)
	if err != nil {
		return own, true
	}
	single, sets := singleOutput(nodes)
	if single == nil || len(sets) > 0 {
		return own, true
	}
	inner, ok := ec.originOfExpr(single.expr, depth+1)
	if !ok {
		return own, true
	}
	inner, ok = ec.templateRef(inner, depth+1)
	if !ok || inner.Pos.File == "" {
		return own, true
	}
	return inner, true
}

// childRef is the raw item key of ref's value, with its origin.
func (ec *EvalCtx) childRef(ref OriginRef, key string, depth int) (OriginRef, bool) {
	ref, _ = ec.templateRef(ref, depth+1)
	inherited := func() (OriginRef, bool) {
		if ref.Inherit && ref.Pos.File != "" {
			return OriginRef{Pos: ref.Pos, Inherit: true}, true
		}
		return OriginRef{}, false
	}
	if !ref.HasRaw {
		return inherited()
	}
	var child any
	found := false
	switch c := Undeprecate(ref.Raw).(type) {
	case []any:
		if i, err := strconv.Atoi(key); err == nil {
			if i < 0 {
				i += len(c)
			}
			if i >= 0 && i < len(c) {
				child, found, key = c[i], true, strconv.Itoa(i)
			}
		}
	case *yaml.OMap:
		child, found = c.GetItem(key)
	case map[string]any:
		child, found = c[key]
	}
	if !found {
		return inherited()
	}
	out := OriginRef{Raw: child, HasRaw: true, Inherit: ref.Inherit}
	if file, line, col, ok := yaml.ValueOrigin(Undeprecate(child)); ok {
		out.Pos = Position{File: file, Line: line, Col: col}
	} else if file, line, col, ok := yaml.ChildOrigin(Undeprecate(ref.Raw), key); ok {
		out.Pos = Position{File: file, Line: line, Col: col}
	} else if ref.Inherit {
		out.Pos = ref.Pos
	}
	return out, true
}

// IsBrokenConditional reports whether err is a broken conditional.
func IsBrokenConditional(err error) (*BrokenConditionalError, bool) {
	var be *BrokenConditionalError
	ok := errors.As(err, &be)
	return be, ok
}

// ResolveOrigin follows ref (a raw value with its origin) through a
// template passing another value along, in vars: what a variable set
// from it (set_fact) carries.
func (e *Engine) ResolveOrigin(ref OriginRef, vars VarGetter) OriginRef {
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: ref.Pos}
	out, _ := ec.templateRef(ref, 0)
	return out
}
