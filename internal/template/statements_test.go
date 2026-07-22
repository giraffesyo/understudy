package template

import "testing"

func TestStatements(t *testing.T) {
	vars := map[string]any{
		"users": []any{"alice", "bob"},
		"d":     map[string]any{"b": int64(2), "a": int64(1)},
		"flag":  true,
		"n":     int64(3),
	}
	cases := []struct {
		src  string
		want any
	}{
		{"{% if flag %}yes{% endif %}", "yes"},
		{"{% if not flag %}a{% else %}b{% endif %}", "b"},
		{"{% if n == 1 %}one{% elif n == 3 %}three{% else %}many{% endif %}", "three"},
		{"{% for u in users %}{{ u }};{% endfor %}", "alice;bob;"},
		{"{% for u in users %}{{ loop.index }}:{{ u }}{% if not loop.last %},{% endif %}{% endfor %}", "1:alice,2:bob"},
		{"{% for k, v in d.items() %}{{ k }}={{ v }} {% endfor %}", "a=1 b=2 "},
		{"{% for x in [] %}x{% else %}empty{% endfor %}", "empty"},
		{"{% for i in range(5) if i % 2 == 0 %}{{ i }}{% endfor %}", "024"},
		{"{% set greeting = 'hi' %}{{ greeting }} there", "hi there"},
		{"{% set total = n * 2 %}{{ total }}", int64(6)},
		{"{% for i in range(3) %}{% if i > 0 %}{{ i }}{% endif %}{% endfor %}", "12"},
		{"{% for u in users %}{% for c in u[:1] %}{{ c }}{% endfor %}{% endfor %}", "ab"},
		{"a\n{% if flag %}b\n{% endif %}c", "a\nb\nc"}, // TrimBlocks eats newline after %}
		{"x {%- if flag %} y{% endif %}", "x y"},       // whitespace control on block tags
	}
	for _, c := range cases {
		expectEq(t, render(t, c.src, vars), c.want, c.src)
	}
	// loop var does not leak out of the loop.
	e := New()
	_, err := e.RenderTemplate("{% for u in users %}{% endfor %}{{ u }}", MapVars(vars), testPos)
	if err == nil {
		t.Error("loop var leaked out of for loop")
	}
}
