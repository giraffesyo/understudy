package template

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerFilters installs the M2 core filter set. The full ~55-filter
// Ansible set lands in M4; these are the ones the walking skeleton and
// early playbooks need.
func registerFilters(e *Engine) {
	f := e.Filters

	f["default"] = filterDefault
	f["d"] = filterDefault

	f["mandatory"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if u, ok := in.(Undefined); ok {
			msg := fmt.Sprintf("mandatory variable %q not defined", u.Name)
			if len(args) > 0 {
				if s, ok := asString(args[0]); ok {
					msg = s
				}
			}
			return nil, fmt.Errorf("%s", msg)
		}
		return in, nil
	}

	f["bool"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		switch t := in.(type) {
		case bool:
			return t, nil
		case string:
			switch strings.ToLower(t) {
			case "yes", "on", "1", "true":
				return true, nil
			}
			return false, nil
		case yaml.UnsafeString:
			return f["bool"](ec, string(t), args, kwargs)
		}
		if n, ok := asFloat(in); ok {
			return n == 1, nil
		}
		return false, nil
	}

	f["int"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		def := int64(0)
		if len(args) > 0 {
			if d, ok := asInt(args[0]); ok {
				def = d
			}
		}
		// Ansible: int(value, default=0, base=10) — base is positional or a
		// kwarg.
		base := int64(10)
		if len(args) > 1 {
			if bi, ok := asInt(args[1]); ok {
				base = bi
			}
		}
		if b, ok := kwargs["base"]; ok {
			if bi, ok := asInt(b); ok {
				base = bi
			}
		}
		switch t := in.(type) {
		case int64:
			return t, nil
		case int:
			return int64(t), nil
		case bool:
			if t {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			return int64(t), nil
		case string, yaml.UnsafeString:
			s, _ := asString(t)
			s = strings.TrimSpace(s)
			// Python's int(s, base) accepts the matching radix prefix.
			s = stripRadixPrefix(s, base)
			if v, err := strconv.ParseInt(s, int(base), 64); err == nil {
				return v, nil
			}
			// Python's int() rejects floats-in-strings, Ansible's filter
			// truncates them.
			if fv, err := strconv.ParseFloat(s, 64); err == nil {
				return int64(fv), nil
			}
			return def, nil
		}
		return def, nil
	}

	f["float"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		def := 0.0
		if len(args) > 0 {
			if d, ok := asFloat(args[0]); ok {
				def = d
			}
		}
		if v, ok := asFloat(in); ok {
			return v, nil
		}
		if s, ok := asString(in); ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return v, nil
			}
		}
		return def, nil
	}

	f["string"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if _, ok := asString(in); ok {
			return in, nil
		}
		return toStr(in), nil
	}

	lengthFilter := func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		n, err := length(in)
		if err != nil {
			return nil, err
		}
		return int64(n), nil
	}
	f["length"] = lengthFilter
	f["count"] = lengthFilter

	f["upper"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "upper")
		if err != nil {
			return nil, err
		}
		return strings.ToUpper(s), nil
	}
	f["lower"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "lower")
		if err != nil {
			return nil, err
		}
		return strings.ToLower(s), nil
	}
	f["capitalize"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "capitalize")
		if err != nil {
			return nil, err
		}
		return pyCapitalize(s), nil
	}
	f["title"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "title")
		if err != nil {
			return nil, err
		}
		return pyTitle(s), nil
	}
	f["trim"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "trim")
		if err != nil {
			return nil, err
		}
		return strings.TrimSpace(s), nil
	}

	f["replace"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "replace")
		if err != nil {
			return nil, err
		}
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
	}

	f["join"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		sep, err := argStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = toStr(item)
		}
		return strings.Join(parts, sep), nil
	}

	f["split"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := requireString(in, "split")
		if err != nil {
			return nil, err
		}
		return strMethods["split"](s, args)
	}

	f["first"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("sequence is empty")
		}
		return items[0], nil
	}
	f["last"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("sequence is empty")
		}
		return items[len(items)-1], nil
	}

	f["list"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return iterate(in)
	}

	f["type_debug"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return pyTypeName(in), nil
	}
}

// stripRadixPrefix removes a 0x/0o/0b prefix when it matches the requested
// base, mirroring Python's int(s, base) which tolerates the prefix.
func stripRadixPrefix(s string, base int64) string {
	if len(s) < 2 || s[0] != '0' {
		return s
	}
	switch base {
	case 16:
		if s[1] == 'x' || s[1] == 'X' {
			return s[2:]
		}
	case 8:
		if s[1] == 'o' || s[1] == 'O' {
			return s[2:]
		}
	case 2:
		if s[1] == 'b' || s[1] == 'B' {
			return s[2:]
		}
	}
	return s
}

// filterDefault implements default/d: replace Undefined (or, with the
// second arg true, any falsy value).
func filterDefault(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	if len(args) == 0 {
		if isUndefined(in) {
			return "", nil
		}
		return in, nil
	}
	useOnFalsy := false
	if len(args) > 1 {
		useOnFalsy = truthy(args[1])
	}
	if isUndefined(in) || (useOnFalsy && !truthy(in)) {
		return args[0], nil
	}
	return in, nil
}

func requireString(in any, filter string) (string, error) {
	s, ok := asString(in)
	if !ok {
		return "", fmt.Errorf("expected a string, got %s", typeName(in))
	}
	return s, nil
}

// pyTypeName matches Ansible's type_debug output (Python type names).
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case yaml.UnsafeString:
		return "AnsibleUnsafeText"
	case []any:
		return "list"
	case map[string]any, Mapping:
		return "dict"
	case Undefined:
		return "AnsibleUndefined"
	}
	return fmt.Sprintf("%T", v)
}
