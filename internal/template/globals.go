package template

import (
	"fmt"
	"strings"
)

// registerGlobals installs callable globals: range, dict, lookup, query.
func registerGlobals(e *Engine) {
	e.Globals["range"] = globalFunc(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
		var start, stop, step int64 = 0, 0, 1
		switch len(args) {
		case 1:
			s, ok := asInt(args[0])
			if !ok {
				return nil, fmt.Errorf("range() arguments must be integers")
			}
			stop = s
		case 2, 3:
			var ok bool
			if start, ok = asInt(args[0]); !ok {
				return nil, fmt.Errorf("range() arguments must be integers")
			}
			if stop, ok = asInt(args[1]); !ok {
				return nil, fmt.Errorf("range() arguments must be integers")
			}
			if len(args) == 3 {
				if step, ok = asInt(args[2]); !ok {
					return nil, fmt.Errorf("range() arguments must be integers")
				}
				if step == 0 {
					return nil, fmt.Errorf("range() step must not be zero")
				}
			}
		default:
			return nil, fmt.Errorf("range() takes 1 to 3 arguments")
		}
		return &rangeValue{start: start, stop: stop, step: step}, nil
	})

	e.Globals["namespace"] = globalFunc(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
		ns := &namespaceValue{attrs: map[string]any{}}
		for _, a := range args {
			if m, ok := anyToMap(a); ok {
				for k, v := range m {
					ns.attrs[k] = v
				}
			}
		}
		for k, v := range kwargs {
			ns.attrs[k] = v
		}
		return ns, nil
	})

	e.Globals["dict"] = globalFunc(func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("dict() only accepts keyword arguments")
		}
		out := make(map[string]any, len(kwargs))
		for k, v := range kwargs {
			out[k] = v
		}
		return out, nil
	})

	mkLookup := func(wantList bool) globalFunc {
		return func(ec *EvalCtx, args []any, kwargs map[string]any) (any, error) {
			if len(args) < 1 {
				return nil, fmt.Errorf("lookup() requires a plugin name")
			}
			name, ok := asString(args[0])
			if !ok {
				return nil, fmt.Errorf("lookup() plugin name must be a string")
			}
			if ec.engine.Lookup == nil {
				return nil, fmt.Errorf("lookup plugin %q is not available in this context", name)
			}
			out, err := ec.engine.Lookup(ec, name, args[1:], kwargs)
			if err != nil {
				return nil, err
			}
			list, isList := out.([]any)
			if !isList {
				list = []any{out}
			}
			if wantList {
				return list, nil
			}
			// lookup(): Ansible joins string results with ",", unwraps a
			// single item, and otherwise returns the list.
			allStrings := len(list) > 0
			parts := make([]string, len(list))
			for i, v := range list {
				s, ok := asString(v)
				if !ok {
					allStrings = false
					break
				}
				parts[i] = s
			}
			switch {
			case allStrings:
				return strings.Join(parts, ","), nil
			case len(list) == 1:
				return list[0], nil
			}
			return list, nil
		}
	}
	e.Globals["lookup"] = mkLookup(false)
	e.Globals["query"] = mkLookup(true)
	e.Globals["q"] = mkLookup(true)

	// The omit sentinel is exposed as a global so it works even before the
	// vars layer injects it per-run.
	e.Globals["omit"] = Omit{}
}
