package template

import (
	"reflect"
	"strings"
	"testing"
)

func TestInPlaceMethods(t *testing.T) {
	e := New()
	vars := MapVars{"d": map[string]any{"a": int64(1)}, "l": []any{int64(1)}}
	for src, want := range map[string]string{
		"{% set x = [3, 1] %}{% set _ = x.append(2) %}{% set _ = x.sort() %}{{ x }}":                 "[1, 2, 3]",
		"{% set x = [1] %}{% set _ = x.extend('ab') %}{% set _ = x.insert(-1, 0) %}{{ x }}":          "[1, 'a', 0, 'b']",
		"{% set x = [1, 2, 3] %}{{ x.pop() }}{{ x.pop(0) }}{{ x }}":                                  "31[2]",
		"{% set x = [1, 2] %}{% set _ = x.remove(1) %}{% set _ = x.reverse() %}{{ x }}":              "[2]",
		"{% set m = {'b': 1} %}{% set _ = m.update({'a': 2}, c=3) %}{{ m }}":                         "{'b': 1, 'a': 2, 'c': 3}",
		"{% set m = {'a': 1} %}{{ m.setdefault('b', 2) }}{{ m.setdefault('a', 9) }}{{ m.pop('a') }}": "211",
		"{% set m = {'a': 1, 'b': 2} %}{{ m.popitem() }}{{ m }}":                                     "['b', 2]{'a': 1}",
		"{% set out = [] %}{% for i in [1, 2] %}{% set _ = out.append(i) %}{% endfor %}{{ out }}":    "[1, 2]",
		"{% set x = {'k': [1]} %}{% set _ = x.k.append(2) %}{{ x }}":                                 "{'k': [1, 2]}",
		"{% set ns = namespace(d={}) %}{% set _ = ns.d.update(a=1) %}{{ ns.d }}":                     "{'a': 1}",
		"{{ d.update({'b': 2}) }}{{ d }} {{ l.append(2) }}{{ l }}":                                   "{'a': 1, 'b': 2} [1, 2]",
		"{% set _ = [].pop() %}ok": "ok",
		// A change shows through every name and container holding the object.
		"{% set y = [] %}{% set x = y %}{% set _ = x.append(1) %}{{ y }}":                 "[1]",
		"{% set n = [[1]] %}{% for i in n %}{% set _ = i.append(2) %}{% endfor %}{{ n }}": "[[1, 2]]",
		"{% set m = d %}{% set _ = m.update(b=2) %}{{ d }}":                               "{'a': 1, 'b': 2}",
	} {
		out, err := e.RenderString(src, vars, testPos)
		if err != nil || out != want {
			t.Errorf("%s = %q, %v; want %q", src, out, err, want)
		}
	}
	// The variables themselves never change.
	if !reflect.DeepEqual(vars["d"], map[string]any{"a": int64(1)}) || !reflect.DeepEqual(vars["l"], []any{int64(1)}) {
		t.Errorf("variables mutated: %v", vars)
	}
	for src, want := range map[string]string{
		"{{ [].pop() }}":                                    "pop from empty list",
		"{% set m = {} %}{{ m.pop('zz') }}":                 "'zz'",
		"{% set x = [1] %}{% set y = x.remove(5) %}{{ y }}": "list.remove(x): x not in list",
		"{{ {}.update(1) }}":                                "'int' object is not iterable",
	} {
		_, err := e.RenderTemplate(src, vars, testPos)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", src, err, want)
		}
	}
}

func TestNativeChunks(t *testing.T) {
	e := New()
	for src, want := range map[string]any{
		"{% set z = 1 %}":                          nil,
		"{% if true %}{{ [5] }}{% endif %}":        []any{int64(5)},
		"{{ none }}{{ [1] }}":                      []any{int64(1)},
		"{{ '' }}{{ [1] }}":                        "[1]",
		"{% for i in [1, 2] %}{{ i }}{% endfor %}": "12",
	} {
		got, err := e.RenderTemplate(src, MapVars{}, testPos)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, %v; want %#v", src, got, err, want)
		}
	}
}

func TestCause(t *testing.T) {
	e := New()
	vars := MapVars{"d": map[string]any{"a": int64(1)}}
	for src, want := range map[string]string{
		"{{ nope }}":    "'nope' is undefined",
		"{{ nope.x }}":  "'nope' is undefined",
		"{{ d.zz.y }}":  "object of type 'dict' has no attribute 'zz'",
		"{{ [1][5] }}":  "object of type 'list' has no attribute 5",
		"{{ 'a' + 1 }}": `Error rendering template: can only concatenate str (not "int") to str`,
		"{{ 1 + }}":     "Syntax error in template: ",
		"{{ d.a.b }}":   "object of type 'int' has no attribute 'b'",
	} {
		_, err := e.RenderTemplate(src, vars, testPos)
		got, ok := Cause(err)
		if !ok || !strings.HasPrefix(got, want) {
			t.Errorf("%s: cause = %q (%v), want %q", src, got, err, want)
		}
	}
}
