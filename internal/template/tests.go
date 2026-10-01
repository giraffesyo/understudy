package template

import (
	"fmt"
	"math"
	"math/big"
	"reflect"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerTests installs the M2 core `is` tests. Task-result tests
// (success/failed/changed/skipped) and version comparison land in M4.
func registerTests(e *Engine) {
	t := e.Tests

	t["defined"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if err := capturedUse(in, ec.pos); err != nil {
			return false, err
		}
		return !isUndefined(in), nil
	}
	t["undefined"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if err := capturedUse(in, ec.pos); err != nil {
			return false, err
		}
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
		case int64, int, *big.Int:
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
		case boundMethod, globalFunc, *methodValue, *globalValue:
			return true, nil
		}
		return false, nil
	}
	t["sameas"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("sameas requires one argument")
		}
		// Containers are the same object when they share their storage;
		// Go values otherwise have no identity, so equal comparable
		// values (nil, booleans) stand in for it.
		if ia, ok := containerOf(in); ok {
			ib, ok := containerOf(args[0])
			return ok && ia == ib, nil
		}
		a, b := reflect.TypeOf(in), reflect.TypeOf(args[0])
		if a != b || (a != nil && !a.Comparable()) {
			return false, nil
		}
		return in == args[0], nil
	}
	t["divisibleby"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("test_divisibleby() missing 1 required positional argument: 'num'")
		}
		// value % num == 0
		if _, isStr := asString(in); isStr {
			return false, fmt.Errorf("not all arguments converted during string formatting")
		}
		m, err := numArith(tokMod, in, args[0])
		if err != nil {
			return false, err
		}
		return equal(m, int64(0)), nil
	}
	// even and odd are value % 2 == 0 and value % 2 == 1.
	t["even"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pyMod2Is(ec, in, 0)
	}
	t["odd"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pyMod2Is(ec, in, 1)
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
		return ec.compareOnce(op, in, args[0], 0, [2]bool{ec.fromVar(-1), ec.fromVar(0)})
	}
}

// pyMod2Is is value % 2 == want in Python: a float's remainder, a
// string's %-formatting.
func pyMod2Is(ec *EvalCtx, in any, want int64) (bool, error) {
	v := Undeprecate(in)
	if s, ok := asString(v); ok {
		_, err := pyPercentFormat(s, []any{int64(2)}, nil)
		return false, err
	}
	switch t := v.(type) {
	case float64:
		m := math.Mod(t, 2)
		if m < 0 {
			m += 2
		}
		return m == float64(want), nil
	case bool, int64, int, *big.Int:
		n, _ := pyIndex(t)
		m := new(big.Int).Mod(n, big.NewInt(2))
		return m.Int64() == want, nil
	}
	return false, fmt.Errorf("unsupported operand type(s) for %%: '%s' and 'int'", pyOperandClass(v, ec.fromVar(-1)))
}
