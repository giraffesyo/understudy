package template

import (
	"math/big"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Markup is markupsafe's Markup: a str marked safe for HTML, which the
// escape, safe, forceescape and tojson filters return. ansible-core
// renders with autoescaping off, so it behaves as a str in templates;
// variable storage does not support it, and converts a template result's
// Markup values to str with a warning.
type Markup string

func isMarkup(v any) bool {
	_, ok := Undeprecate(v).(Markup)
	return ok
}

// htmlEscape is markupsafe's escape of a str.
var htmlEscaper = strings.NewReplacer("&", "&amp;", ">", "&gt;", "<", "&lt;", "'", "&#39;", `"`, "&#34;")

func htmlEscape(s string) string { return htmlEscaper.Replace(s) }

// htmlOf is str(value), or value.__html__() for markup.
func htmlOf(v any) string {
	if s, ok := asString(Undeprecate(v)); ok {
		return s
	}
	return toStr(v)
}

// markupEscape is markupsafe.escape(v): markup as is, anything else
// str() and escaped.
func markupEscape(v any) Markup {
	v = Undeprecate(v)
	if m, ok := v.(Markup); ok {
		return m
	}
	return Markup(htmlEscape(htmlOf(v)))
}

func registerMarkupFilters(e *Engine) {
	f := e.Filters
	escape := func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return markupEscape(in), nil
	}
	f["escape"] = escape
	f["e"] = escape
	// do_forceescape: escaped even when already markup.
	f["forceescape"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return Markup(htmlEscape(htmlOf(in))), nil
	}
	// do_mark_safe: Markup(value).
	f["safe"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return Markup(htmlOf(in)), nil
	}
	// do_tojson(eval_ctx, value, indent=None): json.dumps(value,
	// sort_keys=True, indent=indent) with <, >, & and ' as \u escapes.
	f["tojson"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		e := pyJSONEncoder{sortKeys: true, ensureASCII: true}
		if v, ok := filterArg(args, 0, kwargs, "indent"); ok && Undeprecate(v) != nil {
			n, ok := asInt(Undeprecate(v))
			if !ok {
				if s, isStr := asString(Undeprecate(v)); isStr {
					e.indentStr, e.pretty = s, true
				} else {
					return nil, &pyTypeError{"can't multiply sequence by non-int of type '" + pyClassName(v, false) + "'"}
				}
			} else {
				e.indent, e.pretty = int(max(n, 0)), true
			}
		}
		if err := jsonSerializable(in, ec.fromVar(-1)); err != nil {
			return nil, err
		}
		e.write(sortedForJSON(in), 0)
		return Markup(htmlsafeJSON.Replace(e.b.String())), nil
	}
}

// htmlsafeJSON is htmlsafe_json_dumps' escaping of JSON text.
var htmlsafeJSON = strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`, "'", `\u0027`)

// sortedForJSON is v as json.dumps(sort_keys=True) walks it: ordered
// mappings have their keys sorted (plain maps always are).
func sortedForJSON(v any) any {
	switch t := Undeprecate(v).(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = sortedForJSON(item)
		}
		return out
	case Mapping:
		m := map[string]any{}
		for _, k := range t.Keys() {
			item, _ := t.GetItem(k)
			m[k] = sortedForJSON(item)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, item := range t {
			m[k] = sortedForJSON(item)
		}
		return m
	}
	return v
}

// jsonSerializable is the TypeError json.dumps raises for a value it
// cannot encode.
func jsonSerializable(v any, fromVar bool) error {
	switch t := Undeprecate(v).(type) {
	case nil, bool, int64, int, *big.Int, float64, string, yaml.UnsafeString, Markup:
		return nil
	case []any:
		for _, item := range t {
			if err := jsonSerializable(item, fromVar); err != nil {
				return err
			}
		}
		return nil
	case Mapping:
		for _, k := range t.Keys() {
			item, _ := t.GetItem(k)
			if err := jsonSerializable(item, fromVar); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if err := jsonSerializable(t[k], fromVar); err != nil {
				return err
			}
		}
		return nil
	case *rangeValue:
		return &pyTypeError{"Object of type range is not JSON serializable"}
	}
	if isNumber(v) {
		return nil
	}
	name := pyTypeName(v)
	if _, ok := v.(pyDatetime); ok && fromVar {
		name = "_AnsibleTaggedDateTime" // a variable's datetime carries tags
	}
	return &pyTypeError{"Object of type " + name + " is not JSON serializable"}
}

// markupKeeping is the result of a str method on s that markupsafe's
// Markup wraps (upper, lower, strip, ...): markup stays markup.
func markupKeeping(in any, out string) any {
	if isMarkup(in) {
		return Markup(out)
	}
	return out
}

// markupConcat is a + b where either is markup: the other operand is
// escaped and the result is markup (Markup.__add__, __radd__).
func markupConcat(a, b any) (any, bool) {
	_, am := a.(Markup)
	_, bm := b.(Markup)
	if !am && !bm {
		return nil, false
	}
	if _, ok := asString(a); !ok {
		return nil, false
	}
	if _, ok := asString(b); !ok {
		return nil, false
	}
	return Markup(string(markupEscape(a)) + string(markupEscape(b))), true
}

// percentFormat is format % args (str.__mod__, Markup.__mod__): a single
// mapping argument supplies "%(key)s" conversions. Markup's arguments
// are escaped and its result is markup.
func percentFormat(format any, s string, args []any) (any, error) {
	markup := isMarkup(format)
	var mapping map[string]any
	if len(args) == 1 {
		if m, ok := anyToMap(args[0]); ok {
			mapping = m
		}
	}
	if markup {
		if mapping != nil {
			esc := make(map[string]any, len(mapping))
			for k, v := range mapping {
				esc[k] = markupArg(v)
			}
			mapping = esc
		} else {
			esc := make([]any, len(args))
			for i, v := range args {
				esc[i] = markupArg(v)
			}
			args = esc
		}
	}
	var out string
	var err error
	if mapping != nil {
		out, err = pyPercentFormat(s, nil, mapping)
	} else {
		out, err = pyPercentFormat(s, args, nil)
	}
	if err != nil {
		return nil, err
	}
	if markup {
		return Markup(out), nil
	}
	return out, nil
}

// markupArg is an argument of a Markup operation as markupsafe passes
// it: text (and any other object but a number) escaped.
func markupArg(v any) any {
	switch u := Undeprecate(v).(type) {
	case nil, bool, int64, int, float64:
		return v
	case Markup:
		return u
	}
	if isNumber(v) {
		return v
	}
	return markupEscape(v)
}

// asMarkupOf is s as markup when the value it came from (of) is.
func asMarkupOf(of any, s string) any {
	if isMarkup(of) {
		return Markup(s)
	}
	return s
}
