package template

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// methodValue is x.name for a builtin method of x: callable in a
// template, but not a value a template's result may hold.
type methodValue struct {
	call boundMethod
	name string
	recv any
	// fromVar: x was read from a variable (a lazy container or a tagged
	// string, whose classes define some methods in Python).
	fromVar bool
}

// globalValue is a callable global read by its name (lookup, range, ...).
type globalValue struct {
	fn   any // globalFunc or kwOrderFunc
	name string
}

// fakeAddress stands in for the object addresses Python's reprs show,
// which no two runs share.
const fakeAddress = "0x104c0ffee"

// lazyPythonMethods are the methods ansible-core's lazy containers (and
// the tagged classes under them) define in Python: bound, their repr
// names the class and the container's repr; the others are builtins.
var lazyPythonMethods = map[string]map[string]string{
	"dict": {"get": "_AnsibleLazyTemplateDict", "setdefault": "_AnsibleLazyTemplateDict",
		"items": "_AnsibleLazyTemplateDict", "values": "_AnsibleLazyTemplateDict",
		"pop": "_AnsibleLazyTemplateDict", "popitem": "_AnsibleLazyTemplateDict",
		"copy": "_AnsibleTaggedDict"},
	"list": {"pop": "_AnsibleLazyTemplateList", "index": "_AnsibleLazyTemplateList",
		"remove": "_AnsibleLazyTemplateList", "sort": "_AnsibleLazyTemplateList",
		"copy": "_AnsibleTaggedList"},
}

// pyTypeRepr is the Python type name and repr of the method.
func (m *methodValue) pyTypeRepr() (string, string) {
	if pt, ok := m.recv.(PyTyped); ok {
		// hostvars and its hosts: collections.abc.Mapping's methods.
		return "method", fmt.Sprintf("<bound method Mapping.%s of <ansible.vars.hostvars.%s object at %s>>", m.name, pt.PyTypeName(), fakeAddress)
	}
	kind := "dict"
	switch Undeprecate(m.recv).(type) {
	case []any:
		kind = "list"
	case string:
		kind = "str"
	}
	if m.fromVar {
		if class, ok := lazyPythonMethods[kind][m.name]; ok {
			return "method", fmt.Sprintf("<bound method %s.%s of %s>", class, m.name, pyRepr(m.recv))
		}
	}
	return "builtin_function_or_method", fmt.Sprintf("<built-in method %s of %s object at %s>", m.name, pyClassName(m.recv, m.fromVar), fakeAddress)
}

// globalReprs are the Python types and reprs of the callable globals.
var globalReprs = map[string][2]string{
	"range":     {"type", "<class 'range'>"},
	"dict":      {"type", "<class 'dict'>"},
	"namespace": {"type", "<class 'jinja2.utils.Namespace'>"},
	"cycler":    {"type", "<class 'jinja2.utils.Cycler'>"},
	"joiner":    {"type", "<class 'jinja2.utils.Joiner'>"},
	"lookup":    {"function", "<function _lookup at " + fakeAddress + ">"},
	"query":     {"function", "<function _query at " + fakeAddress + ">"},
	"q":         {"function", "<function _query at " + fakeAddress + ">"},
	"lipsum":    {"function", "<function generate_lorem_ipsum at " + fakeAddress + ">"},
	"now":       {"function", "<function now at " + fakeAddress + ">"},
	"undef":     {"function", "<function TemplateEnvironment._undef at " + fakeAddress + ">"},
}

// pyTypeRepr is the Python type name and repr of the global.
func (g *globalValue) pyTypeRepr() (string, string) {
	if r, ok := globalReprs[g.name]; ok {
		return r[0], r[1]
	}
	return "function", fmt.Sprintf("<function %s at %s>", g.name, fakeAddress)
}

// callableRepr is the repr of a callable value; ok is false for others.
func callableRepr(v any) (string, bool) {
	switch t := v.(type) {
	case *methodValue:
		_, r := t.pyTypeRepr()
		return r, true
	case *globalValue:
		_, r := t.pyTypeRepr()
		return r, true
	}
	return "", false
}

