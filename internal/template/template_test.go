package template

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

var testPos = Position{File: "test.yml", Line: 1, Col: 1}

func render(t *testing.T, src string, vars map[string]any) any {
	t.Helper()
	e := New()
	v, err := e.RenderTemplate(src, MapVars(vars), testPos)
	if err != nil {
		t.Fatalf("RenderTemplate(%q): %v", src, err)
	}
	return v
}

func evalExpr(t *testing.T, src string, vars map[string]any) any {
	t.Helper()
	e := New()
	v, err := e.EvalExpression(src, MapVars(vars), testPos)
	if err != nil {
		t.Fatalf("EvalExpression(%q): %v", src, err)
	}
	return v
}

func expectEq(t *testing.T, got, want any, ctx string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s\n got: %#v\nwant: %#v", ctx, got, want)
	}
}

func TestLiteralsAndArithmetic(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{"1 + 2", int64(3)},
		{"7 - 10", int64(-3)},
		{"3 * 4", int64(12)},
		{"1 / 2", 0.5},         // true division is always float
		{"7 // 2", int64(3)},   // floor division stays int
		{"-7 // 2", int64(-4)}, // Python floors toward -inf
		{"7 % 3", int64(1)},
		{"-7 % 3", int64(2)}, // Python modulo takes divisor's sign
		{"2 ** 10", int64(1024)},
		{"2 ** -1", 0.5},
		{"1.5 + 1", 2.5},
		{"'a' + 'b'", "ab"},
		{"'ab' * 3", "ababab"},
		{"3 * 'ab'", "ababab"},
		{"[1] + [2]", []any{int64(1), int64(2)}},
		{"[0] * 3", []any{int64(0), int64(0), int64(0)}},
		{"1 ~ 2", "12"},
		{"'v' ~ 1.0", "v1.0"},
		{"-5", int64(-5)},
		{"+5", int64(5)},
		{"--5", int64(5)},
		{"(1 + 2) * 3", int64(9)},
		{"1 + 2 * 3", int64(7)},
		{"'a' 'b' 'c'", "abc"}, // adjacent string literal concat
		{"none", nil},
		{"true", true},
		{"True", true},
		{"false", false},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
}

func TestComparisons(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 2", true},
		{"1 == 1.0", true},  // cross-type numeric equality
		{"1 == '1'", false}, // int vs str: false, not error
		{"'a' != 'b'", true},
		{"1 < 2 < 3", true},  // chained
		{"1 < 2 > 3", false}, // chained, mixed
		{"'abc' < 'abd'", true},
		{"[1, 2] == [1, 2]", true},
		{"{'a': 1} == {'a': 1}", true},
		{"2 in [1, 2, 3]", true},
		{"5 not in [1, 2]", true},
		{"'el' in 'hello'", true},
		{"'a' in {'a': 1}", true},
		{"true and true", true},
		{"true and false", false},
		{"false or true", true},
		{"not false", true},
		{"1 and 2", int64(2)}, // Python: returns operand
		{"0 or 'x'", "x"},     // Python: returns operand
		{"'' or 'dflt'", "dflt"},
		{"1 if true else 2", int64(1)},
		{"1 if false else 2", int64(2)},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}

	e := New()
	if _, err := e.EvalExpression("1 < 'a'", nil, testPos); err == nil {
		t.Error("1 < 'a' should error (Python 3 semantics)")
	}
}

func TestPrecedenceQuirks(t *testing.T) {
	vars := map[string]any{"x": int64(5), "b": "7"}
	// Tests bind tighter than not: not (x is defined).
	expectEq(t, evalExpr(t, "not x is defined", vars), false, "not x is defined")
	expectEq(t, evalExpr(t, "not y is defined", vars), true, "not y is defined")
	// Filters bind tighter than arithmetic: a + (b|int).
	expectEq(t, evalExpr(t, "x + b|int", vars), int64(12), "x + b|int")
	// Unary minus with filter: -1|abs is -(1|abs)... we have no abs yet, use int.
	expectEq(t, evalExpr(t, "-3|int", vars), int64(-3), "-3|int")
}

