package template

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Filter errors name values by their Python class, as the exception a
// plugin raises does. A value read from a variable is not the plain
// type ansible-core's plugins see: containers are lazy templating
// wrappers and scalars loaded from YAML carry tags. The engine tracks
// which of a filter's arguments are variable references; values computed
// in the template are plain.

// isVarRef reports whether e reads a variable (a name that is not a
// template local, or an attribute or item of one).
func (ec *EvalCtx) isVarRef(e Expr) bool {
	switch t := e.(type) {
	case *nameExpr:
		for s := ec; s != nil; s = s.parent {
			if _, ok := s.locals[t.name]; ok {
				return false
			}
		}
		if _, ok := ec.engine.Globals[t.name]; ok {
			if ec.vars == nil {
				return false
			}
			if _, isVar := ec.vars.Get(t.name); !isVar {
				return false
			}
		}
		return true
	case *getAttrExpr:
		return ec.isVarRef(t.x)
	case *getItemExpr:
		return ec.isVarRef(t.x)
	}
	return false
}

// fromVar reports whether the filter being called got argument i (-1:
// its input) from a variable.
func (ec *EvalCtx) fromVar(i int) bool {
	i++
	return i >= 0 && i < len(ec.filterVars) && ec.filterVars[i]
}

// pyClassName is the class name of v as a plugin sees it (fromVar: read
// from a variable).
func pyClassName(v any, fromVar bool) string {
	v = Undeprecate(v)
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		if fromVar {
			return "_AnsibleTaggedInt"
		}
		return "int"
	case float64:
		if fromVar {
			return "_AnsibleTaggedFloat"
		}
		return "float"
	case string:
		if _, _, _, ok := yaml.Origin(t); ok || fromVar {
			return "_AnsibleTaggedStr"
		}
		return "str"
	case yaml.UnsafeString:
		if fromVar {
			return "_AnsibleTaggedStr"
		}
		return "str"
	case []any:
		if fromVar {
			return "_AnsibleLazyTemplateList"
		}
		return "list"
	case Undefined:
		return "AnsibleUndefined"
	}
	if _, ok := v.(Mapping); ok || isMap(v) {
		if fromVar {
			return "_AnsibleLazyTemplateDict"
		}
		return "dict"
	}
	return typeName(v)
}

func isMap(v any) bool {
	_, ok := anyToMap(v)
	return ok
}

// pyTypeRepr is repr(type(v)): "<class 'int'>", the tagged and lazy
// classes with their module.
func pyTypeRepr(v any, fromVar bool) string {
	name := pyClassName(v, fromVar)
	switch name {
	case "_AnsibleTaggedInt", "_AnsibleTaggedFloat", "_AnsibleTaggedStr":
		return "<class 'ansible.module_utils._internal._datatag." + name + "'>"
	case "_AnsibleLazyTemplateList", "_AnsibleLazyTemplateDict":
		return "<class 'ansible._internal._templating._lazy_containers." + name + "'>"
	}
	return "<class '" + name + "'>"
}

// pyB64Decode is base64.b64decode(s) (validate=False): characters
// outside the alphabet are discarded, and binascii's errors raised.
func pyB64Decode(s string) ([]byte, error) {
	var data []byte
	pads := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			data = append(data, c)
			pads = 0
		case c == '=':
			// Enough padding for the quad ends the input.
			if q := len(data) % 4; q >= 2 {
				pads++
				if q+pads >= 4 {
					return base64.RawStdEncoding.DecodeString(string(data))
				}
			}
		}
	}
	switch len(data) % 4 {
	case 1:
		return nil, fmt.Errorf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(data))
	case 2, 3:
		return nil, errors.New("Incorrect padding")
	}
	return base64.RawStdEncoding.DecodeString(string(data))
}

// pyTypeError is a Python TypeError.
type pyTypeError struct{ msg string }

func (e *pyTypeError) Error() string { return e.msg }

// pyReal is a math function's float argument (PyFloat_AsDouble): ints,
// floats and bools; anything else is a TypeError.
func pyReal(v any, fromVar bool) (float64, error) {
	v = Undeprecate(v)
	if b, ok := v.(*big.Int); ok {
		return bigToFloat(b)
	}
	if f, ok := asFloat(v); ok {
		return f, nil
	}
	return 0, &pyTypeError{fmt.Sprintf("must be real number, not %s", pyClassName(v, fromVar))}
}

// pyFloat is float(v): numbers, and strings in Python's float syntax.
func pyFloat(v any) (float64, error) {
	if s, ok := asString(v); ok {
		f, ok := pyParseFloat(s)
		if !ok {
			return 0, fmt.Errorf("could not convert string to float: %s", pyStrRepr(s))
		}
		return f, nil
	}
	f, err := pyReal(v, false)
	if err != nil {
		return 0, &pyTypeError{fmt.Sprintf("float() argument must be a string or a real number, not '%s'", pyClassName(v, false))}
	}
	return f, nil
}

// pyMathPow is math.pow: a domain error (a negative base to a fractional
// power, zero to a negative one) and overflow are ValueError and
// OverflowError.
func pyMathPow(x, y float64) (float64, error) {
	r := math.Pow(x, y)
	switch {
	case math.IsNaN(r) && !math.IsNaN(x) && !math.IsNaN(y):
		return 0, errors.New("math domain error")
	case x == 0 && y < 0 && !math.IsInf(y, 0):
		return 0, errors.New("math domain error")
	case math.IsInf(r, 0) && !math.IsInf(x, 0) && !math.IsInf(y, 0):
		return 0, errors.New("math range error")
	}
	return r, nil
}

// isFlattenNull is the flatten filter's null check: element in (None,
// 'None', 'null').
func isFlattenNull(v any) bool {
	v = Undeprecate(v)
	if v == nil {
		return true
	}
	s, ok := asString(v)
	return ok && (s == "None" || s == "null")
}

// handlingError is a plugin's exception raised while it handled another
// (an implicit __context__): ansible-core shows it apart from the plugin
// failure it causes rather than collapsed into it.
type handlingError struct{ msg string }

func (e *handlingError) Error() string { return e.msg }

// whileHandling words a plugin error raised in an except block.
func whileHandling(format string, args ...any) error {
	return &handlingError{msg: fmt.Sprintf(format, args...)}
}

// SplitCause reports a plugin failure whose exception was raised while
// handling another: the plugin's own message ("The filter plugin
// 'ansible.builtin.items2dict' failed.") and its cause's, which
// ansible-core's error display shows as separate events.
func SplitCause(err error) (head, detail string, ok bool) {
	var te *TemplateError
	if errors.As(err, &te) && te.Plugin && te.pluginHead != "" {
		return te.pluginHead, te.pluginDetail, true
	}
	return "", "", false
}

// errNotIterable is Python's TypeError iterating a non-iterable.
func errNotIterable(v any, fromVar bool) error {
	return fmt.Errorf("'%s' object is not iterable", pyClassName(v, fromVar))
}
