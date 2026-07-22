package template

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Options mirror Ansible's Templar Jinja environment settings.
type Options struct {
	TrimBlocks          bool // Ansible default: true
	LstripBlocks        bool // Ansible default: false
	KeepTrailingNewline bool // Ansible default: true
}

// DefaultOptions returns Ansible's Templar defaults.
func DefaultOptions() Options {
	return Options{TrimBlocks: true, LstripBlocks: false, KeepTrailingNewline: true}
}

// VarGetter resolves template variable names. The vars layer implements
// this; a plain map works via MapVars.
type VarGetter interface {
	Get(name string) (any, bool)
}

// MapVars adapts a plain map to VarGetter for tests and simple callers.
type MapVars map[string]any

func (m MapVars) Get(name string) (any, bool) {
	v, ok := m[name]
	return v, ok
}

type (
	FilterFunc func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error)
	TestFunc   func(ec *EvalCtx, in any, args []any) (bool, error)
	LookupFunc func(ec *EvalCtx, name string, terms []any, kwargs map[string]any) (any, error)
)

// Engine holds the filter/test/global registries and options. One Engine is
// shared per run; it is immutable during rendering.
type Engine struct {
	Filters map[string]FilterFunc
	Tests   map[string]TestFunc
	Globals map[string]any
	Lookup  LookupFunc // filled in by the executor; nil => lookups error
	Opts    Options
}

// New returns an Engine with the built-in filters, tests, and globals.
func New() *Engine {
	e := &Engine{
		Filters: map[string]FilterFunc{},
		Tests:   map[string]TestFunc{},
		Globals: map[string]any{},
		Opts:    DefaultOptions(),
	}
	registerFilters(e)
	registerTests(e)
	registerGlobals(e)
	return e
}

// TemplateError is a template syntax or evaluation error, pointing at both
// the document position and the offending spot in the template string.
type TemplateError struct {
	Pos Position
	Msg string
	Src string
	Off int
}

func (e *TemplateError) Error() string {
	loc := e.Pos.String()
	snippet := e.Src
	if len(snippet) > 80 {
		lo := max(e.Off-40, 0)
		hi := min(lo+80, len(snippet))
		snippet = snippet[lo:hi]
	}
	if snippet != "" {
		return fmt.Sprintf("%s: %s (in %q)", loc, e.Msg, snippet)
	}
	return fmt.Sprintf("%s: %s", loc, e.Msg)
}

// EvalCtx is the per-render evaluation context.
type EvalCtx struct {
	engine *Engine
	vars   VarGetter
	locals map[string]any // {% set %} and loop variables
	pos    Position
	src    string
	depth  int
}

func (ec *EvalCtx) Engine() *Engine    { return ec.engine }
func (ec *EvalCtx) Position() Position { return ec.pos }

func (ec *EvalCtx) errf(off int, format string, args ...any) error {
	return &TemplateError{Pos: ec.pos, Msg: sprintf(format, args...), Src: ec.src, Off: off}
}

// lookupName resolves a bare name: locals, then vars, then globals.
func (ec *EvalCtx) lookupName(name string) (any, bool) {
	if v, ok := ec.locals[name]; ok {
		return v, true
	}
	if ec.vars != nil {
		if v, ok := ec.vars.Get(name); ok {
			return v, true
		}
	}
	if v, ok := ec.engine.Globals[name]; ok {
		return v, true
	}
	return nil, false
}

// HasTemplate reports whether s contains any template syntax worth parsing.
func HasTemplate(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#")
}

// RenderTemplate renders a string template. If the template is exactly one
// {{ expression }} with no surrounding text, the expression's native value
// is returned (Ansible's native-types rule); otherwise the concatenated
// string is returned. Undefined anywhere in output is an error.
func (e *Engine) RenderTemplate(src string, vars VarGetter, pos Position) (any, error) {
	if !HasTemplate(src) {
		return src, nil
	}
	nodes, err := e.parseTemplate(src, pos)
	if err != nil {
		return nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src}

	// Native-types rule: exactly one output node and nothing else.
	if len(nodes) == 1 {
		if out, ok := nodes[0].(outputNode); ok {
			v, err := ec.eval(out.expr)
			if err != nil {
				return nil, err
			}
			if u, ok := v.(Undefined); ok {
				return nil, &UndefinedError{Pos: pos, Name: u.Name}
			}
			return v, nil
		}
	}

	var b strings.Builder
	for _, n := range nodes {
		switch t := n.(type) {
		case textNode:
			b.WriteString(t.text)
		case outputNode:
			v, err := ec.eval(t.expr)
			if err != nil {
				return nil, err
			}
			if u, ok := v.(Undefined); ok {
				return nil, &UndefinedError{Pos: pos, Name: u.Name}
			}
			if _, ok := v.(Omit); ok {
				// Omit in a mixed template renders as the omit placeholder;
				// the executor treats a pure-Omit value specially instead.
				return nil, &TemplateError{Pos: pos, Msg: "'omit' can only be used as the entire value of a module argument", Src: src}
			}
			b.WriteString(toStr(v))
		}
	}
	return b.String(), nil
}

// EvalExpression evaluates src as a bare Jinja expression (when:,
// failed_when:, until: semantics — no {{ }} needed).
func (e *Engine) EvalExpression(src string, vars VarGetter, pos Position) (any, error) {
	// The spaces matter: "{{-" would otherwise read as a whitespace-control
	// marker and eat a leading minus sign.
	toks, err := lex("{{ "+src+" }}", e.Opts, pos)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	if _, err := p.expect(tokVarStart); err != nil {
		return nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokVarEnd); err != nil {
		return nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src}
	v, err := ec.eval(expr)
	if err != nil {
		return nil, err
	}
	if u, ok := v.(Undefined); ok {
		return nil, &UndefinedError{Pos: pos, Name: u.Name}
	}
	return v, nil
}

// EvalBool evaluates a bare expression and applies Python truthiness — the
// `when:` contract.
func (e *Engine) EvalBool(src string, vars VarGetter, pos Position) (bool, error) {
	v, err := e.EvalExpression(src, vars, pos)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// parseTemplate lexes and parses a template into its node list.
// Statements ({% if %}, {% for %}, ...) land in M4; for now they are a
// clear parse error rather than a silent misrender.
func (e *Engine) parseTemplate(src string, pos Position) ([]tmplNode, error) {
	toks, err := lex(src, e.Opts, pos)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	var nodes []tmplNode
	for {
		t := p.peek()
		switch t.kind {
		case tokEOF:
			return nodes, nil
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
			p.next()
			name := "statement"
			if p.kind() == tokName {
				name = "'{% " + p.peek().val + " %}'"
			}
			return nil, p.errf("template statements are not supported yet (%s); coming in a later milestone", name)
		default:
			return nil, p.errf("unexpected %s at template top level", p.describe(t))
		}
	}
}

// Sentinel used by the yaml package's UnsafeString: rendering leaves it
// untouched, and RenderTemplate never re-templates its contents. The
// executor relies on this for register results.
var _ = yaml.UnsafeString("")