func TestAttrAndSubscript(t *testing.T) {
	vars := map[string]any{
		"d":    map[string]any{"key": "val", "nested": map[string]any{"x": int64(1)}},
		"l":    []any{"a", "b", "c"},
		"s":    "hello",
		"item": map[string]any{"value": map[string]any{"port": int64(80)}},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"d.key", "val"},
		{"d['key']", "val"},
		{"d.nested.x", int64(1)},
		{"item.value.port", int64(80)},
		{"l[0]", "a"},
		{"l[-1]", "c"},
		{"s[1]", "e"},
		{"s[-1]", "o"},
		{"l[1:]", []any{"b", "c"}},
		{"l[:2]", []any{"a", "b"}},
		{"l[::-1]", []any{"c", "b", "a"}},
		{"s[1:4]", "ell"},
		{"s[::-1]", "olleh"},
		{"l.0", "a"}, // Jinja tuple-attr access
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestMethods(t *testing.T) {
	vars := map[string]any{
		"s": "Hello World",
		"d": map[string]any{"b": int64(2), "a": int64(1)},
		"l": []any{"x", "y", "x"},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"s.upper()", "HELLO WORLD"},
		{"s.lower()", "hello world"},
		{"s.split()", []any{"Hello", "World"}},
		{"s.split('o')", []any{"Hell", " W", "rld"}},
		{"s.startswith('Hello')", true},
		{"s.endswith('x')", false},
		{"s.replace('World', 'There')", "Hello There"},
		{"', '.join(['a', 'b'])", "a, b"},
		{"d.keys()", []any{"a", "b"}},
		{"d.values()", []any{int64(1), int64(2)}},
		{"d.items()", []any{[]any{"a", int64(1)}, []any{"b", int64(2)}}},
		{"d.get('a')", int64(1)},
		{"d.get('z', 'dflt')", "dflt"},
		{"d.get('z')", nil},
		{"l.index('y')", int64(1)},
		{"l.count('x')", int64(2)},
		{"'42'.zfill(5)", "00042"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestUndefinedChaining(t *testing.T) {
	e := New()
	vars := MapVars{"d": map[string]any{"present": int64(1)}}

	// Chainable: deep access on undefined does not error under `is defined`.
	for _, expr := range []string{
		"missing is defined",
		"missing.deep.deeper is defined",
		"d.absent is defined",
		"d.absent.deeper is defined",
		"missing['key'] is defined",
	} {
		v, err := e.EvalExpression(expr, vars, testPos)
		if err != nil {
			t.Errorf("%s: unexpected error %v", expr, err)
		} else if v != false {
			t.Errorf("%s = %#v, want false", expr, v)
		}
	}
	if v, _ := e.EvalExpression("d.present is defined", vars, testPos); v != true {
		t.Error("d.present should be defined")
	}

	// Strict: any real use errors with the full dotted name.
	for _, expr := range []string{
		"missing + 1", "missing | upper", "not missing", "missing > 1",
		"missing.deep | length",
	} {
		_, err := e.EvalExpression(expr, vars, testPos)
		if err == nil {
			t.Errorf("%s: expected UndefinedError", expr)
			continue
		}
		if !strings.Contains(err.Error(), "undefined") {
			t.Errorf("%s: error %q does not mention undefined", expr, err)
		}
	}
	// The name in the error is the dotted path.
	_, err := e.EvalExpression("missing.deep.attr | int", vars, testPos)
	if err == nil || !strings.Contains(err.Error(), "missing.deep.attr") {
		t.Errorf("dotted-path error = %v", err)
	}

	// default() rescues undefined at any depth.
	expectEq(t, evalExpr(t, "missing | default('fb')", nil), "fb", "default on undefined")
	expectEq(t, evalExpr(t, "missing.deep | default('fb')", nil), "fb", "default on chained undefined")
	expectEq(t, evalExpr(t, "'' | default('fb')", nil), "", "default does not replace empty string")
	expectEq(t, evalExpr(t, "'' | default('fb', true)", nil), "fb", "default with boolean replaces falsy")
}

func TestCoreFilters(t *testing.T) {
	vars := map[string]any{"l": []any{int64(3), int64(1), int64(2)}}
	cases := []struct {
		expr string
		want any
	}{
		{"'x' | upper", "X"},
		{"'ABC' | lower", "abc"},
		{"' pad ' | trim", "pad"},
		{"'hello world' | title", "Hello World"},
		{"'hello' | capitalize", "Hello"},
		{"'a,b,c' | split(',')", []any{"a", "b", "c"}},
		{"['a', 'b'] | join('-')", "a-b"},
		{"[1, 2, 3] | length", int64(3)},
		{"'hello' | length", int64(5)},
		{"{'a': 1} | length", int64(1)},
		{"l | first", int64(3)},
		{"l | last", int64(2)},
		{"'42' | int", int64(42)},
		{"'4.7' | int", int64(4)},
		{"'bad' | int", int64(0)},
		{"'bad' | int(99)", int64(99)},
		{"'ff' | int(base=16)", int64(255)},
		{"true | int", int64(1)},
		{"'3.14' | float", 3.14},
		{"'yes' | bool", true},
		{"'no' | bool", false},
		{"'True' | bool", true},
		{"1 | bool", true},
		{"0 | bool", false},
		{"42 | string", "42"},
		{"'abc' | list", []any{"a", "b", "c"}},
		{"'a-b' | replace('-', '_')", "a_b"},
		{"42 | type_debug", "int"},
		{"'s' | type_debug", "str"},
		{"[] | type_debug", "list"},
		{"none | type_debug", "NoneType"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}

	e := New()
	if _, err := e.EvalExpression("missing | mandatory", nil, testPos); err == nil {
		t.Error("mandatory on undefined should error")
	}
	if _, err := e.EvalExpression("1 | nosuchfilter", nil, testPos); err == nil ||
		!strings.Contains(err.Error(), `no filter named "nosuchfilter"`) {
		t.Error("unknown filter should name itself in the error")
	}
}

func TestCoreTests(t *testing.T) {
	vars := map[string]any{"n": int64(4), "s": "str", "l": []any{}, "d": map[string]any{}}
	cases := []struct {
		expr string
		want bool
	}{
		{"n is defined", true},
		{"nope is undefined", true},
		{"none is none", true},
		{"n is number", true},
		{"n is integer", true},
		{"2.5 is float", true},
		{"s is string", true},
		{"l is sequence", true},
		{"d is mapping", true},
		{"s is not mapping", true},
		{"true is boolean", true},
		{"n is even", true},
		{"3 is odd", true},
		{"n is divisibleby 2", true},
		{"n is divisibleby(2)", true},
		{"2 is in [1, 2]", true},
		{"n is eq 4", true},
		{"n is gt 3", true},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestRangeGlobal(t *testing.T) {
	expectEq(t, evalExpr(t, "range(3) | list", nil), []any{int64(0), int64(1), int64(2)}, "range(3)")
	expectEq(t, evalExpr(t, "range(1, 4) | list", nil), []any{int64(1), int64(2), int64(3)}, "range(1,4)")
	expectEq(t, evalExpr(t, "range(10, 0, -3) | list", nil), []any{int64(10), int64(7), int64(4), int64(1)}, "range step")
	// Lazy: length without materializing.
	expectEq(t, evalExpr(t, "range(100000000) | length", nil), int64(100000000), "lazy range length")
	expectEq(t, evalExpr(t, "range(10)[3]", nil), int64(3), "range index")
}

func TestNativeTypesRule(t *testing.T) {
	vars := map[string]any{
		"ports": []any{int64(80), int64(443)},
		"n":     int64(5),
		"d":     map[string]any{"a": int64(1)},
		"flag":  true,
	}
	// A single bare {{ expr }} returns the native value.
	expectEq(t, render(t, "{{ ports }}", vars), []any{int64(80), int64(443)}, "native list")
	expectEq(t, render(t, "{{ n }}", vars), int64(5), "native int")
	expectEq(t, render(t, "{{ d }}", vars), map[string]any{"a": int64(1)}, "native dict")
	expectEq(t, render(t, "{{ flag }}", vars), true, "native bool")
	expectEq(t, render(t, "{{ none }}", vars), nil, "native none")
	// Any surrounding text (even whitespace) stringifies.
	expectEq(t, render(t, " {{ n }}", vars), " 5", "leading space -> string")
	expectEq(t, render(t, "{{ n }}!", vars), "5!", "trailing text -> string")
	expectEq(t, render(t, "{{ n }}{{ n }}", vars), "55", "two outputs -> string")
	// Python-style stringification of containers.
	expectEq(t, render(t, "v={{ ports }}", vars), "v=[80, 443]", "list str form")
	expectEq(t, render(t, "v={{ d }}", vars), "v={'a': 1}", "dict str form")
	expectEq(t, render(t, "v={{ flag }}", vars), "v=True", "bool str form")
	// No template syntax: input returned unchanged.
	expectEq(t, render(t, "plain string", vars), "plain string", "no template")
}

func TestTextModeAndComments(t *testing.T) {
	vars := map[string]any{"name": "world"}
	expectEq(t, render(t, "hello {{ name }}", vars), "hello world", "interp")
	expectEq(t, render(t, "a {# comment #}b", vars), "a b", "comment stripped")
	expectEq(t, render(t, "{% raw %}{{ not_templated }}{% endraw %}", vars), "{{ not_templated }}", "raw")
	expectEq(t, render(t, "x {{- name -}} y", vars), "xworldy", "whitespace control")
	expectEq(t, render(t, "{{ 'lit {{ x }}' }}", vars), "lit {{ x }}", "braces in string literal")
}

func TestEvalBool(t *testing.T) {
	e := New()
	vars := MapVars{
		"ansible_os_family": "Debian",
		"count":             int64(0),
		"items":             []any{int64(1)},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{`ansible_os_family == "Debian"`, true},
		{`ansible_os_family == "RedHat"`, false},
		{"count", false}, // 0 is falsy
		{"items", true},  // non-empty list is truthy
		{"items | length > 0", true},
		{"count is defined and count == 0", true},
		{"missing is defined and missing > 1", false}, // short-circuit
	}
	for _, c := range cases {
		got, err := e.EvalBool(c.expr, vars, testPos)
		if err != nil {
			t.Errorf("EvalBool(%q): %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("EvalBool(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
	// when: with a plain undefined var errors (strict), unlike `x is defined`.
	if _, err := e.EvalBool("missing", vars, testPos); err == nil {
		t.Error("EvalBool(missing) should error")
	}
}

func TestStatementsRejectedForNow(t *testing.T) {
	e := New()
	_, err := e.RenderTemplate("{% if x %}y{% endif %}", nil, testPos)
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("statement error = %v", err)
	}
}

func TestFloatFormatting(t *testing.T) {
	expectEq(t, render(t, "{{ 2.0 }}x", nil), "2.0x", "float keeps .0")
	expectEq(t, render(t, "{{ 1 / 2 }}x", nil), "0.5x", "division output")
	expectEq(t, evalExpr(t, "10 / 4", nil), 2.5, "true div")
	if v := evalExpr(t, "0.1 + 0.2", nil).(float64); math.Abs(v-0.3) > 1e-9 {
		t.Errorf("0.1+0.2 = %v", v)
	}
}

func TestErrorsCarryPosition(t *testing.T) {
	e := New()
	pos := Position{File: "site.yml", Line: 14, Col: 7}
	_, err := e.RenderTemplate("{{ 1 + }}", nil, pos)
	if err == nil || !strings.Contains(err.Error(), "site.yml:14:7") {
		t.Errorf("error should carry document position, got: %v", err)
	}
	_, err = e.EvalExpression("boom | nosuch", nil, pos)
	if err == nil || !strings.Contains(err.Error(), "site.yml:14:7") {
		t.Errorf("eval error should carry position, got: %v", err)
	}
}

func FuzzRender(f *testing.F) {
	seeds := []string{
		"{{ x }}", "{{ 1 + 2 }}", "a {{ b }} c", "{% raw %}x{% endraw %}",
		"{# c #}", "{{ x | default(1) }}", "{{ x.y.z }}", "{{ [1,2][0] }}",
		"{{ 'a' ~ 1 }}", "{{ d.items() }}", "{{- x -}}", "{{ x if y else z }}",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	e := New()
	vars := MapVars{"x": int64(1), "y": "s", "d": map[string]any{"k": "v"}}
	f.Fuzz(func(t *testing.T, src string) {
		// Must never panic; errors are fine.
		e.RenderTemplate(src, vars, testPos)
		e.EvalExpression(src, vars, testPos)
	})
}
