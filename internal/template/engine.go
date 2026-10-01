package template

import (
	"errors"
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Options mirror Ansible's Templar Jinja environment settings.
type Options struct {
	TrimBlocks          bool // Ansible default: true
	LstripBlocks        bool // Ansible default: false
	KeepTrailingNewline bool // Ansible default: true

	// Delimiters (the template module's *_start_string/*_end_string);
	// empty means Jinja's default.
	BlockStart, BlockEnd       string
	VariableStart, VariableEnd string
	CommentStart, CommentEnd   string

	// NewlineSequence, when set, is Jinja's newline_sequence: newlines in
	// template text are normalized to it (and trailing newlines a file
	// render restores use it).
	NewlineSequence string
}

// delims returns the six delimiters with defaults filled in.
func (o Options) delims() (blockStart, blockEnd, varStart, varEnd, commentStart, commentEnd string) {
	pick := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	return pick(o.BlockStart, "{%"), pick(o.BlockEnd, "%}"), pick(o.VariableStart, "{{"),
		pick(o.VariableEnd, "}}"), pick(o.CommentStart, "{#"), pick(o.CommentEnd, "#}")
}

// exprOpts are the options for lexing a bare expression wrapped in the
// default "{{ }}".
func (o Options) exprOpts() Options {
	o.BlockStart, o.BlockEnd, o.VariableStart, o.VariableEnd, o.CommentStart, o.CommentEnd = "", "", "", "", "", ""
	return o
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

	// Deprecation receives each deprecated value a template reads, with
	// the template's position (nil: no warnings).
	Deprecation func(pos Position, d Deprecated)
	// Verbose receives Display.verbose messages plugins print at a given
	// verbosity (nil: none).
	Verbose func(verbosity int, msg string)
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
	registerAnsibleFilters(e)
	registerRegexFilters(e)
	registerCompatFilters(e)
	registerTests(e)
	registerAnsibleTests(e)
	registerGlobals(e)
	guardRecursion(e)
	return e
}

// NewEvalCtx builds an evaluation context over vars, for callers that
// invoke plugins directly (with_<lookup> loops).
func (e *Engine) NewEvalCtx(vars VarGetter, pos Position) *EvalCtx {
	return &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos}
}

// TemplateError is a template syntax or evaluation error, pointing at both
// the document position and the offending spot in the template string.
type TemplateError struct {
	Pos    Position
	Msg    string
	Src    string
	Off    int
	Syntax bool // a lexer/parser error (else raised while rendering)
	// Expr marks a syntax error in a bare expression (a conditional)
	// rather than a template.
	Expr bool
	// Line is the line of the template the syntax error is on (Jinja's
	// lineno), 0 when unknown.
	Line int
	// Plugin marks an error a filter or test plugin raised: Msg is
	// ansible-core's "The filter plugin '...' failed: ..." chain.
	Plugin bool
	// pluginHead and pluginDetail split a plugin failure whose exception
	// was raised while handling another (see SplitCause).
	pluginHead, pluginDetail string
}

// Cause is the error as ansible-core words a template failure's cause:
// "'x' is undefined", "object of type 'dict' has no attribute 'y'",
// "Syntax error in template: ..." or "Error rendering template: ...".
// ok is false for an error that is not a template error.
func Cause(err error) (msg string, ok bool) {
	var ue *UndefinedError
	if errors.As(err, &ue) {
		if ue.Hint != "" {
			return ue.Hint, true
		}
		return undefinedCause(ue.Name), true
	}
	var te *TemplateError
	if errors.As(err, &te) {
		if te.Syntax {
			head := "Syntax error in template"
			if te.Expr {
				head = "Syntax error in expression"
				if HasTemplate(te.Src) {
					head += ". Template delimiters are not supported in expressions"
				}
			}
			return head + ": " + te.Msg, true
		}
		if te.Plugin {
			return te.Msg, true
		}
		return "Error rendering template: " + te.Msg, true
	}
	var re *RecursionError
	if errors.As(err, &re) {
		return re.Error(), true
	}
	return "", false
}

