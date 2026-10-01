package template

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

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
	case Markup:
		return "Markup"
	case pyDatetime:
		if fromVar {
			return "_AnsibleTaggedDateTime"
		}
		return "datetime.datetime"
	case pyDate:
		if fromVar {
			return "_AnsibleTaggedDate"
		}
		return "datetime.date"
	case pyTime:
		if fromVar {
			return "_AnsibleTaggedTime"
		}
		return "datetime.time"
	case *pyTZ:
		return "datetime.timezone"
	case pyTimedelta:
		return "datetime.timedelta"
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
// handling another, or is about a value: the plugin's own message ("The
// filter plugin 'ansible.builtin.items2dict' failed.") and its cause's,
// which ansible-core's error display shows as separate events, the cause
// with the value it is about when it has one.
func SplitCause(err error) (head, detail, value string, ok bool) {
	var te *TemplateError
	if errors.As(err, &te) && te.Plugin && te.pluginHead != "" {
		return te.pluginHead, te.pluginDetail, te.pluginValue, true
	}
	return "", "", "", false
}

// errNotIterable is Python's TypeError iterating a non-iterable.
func errNotIterable(v any, fromVar bool) error {
	return fmt.Errorf("'%s' object is not iterable", pyClassName(v, fromVar))
}

// pyRaiseFrom is `raise Error(msg) from cause` for a cause that is a plain
// Python exception: ansible-core words the two as one message.
func pyRaiseFrom(msg, cause string) error {
	return errors.New(strings.TrimRight(msg, ". ") + ": " + cause)
}

// objError is an AnsibleError raised about a value (obj=): ansible-core
// shows it apart from the plugin's failure, at the value's origin
// (unknown here) with the value itself. pre are the messages of the
// errors raised from it, outermost first, which it collapses into.
type objError struct {
	pre        []string
	msg, value string
}

func (e *objError) Error() string {
	out := ""
	for _, p := range append(append([]string{}, e.pre...), e.msg) {
		if out != "" {
			out = strings.TrimRight(out, ". ") + ": "
		}
		out += p
	}
	return out
}

// pyShorten is textwrap.shorten(s, width): whitespace collapsed, and
// when still too wide, the words that fit followed by " [...]".
func pyShorten(s string, width int) string {
	words := strings.Fields(s)
	text := strings.Join(words, " ")
	if len([]rune(text)) <= width {
		return text
	}
	const placeholder = " [...]"
	var line []string
	n := 0
	for _, w := range words {
		l := len([]rune(w))
		if len(line) > 0 {
			l++
		}
		if n+l > width {
			if len(line) == 0 {
				line = append(line, string([]rune(w)[:width]))
				n = width
			}
			break
		}
		line = append(line, w)
		n += l
	}
	for len(line) > 0 {
		if n+len(placeholder) <= width {
			return strings.Join(line, " ") + placeholder
		}
		n -= len([]rune(line[len(line)-1]))
		if len(line) > 1 {
			n--
		}
		line = line[:len(line)-1]
	}
	return strings.TrimLeft(placeholder, " ")
}

// operandError is a binary operator's TypeError naming its operands'
// classes, which depend on whether each was read from a variable (a
// tagged or lazy value): the format has a %s for each.
type operandError struct {
	format string
	a, b   any
}

func newOperandError(format string, a, b any) *operandError {
	return &operandError{format: format, a: a, b: b}
}

func (e *operandError) Error() string { return e.text(false, false) }

func (e *operandError) text(aVar, bVar bool) string {
	return fmt.Sprintf(e.format, pyClassName(e.a, aVar), pyClassName(e.b, bVar))
}

// operandText is err's message, with an operand error's classes named
// for operands read from variables (aVar, bVar).
func operandText(err error, aVar, bVar bool) string {
	var oe *operandError
	if errors.As(err, &oe) {
		return oe.text(aVar, bVar)
	}
	return err.Error()
}
