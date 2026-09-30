package template

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// boundMethod is a builtin method already bound to its receiver (the result
// of evaluating `x.upper`); calling syntax invokes it.
type boundMethod func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error)

// globalFunc is a callable global (range, dict, lookup, query).
type globalFunc func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error)

// kwOrderFunc is a global that needs its keyword arguments in call order.
type kwOrderFunc func(ec *EvalCtx, args []any, kwargs *yaml.OMap) (any, error)

// lookupMethod resolves builtin str/list/dict methods for getattr.
func lookupMethod(x any, name string) (boundMethod, bool) {
	if s, ok := asString(x); ok {
		if m, ok := strMethods[name]; ok {
			return func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
				return m(s, args)
			}, true
		}
		return nil, false
	}
	switch t := x.(type) {
	case map[string]any:
		if m, ok := dictMethods[name]; ok {
			return func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
				out, err := m(sortedKeys(t), t, args)
				return ec.dictMethodReads(name, t, out, err)
			}, true
		}
	case Mapping:
		if m, ok := dictMethods[name]; ok {
			// Preserve the mapping's own key order (Python dict methods do).
			keys := t.Keys()
			mm := mappingToMap(t)
			return func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
				out, err := m(keys, mm, args)
				return ec.dictMethodReads(name, mm, out, err)
			}, true
		}
	case []any:
		if m, ok := listMethods[name]; ok {
			return func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
				return m(t, args)
			}, true
		}
	}
	return nil, false
}

func argStr(args []any, i int, def string) (string, error) {
	if i >= len(args) {
		return def, nil
	}
	s, ok := asString(args[i])
	if !ok {
		return "", fmt.Errorf("argument %d must be a string, got %s", i+1, typeName(args[i]))
	}
	return s, nil
}

func argInt(args []any, i int, def int64) (int64, error) {
	if i >= len(args) {
		return def, nil
	}
	n, ok := asInt(args[i])
	if !ok {
		return 0, fmt.Errorf("argument %d must be an integer, got %s", i+1, typeName(args[i]))
	}
	return n, nil
}

var strMethods = map[string]func(s string, args []any) (any, error){
	"upper":      func(s string, _ []any) (any, error) { return strings.ToUpper(s), nil },
	"lower":      func(s string, _ []any) (any, error) { return strings.ToLower(s), nil },
	"title":      func(s string, _ []any) (any, error) { return pyTitle(s), nil },
	"capitalize": func(s string, _ []any) (any, error) { return pyCapitalize(s), nil },
	"strip": func(s string, args []any) (any, error) {
		cut, err := argStr(args, 0, " \t\n\r\v\f")
		if err != nil {
			return nil, err
		}
		return strings.Trim(s, cut), nil
	},
	"lstrip": func(s string, args []any) (any, error) {
		cut, err := argStr(args, 0, " \t\n\r\v\f")
		if err != nil {
			return nil, err
		}
		return strings.TrimLeft(s, cut), nil
	},
	"rstrip": func(s string, args []any) (any, error) {
		cut, err := argStr(args, 0, " \t\n\r\v\f")
		if err != nil {
			return nil, err
		}
		return strings.TrimRight(s, cut), nil
	},
	"split": func(s string, args []any) (any, error) {
		if len(args) == 0 || args[0] == nil {
			return strsToAnys(strings.Fields(s)), nil
		}
		sep, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		maxSplit, err := argInt(args, 1, -1)
		if err != nil {
			return nil, err
		}
		if maxSplit < 0 {
			return strsToAnys(strings.Split(s, sep)), nil
		}
		return strsToAnys(strings.SplitN(s, sep, int(maxSplit)+1)), nil
	},
	"rsplit": func(s string, args []any) (any, error) {
		sep, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		maxSplit, err := argInt(args, 1, -1)
		if err != nil {
			return nil, err
		}
		if sep == "" {
			return strsToAnys(strings.Fields(s)), nil
		}
		parts := strings.Split(s, sep)
		if maxSplit >= 0 && int64(len(parts)-1) > maxSplit {
			keep := len(parts) - int(maxSplit)
			head := strings.Join(parts[:keep], sep)
			parts = append([]string{head}, parts[keep:]...)
		}
		return strsToAnys(parts), nil
	},
	"splitlines": func(s string, _ []any) (any, error) {
		if s == "" {
			return []any{}, nil
		}
		s = strings.TrimSuffix(s, "\n")
		return strsToAnys(strings.Split(s, "\n")), nil
	},
	"startswith": func(s string, args []any) (any, error) {
		p, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		return strings.HasPrefix(s, p), nil
	},
	"endswith": func(s string, args []any) (any, error) {
		p, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		return strings.HasSuffix(s, p), nil
	},
	"replace": func(s string, args []any) (any, error) {
		old, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		niu, err := argStr(args, 1, "")
		if err != nil {
			return nil, err
		}
		count, err := argInt(args, 2, -1)
		if err != nil {
			return nil, err
		}
		return strings.Replace(s, old, niu, int(count)), nil
	},
	"find": func(s string, args []any) (any, error) {
		sub, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		return int64(strings.Index(s, sub)), nil
	},
	"index": func(s string, args []any) (any, error) {
		sub, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		i := strings.Index(s, sub)
		if i < 0 {
			return nil, fmt.Errorf("substring not found")
		}
		return int64(i), nil
	},
	"count": func(s string, args []any) (any, error) {
		sub, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		return int64(strings.Count(s, sub)), nil
	},
	"join": func(s string, args []any) (any, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("join() takes exactly one argument")
		}
		items, err := iterate(args[0])
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(items))
		for i, item := range items {
			str, ok := asString(item)
			if !ok {
				return nil, fmt.Errorf("sequence item %d: expected str, %s found", i, typeName(item))
			}
			parts[i] = str
		}
		return strings.Join(parts, s), nil
	},
	"zfill": func(s string, args []any) (any, error) {
		w, err := argInt(args, 0, 0)
		if err != nil {
			return nil, err
		}
		for int64(len(s)) < w {
			s = "0" + s
		}
		return s, nil
	},
}

