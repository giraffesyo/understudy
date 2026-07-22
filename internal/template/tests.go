package template

import (
	"fmt"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerTests installs the M2 core `is` tests. Task-result tests
// (success/failed/changed/skipped) and version comparison land in M4.
func registerTests(e *Engine) {
	t := e.Tests

	t["defined"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return !isUndefined(in), nil
	}
	t["undefined"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return isUndefined(in), nil
	}
	t["none"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return in == nil, nil
	}
	t["boolean"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		_, ok := in.(bool)
		return ok, nil
	}
	t["true"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return in == true, nil
	}
	t["false"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return in == false, nil
	}
	t["string"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		_, ok := asString(in)
		return ok, nil
	}
	t["number"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if _, ok := in.(bool); ok {
			return true, nil // Python: bool is a number
		}
		return isNumber(in), nil
	}
	t["integer"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		switch in.(type) {
		case int64, int:
			return true, nil
		}
		return false, nil
	}
	t["float"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		_, ok := in.(float64)
		return ok, nil
	}
	t["mapping"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		switch in.(type) {
		case map[string]any, Mapping:
			return true, nil
		}
		return false, nil
	}
	t["sequence"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		// Jinja's sequence test: anything with __getitem__+__len__;
		// strings and dicts count.
		switch in.(type) {
		case []any, string, yaml.UnsafeString, map[string]any, Mapping:
			return true, nil
		}
		return false, nil
	}
	t["iterable"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		_, err := iterate(in)
		return err == nil, nil
	}
	t["callable"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		switch in.(type) {
		case boundMethod, globalFunc:
			return true, nil
		}
		return false, nil
	}
	t["sameas"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("sameas requires one argument")
		}
		// No object identity for Go values; only nil/bool make sense.
		return in == args[0], nil
	}
	t["divisibleby"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		n, ok := asInt(in)
		if !ok {
			return false, fmt.Errorf("expected an integer, got %s", typeName(in))
		}
		if len(args) != 1 {
			return false, fmt.Errorf("divisibleby requires one argument")
		}
		d, ok := asInt(args[0])
		if !ok || d == 0 {
			return false, fmt.Errorf("divisor must be a non-zero integer")
		}
		return n%d == 0, nil
	}
	t["even"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		n, ok := asInt(in)
		if !ok {
			return false, fmt.Errorf("expected an integer, got %s", typeName(in))
		}
		return n%2 == 0, nil
	}
	t["odd"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		n, ok := asInt(in)
		if !ok {
			return false, fmt.Errorf("expected an integer, got %s", typeName(in))
		}
		return n%2 != 0, nil
	}
	t["in"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("'in' test requires one argument")
		}
		return contains(in, args[0])
	}
	t["truthy"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return truthy(in), nil
	}
	t["falsy"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return !truthy(in), nil
	}
	// Comparison-operator aliases (used with select/reject in M4, harmless now).
	t["eq"] = cmpTest("==")
	t["equalto"] = cmpTest("==")
	t["ne"] = cmpTest("!=")
	t["lt"] = cmpTest("<")
	t["lessthan"] = cmpTest("<")
	t["le"] = cmpTest("<=")
	t["gt"] = cmpTest(">")
	t["greaterthan"] = cmpTest(">")
	t["ge"] = cmpTest(">=")
}

func cmpTest(op string) TestFunc {
	return func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("comparison test requires one argument")
		}
		return ec.compareOnce(op, in, args[0], 0)
	}
}