// FileErrorOrigin is the origin ansible-core gives an error raised
// rendering a template file (Position.WholeFile): the file and the line of
// a syntax error (col is NoColumn), or just the file (line 0) for an
// undefined value or a rendering error. ok is false for other errors, and
// for a plugin's error, which ansible-core attributes to the outermost
// template.
func FileErrorOrigin(err error) (file string, line, col int, ok bool) {
	var te *TemplateError
	if errors.As(err, &te) {
		if !te.Pos.WholeFile || te.Plugin {
			return "", 0, 0, false
		}
		if te.Syntax && te.Line > 0 {
			return te.Pos.File, te.Line, NoColumn, true
		}
		return te.Pos.File, 0, 0, true
	}
	var ue *UndefinedError
	if errors.As(err, &ue) && ue.Pos.WholeFile {
		return ue.Pos.File, 0, 0, true
	}
	return "", 0, 0, false
}

// ConditionalCause words the error of a conditional (when, until,
// changed_when, failed_when, assert's that) that did not evaluate: as
// Cause, an undefined value as "Error while evaluating conditional: ...".
func ConditionalCause(err error) string {
	msg, ok := Cause(err)
	if !ok {
		return err.Error()
	}
	var ue *UndefinedError
	if errors.As(err, &ue) {
		return "Error while evaluating conditional: " + msg
	}
	return msg
}

// undefinedCause words an undefined name as Jinja's undefined error:
// an attribute or index missing on a defined value ("<type> object.attr"
// or "<type> object[i]") names the value's type, anything else the
// undefined variable.
func undefinedCause(name string) string {
	if i := strings.Index(name, " object"); i > 0 && !strings.ContainsAny(name[:i], ".[ ") {
		typ, rest := name[:i], name[i+len(" object"):]
		switch {
		case strings.HasPrefix(rest, "."):
			attr := rest[1:]
			if j := strings.IndexAny(attr, ".["); j >= 0 {
				attr = attr[:j]
			}
			return fmt.Sprintf("object of type '%s' has no attribute '%s'", typ, attr)
		case strings.HasPrefix(rest, "["):
			idx := rest[1:]
			if j := strings.Index(idx, "]"); j >= 0 {
				idx = idx[:j]
			}
			return fmt.Sprintf("object of type '%s' has no attribute %s", typ, idx)
		}
	}
	if j := strings.IndexAny(name, ".["); j > 0 {
		name = name[:j]
	}
	return fmt.Sprintf("'%s' is undefined", name)
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
	parent *EvalCtx       // enclosing scope (loop bodies, macros, with)
	pos    Position
	src    string
	depth  int

	searchPath []string                // template dirs for include/import/extends
	blocks     map[string][][]tmplNode // block overrides, child-most first
	blockName  string                  // block being rendered (for super())
	blockLevel int

	lastDeprecated *Deprecated // the deprecated value access() read last

	own *ownership // what the render may mutate in place (nil: nothing)

	// filterVars marks which of the running filter's input and positional
	// arguments were read from variables (see pyClassName).
	filterVars []bool
	// testKwargs are the running test's keyword arguments.
	testKwargs map[string]any
	// callKwargs names the running filter's keyword arguments, in call
	// order.
	callKwargs []string
	// replaceMarkers keeps undefined items of literals as markers (see
	// EvalExpressionReplacing).
	replaceMarkers bool
}

func (ec *EvalCtx) Engine() *Engine    { return ec.engine }
func (ec *EvalCtx) Vars() VarGetter    { return ec.vars }
func (ec *EvalCtx) Position() Position { return ec.pos }

func (ec *EvalCtx) errf(off int, format string, args ...any) error {
	return &TemplateError{Pos: ec.pos, Msg: sprintf(format, args...), Src: ec.src, Off: off}
}