// StorageError is a template result holding a value ansible-core cannot
// store in a variable (a method, a function, a class).
type StorageError struct {
	Type, Repr string
	// Expr: the result of a bare expression (debug's var), not a
	// template.
	Expr bool
	// Pos is where the template or expression is.
	Pos Position
}

func (e *StorageError) Error() string {
	return fmt.Sprintf("Type '%s' is unsupported for variable storage.", e.Type)
}

// Rendering is the error that raised it: "Error rendering template." or
// "Error rendering expression.".
func (e *StorageError) Rendering() string {
	if e.Expr {
		return "Error rendering expression."
	}
	return "Error rendering template."
}

// AsStorageError finds a StorageError in err's chain.
func AsStorageError(err error) (*StorageError, bool) {
	var se *StorageError
	ok := errors.As(err, &se)
	return se, ok
}

// checkStorable fails for a result that holds a callable, as
// ansible-core's finalization of a template's result does.
func checkStorable(v any, expr bool, pos Position) error {
	var walk func(v any, depth int) error
	walk = func(v any, depth int) error {
		if depth > 100 {
			return nil
		}
		switch t := Undeprecate(v).(type) {
		case *methodValue:
			typ, r := t.pyTypeRepr()
			return &StorageError{Type: typ, Repr: r, Expr: expr, Pos: pos}
		case *globalValue:
			typ, r := t.pyTypeRepr()
			return &StorageError{Type: typ, Repr: r, Expr: expr, Pos: pos}
		case []any:
			for _, x := range t {
				if err := walk(x, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, k := range sortedKeys(t) {
				if err := walk(t[k], depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v, 0)
}

// variableTemplate is a template that is a variable reference alone: a
// name, then attributes and subscripts.
var variableTemplate = regexp.MustCompile(`^\{\{-?\s*[A-Za-z_][A-Za-z0-9_]*(\s*(\.\s*[A-Za-z_][A-Za-z0-9_]*|\[[^\[\]]*\]))*\s*-?\}\}$`)

// IsVariableTemplate reports whether src is a single variable reference
// ("{{ item }}", "{{ names[0] }}"): its result is a variable's own value,
// which keeps that value's trust, where a value the template computes
// (a literal, a filter's output, text around it) is untrusted.
func IsVariableTemplate(src string) bool {
	return variableTemplate.MatchString(strings.TrimSpace(src))
}

// dropNestedOmit is v without the omit values its containers hold: a
// template's result keeps no omitted item or key (the whole result
// being omit is the caller's to handle).
func dropNestedOmit(v any) any {
	var walk func(v any, depth int) (any, bool)
	walk = func(v any, depth int) (any, bool) {
		if depth > 100 {
			return v, false
		}
		switch t := v.(type) {
		case []any:
			var out []any
			changed := false
			for i, x := range t {
				if _, isOmit := x.(Omit); isOmit {
					if !changed {
						out = append([]any{}, t[:i]...)
						changed = true
					}
					continue
				}
				nx, c := walk(x, depth+1)
				if c && !changed {
					out = append([]any{}, t[:i]...)
					changed = true
				}
				if changed {
					out = append(out, nx)
				}
			}
			if !changed {
				return v, false
			}
			if out == nil {
				out = []any{}
			}
			return out, true
		case map[string]any:
			var out map[string]any
			for k, x := range t {
				if _, isOmit := x.(Omit); isOmit {
					if out == nil {
						out = maps.Clone(t)
					}
					delete(out, k)
					continue
				}
				if nx, c := walk(x, depth+1); c {
					if out == nil {
						out = maps.Clone(t)
					}
					out[k] = nx
				}
			}
			if out == nil {
				return v, false
			}
			return out, true
		case *yaml.OMap:
			changed := false
			out := yaml.NewOMap()
			for _, k := range t.Keys() {
				x := t.Get(k)
				if _, isOmit := x.(Omit); isOmit {
					changed = true
					continue
				}
				nx, c := walk(x, depth+1)
				changed = changed || c
				out.Set(k, nx)
			}
			if !changed {
				return v, false
			}
			return out, true
		}
		return v, false
	}
	out, _ := walk(v, 0)
	return out
}
