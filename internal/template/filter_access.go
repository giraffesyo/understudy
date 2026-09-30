package template

import (
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// A filter warns about the deprecated values it reads, not about every
// one its input holds: ansible-core hands plugins lazy containers that
// report a deprecated value when an item is retrieved, so what warns is
// what the filter's own code touches. filterReads reports those reads for
// a filter's input: nothing for the filters that pass their input on or
// measure it, a mapping's keys (never its values) for the filters that
// iterate, the items or values themselves (not what they hold) for the
// filters that take a mapping apart, the named attribute of each item for
// the attribute filters, and the whole input, deeply, for the rest (the
// serializers and string conversions read everything).
func (ec *EvalCtx) filterReads(name string, in any, args []any, kwargs map[string]any) {
	attr := func(i int) (string, bool) {
		if a, ok := kwargs["attribute"]; ok {
			s, isStr := a.(string)
			return s, isStr
		}
		if i >= 0 && len(args) > i {
			s, isStr := args[i].(string)
			return s, isStr
		}
		return "", false
	}
	switch name {
	case "default", "d", "mandatory", "length", "count", "type_debug", "ternary":
		return
	case "list", "select", "reject", "reverse", "batch", "slice", "shuffle", "random", "zip", "product":
		ec.readItems(in, false)
	case "first":
		ec.readEnd(in, true)
	case "last":
		ec.readEnd(in, false)
	case "join":
		// A mapping joins its keys; a list's items are str()ed.
		if !isMapping(in) {
			walkDeprecated(in, ec.deprecated, false)
		}
	case "dict2items", "dictsort":
		ec.readItems(in, true)
	case "combine":
		if r, _ := kwargs["recursive"].(bool); r {
			walkDeprecated(in, ec.deprecated, false)
			return
		}
		ec.readItems(in, true)
	case "items2dict":
		keyName, valueName := "key", "value"
		if s, ok := kwargs["key_name"].(string); ok {
			keyName = s
		}
		if s, ok := kwargs["value_name"].(string); ok {
			valueName = s
		}
		ec.readAttrs(in, keyName, valueName)
	case "selectattr", "rejectattr", "groupby":
		if a, ok := attr(0); ok {
			ec.readAttrs(in, a)
		} else {
			ec.readItems(in, false)
		}
	case "sort", "unique", "min", "max", "sum":
		if a, ok := attr(-1); ok {
			ec.readAttrs(in, a)
		} else {
			ec.readItems(in, false)
		}
	case "map":
		if a, ok := attr(-1); ok {
			ec.readAttrs(in, a)
			return
		}
		// map(filter, ...) reads each item as that filter does.
		if len(args) == 0 {
			ec.readItems(in, false)
			return
		}
		sub, _ := args[0].(string)
		for _, item := range iterItems(in, false) {
			ec.filterReads(sub, ec.readValue(item), args[1:], kwargs)
		}
	default:
		walkDeprecated(in, ec.deprecated, false)
	}
}

// readValue is reading v itself: a deprecated value warns (not what it
// holds).
func (ec *EvalCtx) readValue(v any) any {
	for {
		d, ok := v.(Deprecated)
		if !ok {
			return v
		}
		ec.deprecated(d)
		v = d.Value
	}
}

// readItems reads a list's items, or a mapping's values (values) or
// nothing (iterating a mapping yields its keys).
func (ec *EvalCtx) readItems(in any, values bool) {
	for _, item := range iterItems(in, values) {
		ec.readValue(item)
	}
}

// readEnd reads a list's first (or last) item.
func (ec *EvalCtx) readEnd(in any, first bool) {
	items := iterItems(in, false)
	switch {
	case len(items) == 0:
	case first:
		ec.readValue(items[0])
	default:
		ec.readValue(items[len(items)-1])
	}
}

// readAttrs reads each item of a list (a mapping yields keys) and the
// named attributes of each: dotted paths, integer parts indexing lists.
func (ec *EvalCtx) readAttrs(in any, attrs ...string) {
	for _, item := range iterItems(in, false) {
		item = ec.readValue(item)
		for _, a := range attrs {
			v := item
			for _, part := range strings.Split(a, ".") {
				var ok bool
				if v, ok = getAttrOrItem(v, part); !ok {
					break
				}
				v = ec.readValue(v)
			}
		}
	}
}

// iterItems is what iterating in yields: a list's items, a mapping's
// keys (nothing to read), or its values when values is set.
func iterItems(in any, values bool) []any {
	switch t := in.(type) {
	case []any:
		return t
	case map[string]any:
		if !values {
			return nil
		}
		out := make([]any, 0, len(t))
		for _, k := range sortedKeys(t) {
			out = append(out, t[k])
		}
		return out
	case *yaml.OMap:
		if !values {
			return nil
		}
		out := make([]any, 0, t.Len())
		for _, k := range t.Keys() {
			out = append(out, t.Get(k))
		}
		return out
	}
	return nil
}

// getAttrOrItem is one step of an attribute path.
func getAttrOrItem(v any, part string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		x, ok := t[part]
		return x, ok
	case *yaml.OMap:
		return t.GetItem(part)
	case []any:
		if i, err := strconv.Atoi(part); err == nil && i >= 0 && i < len(t) {
			return t[i], true
		}
	}
	return nil, false
}

func isMapping(v any) bool {
	switch v.(type) {
	case map[string]any, *yaml.OMap:
		return true
	}
	return false
}