// lookupName resolves a bare name: locals (innermost scope outward), then
// vars, then globals.
func (ec *EvalCtx) lookupName(name string) (any, bool) {
	for s := ec; s != nil; s = s.parent {
		if v, ok := s.locals[name]; ok {
			return ec.own.resolve(v), true
		}
	}
	if name == "super" && ec.blockName != "" {
		return ec.superFunc(), true
	}
	if ec.vars != nil {
		if tg, ok := ec.vars.(TaggedGetter); ok {
			if v, ok := tg.GetTagged(name); ok {
				return ec.own.variable(name, ec.access(v)), true
			}
		} else if v, ok := ec.vars.Get(name); ok {
			return ec.own.variable(name, ec.access(v)), true
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
	if err := e.syntaxError(src, pos, false, true); err != nil {
		return nil, err
	}
	nodes, err := e.parseTemplate(src, pos)
	if err != nil {
		return nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src, own: newOwnership(src)}

	// Native-types rule: exactly one output expression and nothing that
	// renders text. {% set %} nodes are allowed before it — they only bind
	// locals — so `{% set x = [1] %}{{ x }}` returns a native list, matching
	// Ansible's native-Jinja behavior.
	single, sets := singleOutput(nodes)
	if single != nil {
		for _, s := range sets {
			v, err := ec.eval(s.val)
			if err != nil && len(s.names) == 0 {
				v, err = captureSetError(s.val, err)
			}
			if err != nil {
				return nil, err
			}
			if err := ec.assignSet(s, v); err != nil {
				return nil, err
			}
		}
		v, err := ec.eval(single.expr)
		if err != nil {
			return nil, err
		}
		if err := tripMarkers(v, pos); err != nil {
			return nil, err
		}
		v = ec.own.settle(v)
		if HasCycle(v) {
			return nil, &RecursionError{In: "template"}
		}
		return ec.finalize(v), nil
	}

	// Otherwise native Jinja concatenates the output chunks: none is
	// None, a single {{ }} value (inside if/for blocks too) stays native.
	var b strings.Builder
	out := &renderOutput{b: &b, pos: pos, src: src, native: &nativeChunks{}}
	if err := ec.execTemplate(nodes, out); err != nil {
		return nil, err
	}
	switch {
	case out.native.n == 0:
		return nil, nil
	case out.native.n == 1 && out.native.isValue:
		return out.native.first, nil
	}
	return b.String(), nil
}

// WithOptions returns a copy of the engine rendering with opts (the
// template module's Jinja environment overrides); registries are shared.
func (e *Engine) WithOptions(opts Options) *Engine {
	c := *e
	c.Opts = opts
	return &c
}

// RenderFile renders a template file's content: always text, with
// include/import/extends resolved against searchPath (Ansible's template
// search path: the template's own directory, then role and playbook
// template directories).
func (e *Engine) RenderFile(src string, vars VarGetter, pos Position, searchPath []string) (string, error) {
	pos.WholeFile = true
	if err := e.syntaxError(src, pos, false, false); err != nil {
		return "", err
	}
	// Jinja (keep_trailing_newline=False) drops one trailing newline; Ansible
	// then restores the source's trailing newlines the output lacks.
	nodes, err := e.parseTemplate(strings.TrimSuffix(src, "\n"), pos)
	if err != nil {
		return "", err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src, searchPath: searchPath, own: newOwnership(src)}
	var b strings.Builder
	out := &renderOutput{b: &b, pos: pos, src: src}
	if err := ec.execTemplate(nodes, out); err != nil {
		return "", err
	}
	res := b.String()
	nl := "\n"
	if e.Opts.NewlineSequence != "" {
		nl = e.Opts.NewlineSequence
	}
	if want, have := trailingNewlines(src), trailingNewlines(res); want > have {
		res += strings.Repeat(nl, want-have)
	}
	return res, nil
}

func trailingNewlines(s string) int {
	return len(s) - len(strings.TrimRight(s, "\n"))
}

// RenderString renders a template and always returns text — the template
// module's contract (the native-types rule never applies to file content).
func (e *Engine) RenderString(src string, vars VarGetter, pos Position) (string, error) {
	v, err := e.RenderTemplate(src, vars, pos)
	if err != nil {
		return "", err
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	if v == nil {
		return "", nil // finalize: a lone {{ none }} renders empty
	}
	return toStr(v), nil
}

// EvalExpression evaluates src as a bare Jinja expression (when:,
// failed_when:, until: semantics — no {{ }} needed).
func (e *Engine) EvalExpression(src string, vars VarGetter, pos Position) (any, error) {
	v, ec, err := e.evalExpression(src, vars, pos)
	if err != nil {
		return nil, err
	}
	if HasCycle(v) {
		return nil, &RecursionError{In: "expression"}
	}
	return ec.finalize(v), nil
}

// evalExpression evaluates src without finalizing the result.
func (e *Engine) evalExpression(src string, vars VarGetter, pos Position) (any, *EvalCtx, error) {
	if err := e.syntaxError(src, pos, true, false); err != nil {
		return nil, nil, err
	}
	// The spaces matter: "{{-" would otherwise read as a whitespace-control
	// marker and eat a leading minus sign.
	toks, err := lex("{{ "+src+" }}", e.Opts.exprOpts(), pos)
	if err != nil {
		return nil, nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	if _, err := p.expect(tokVarStart); err != nil {
		return nil, nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, nil, err
	}
	if _, err := p.expect(tokVarEnd); err != nil {
		return nil, nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src, own: newOwnership(src)}
	v, err := ec.eval(expr)
	if err != nil {
		return nil, nil, err
	}
	if err := tripMarkers(v, pos); err != nil {
		return nil, nil, err
	}
	return v, ec, nil
}

// EvalBool evaluates a bare expression and applies Python truthiness — the
// `when:` contract.
func (e *Engine) EvalBool(src string, vars VarGetter, pos Position) (bool, error) {
	v, _, err := e.evalExpression(src, vars, pos) // truthiness reads no items
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

// syntaxError is the TemplateSyntaxError Jinja raises compiling src (a
// template, or with expression set a bare expression), nil when it
// compiles. escapeBackslashes is ansible-core's escape_backslashes
// option (string literals in {{ }} keep their backslashes).
func (e *Engine) syntaxError(src string, pos Position, expression, escapeBackslashes bool) error {
	je := e.jinjaCheck(src, e.Opts, expression, escapeBackslashes)
	if je == nil {
		return nil
	}
	msg := je.msg
	if je.cause != "" && !strings.HasSuffix(msg, je.cause) {
		msg = strings.TrimRight(msg, ". ") + ": " + je.cause
	}
	return &TemplateError{Pos: pos, Msg: msg, Src: src, Syntax: true, Expr: expression, Line: je.line}
}

// singleOutput reports whether nodes are exactly one output expression plus
// optional preceding {% set %} statements (and nothing else).
func singleOutput(nodes []tmplNode) (*outputNode, []*setNode) {
	var out *outputNode
	var sets []*setNode
	for i := range nodes {
		switch t := nodes[i].(type) {
		case outputNode:
			if out != nil {
				return nil, nil
			}
			out = &t
		case *setNode:
			if out != nil {
				return nil, nil // set after output: order matters, string path
			}
			sets = append(sets, t)
		default:
			return nil, nil
		}
	}
	return out, sets
}

// parseTemplate lexes and parses a template into its node list, including
// {% if %}, {% for %}, and {% set %} statements.
func (e *Engine) parseTemplate(src string, pos Position) ([]tmplNode, error) {
	toks, err := lex(src, e.Opts, pos)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	return p.parseBody()
}

// Sentinel used by the yaml package's UnsafeString: rendering leaves it
// untouched, and RenderTemplate never re-templates its contents. The
// executor relies on this for register results.
var _ = yaml.UnsafeString("")