func pyTitle(s string) string {
	prevLetter := false
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			if !prevLetter {
				prevLetter = true
				return []rune(strings.ToUpper(string(r)))[0]
			}
			return []rune(strings.ToLower(string(r)))[0]
		}
		prevLetter = false
		return r
	}, s)
}

func pyCapitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func strsToAnys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// dictMethods receive the receiver's keys in iteration order (sorted for a
// bare Go map, insertion order for an *OMap) plus the value map.
var dictMethods = map[string]func(keys []string, m map[string]any, args []any) (any, error){
	"keys": func(keys []string, _ map[string]any, _ []any) (any, error) {
		return strsToAnys(keys), nil
	},
	"values": func(keys []string, m map[string]any, _ []any) (any, error) {
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = m[k]
		}
		return out, nil
	},
	"items": func(keys []string, m map[string]any, _ []any) (any, error) {
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = []any{k, m[k]}
		}
		return out, nil
	},
	"get": func(_ []string, m map[string]any, args []any) (any, error) {
		key, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		if v, ok := m[key]; ok {
			return v, nil
		}
		if len(args) > 1 {
			return args[1], nil
		}
		return nil, nil
	},
}

// dictMethodReads reports the deprecated values a dict method reads, as
// a lazy mapping's items() and get() retrieve them: items() every value
// (which stay deprecated in its pairs), get() the value it returns.
func (ec *EvalCtx) dictMethodReads(name string, m map[string]any, out any, err error) (any, error) {
	if err != nil {
		return out, err
	}
	switch name {
	case "items":
		for _, k := range sortedKeys(m) {
			ec.readValue(m[k])
		}
	case "get":
		return ec.access(out), nil
	}
	return out, nil
}

var listMethods = map[string]func(l []any, args []any) (any, error){
	"index": func(l []any, args []any) (any, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("index() takes an argument")
		}
		for i, item := range l {
			if equal(item, args[0]) {
				return int64(i), nil
			}
		}
		return nil, fmt.Errorf("%s is not in list", pyRepr(args[0]))
	},
	"count": func(l []any, args []any) (any, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("count() takes an argument")
		}
		n := int64(0)
		for _, item := range l {
			if equal(item, args[0]) {
				n++
			}
		}
		return n, nil
	},
}

// Keep the yaml import referenced even as methods evolve.
var _ = yaml.UnsafeString("")
