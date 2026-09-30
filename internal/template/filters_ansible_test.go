package template

import (
	"strings"
	"testing"
)

func TestSequenceFilters(t *testing.T) {
	vars := map[string]any{
		"nums":   []any{int64(3), int64(1), int64(2)},
		"nested": []any{[]any{int64(1)}, []any{int64(2), []any{int64(3)}}},
		"users": []any{
			map[string]any{"name": "alice", "uid": int64(1), "admin": true},
			map[string]any{"name": "bob", "uid": int64(2), "admin": false},
			map[string]any{"name": "carol", "uid": int64(3), "admin": true},
		},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"nums | min", int64(1)},
		{"nums | max", int64(3)},
		{"nums | sum", int64(6)},
		{"[1, 1, 2, 2, 3] | unique", []any{int64(1), int64(2), int64(3)}},
		{"nums | sort", []any{int64(1), int64(2), int64(3)}},
		{"nums | sort(reverse=true)", []any{int64(3), int64(2), int64(1)}},
		{"nums | reverse", []any{int64(2), int64(1), int64(3)}},
		{"'abc' | reverse", "cba"},
		{"nested | flatten", []any{int64(1), int64(2), int64(3)}},
		{"nested | flatten(levels=1)", []any{int64(1), int64(2), []any{int64(3)}}},
		{"[1, 2] | zip(['a', 'b']) | list", []any{[]any{int64(1), "a"}, []any{int64(2), "b"}}},
		{"users | map(attribute='name') | list", []any{"alice", "bob", "carol"}},
		{"['a', 'b'] | map('upper') | list", []any{"A", "B"}},
		{"[0, 1, '', 'x'] | select | list", []any{int64(1), "x"}},
		{"[0, 1, '', 'x'] | reject | list", []any{int64(0), ""}},
		{"[1, 2, 3, 4] | select('gt', 2) | list", []any{int64(3), int64(4)}},
		{"users | selectattr('admin') | map(attribute='name') | list", []any{"alice", "carol"}},
		{"users | rejectattr('admin') | map(attribute='name') | list", []any{"bob"}},
		{"users | selectattr('uid', 'eq', 2) | map(attribute='name') | list", []any{"bob"}},
		{"users | sort(attribute='uid', reverse=true) | map(attribute='name') | list", []any{"carol", "bob", "alice"}},
		{"[1, 2, 3] | union([3, 4])", []any{int64(1), int64(2), int64(3), int64(4)}},
		{"[1, 2, 3] | intersect([2, 3, 4])", []any{int64(2), int64(3)}},
		{"[1, 2, 3] | difference([2])", []any{int64(1), int64(3)}},
		{"[1, 2] | symmetric_difference([2, 3])", []any{int64(1), int64(3)}},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestDictFilters(t *testing.T) {
	vars := map[string]any{
		"d": map[string]any{"b": int64(2), "a": int64(1)},
		"defaults": map[string]any{
			"opts": map[string]any{"x": int64(1), "y": int64(2)},
			"name": "base",
		},
		"override": map[string]any{
			"opts": map[string]any{"y": int64(99)},
		},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"d | dict2items", []any{
			map[string]any{"key": "a", "value": int64(1)},
			map[string]any{"key": "b", "value": int64(2)},
		}},
		{"d | dict2items(key_name='k', value_name='v') | map(attribute='k') | list", []any{"a", "b"}},
		{"[{'key': 'x', 'value': 1}] | items2dict", map[string]any{"x": int64(1)}},
		{"d | combine({'c': 3})", map[string]any{"a": int64(1), "b": int64(2), "c": int64(3)}},
		// Top-level replace: opts is wholly replaced.
		{"defaults | combine(override)", map[string]any{
			"name": "base",
			"opts": map[string]any{"y": int64(99)},
		}},
		// recursive=True deep-merges.
		{"defaults | combine(override, recursive=true)", map[string]any{
			"name": "base",
			"opts": map[string]any{"x": int64(1), "y": int64(99)},
		}},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestChunkingAndSizeFilters(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		// batch / slice (Jinja2 builtins)
		{"[1, 2, 3, 4, 5] | batch(2) | list", []any{
			[]any{int64(1), int64(2)}, []any{int64(3), int64(4)}, []any{int64(5)}}},
		{"[1, 2, 3, 4, 5] | batch(2, 0) | list", []any{
			[]any{int64(1), int64(2)}, []any{int64(3), int64(4)}, []any{int64(5), int64(0)}}},
		{"['a', 'b', 'c', 'd', 'e'] | slice(2) | list", []any{
			[]any{"a", "b", "c"}, []any{"d", "e"}}},
		{"[1, 2, 3, 4] | slice(3) | list", []any{
			[]any{int64(1), int64(2)}, []any{int64(3)}, []any{int64(4)}}},
		// truncate / wordwrap
		{"'the quick brown fox' | truncate(10, true, '...')", "the qui..."},
		{"'the quick brown fox' | truncate(12, false, '...')", "the..."},
		{"'short' | truncate(20)", "short"},
		{"'hello world foo bar baz' | wordwrap(10)", "hello\nworld foo\nbar baz"},
		// human_readable / human_to_bytes
		{"500 | human_readable", "500.00 Bytes"},
		{"1024 | human_readable", "1.00 KB"},
		{"1048576 | human_readable", "1.00 MB"},
		{"1024 | human_readable(isbits=true)", "1.00 Kb"},
		{"'1024' | human_to_bytes", int64(1024)},
		{"'2MB' | human_to_bytes", int64(2097152)},
		{"'1.5 GB' | human_to_bytes", int64(1610612736)},
		// combine list_merge
		{"{'x': [1, 2]} | combine({'x': [3]}, list_merge='append')", map[string]any{"x": []any{int64(1), int64(2), int64(3)}}},
		{"{'x': [1, 2]} | combine({'x': [3]}, list_merge='prepend')", map[string]any{"x": []any{int64(3), int64(1), int64(2)}}},
		{"{'x': [1, 2]} | combine({'x': [3]}, list_merge='keep')", map[string]any{"x": []any{int64(1), int64(2)}}},
		{"{'x': [1, 2, 3]} | combine({'x': [2, 4]}, list_merge='append_rp')", map[string]any{"x": []any{int64(1), int64(3), int64(2), int64(4)}}},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
}

func TestMathAndStringPadFilters(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{"'hi' | center(10)", "    hi    "},
		{"'abc' | center(2)", "abc"},
		{"2 | pow(10)", 1024.0},
		{"16 | root", 4.0},
		{"8 | log(2)", 3.0},
		{"100 | log(10)", 2.0},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
	// strftime renders in local time; assert structure, not an exact string.
	if got := evalExpr(t, "'%Y-%m-%d' | strftime(1609459200)", nil).(string); len(got) != 10 {
		t.Errorf("strftime date length = %d (%q), want 10", len(got), got)
	}
	// random with a seed is deterministic within understudy.
	a := evalExpr(t, "1000 | random(seed='x')", nil)
	b := evalExpr(t, "1000 | random(seed='x')", nil)
	expectEq(t, a, b, "seeded random is deterministic")
	if n, ok := a.(int64); !ok || n < 0 || n >= 1000 {
		t.Errorf("random(1000) = %v, want int64 in [0,1000)", a)
	}
	if elem := evalExpr(t, "['only'] | random", nil); elem != "only" {
		t.Errorf("random of single-element list = %v, want 'only'", elem)
	}
}

// withCryptGensalt pins whether password_hash emulates libxcrypt's
// crypt_gensalt (see cryptGensalt) for the duration of a test.
func withCryptGensalt(t *testing.T, on bool) {
	saved := cryptGensalt
	cryptGensalt = func() bool { return on }
	t.Cleanup(func() { cryptGensalt = saved })
}

func TestPasswordHash(t *testing.T) {
	withCryptGensalt(t, false)
	// Oracle values from real ansible-playbook (passlib), with pinned salt and
	// rounds so the result is deterministic.
	cases := []struct {
		expr string
		want any
	}{
		{"'mypassword' | password_hash('sha512', 'abcdefghijklmnop', rounds=5000)",
			"$6$abcdefghijklmnop$jxZ5UKgPKWlCx21QPZbkQOj73EKOWhff2HX66XmEGXBN7/VGv5K5AH0mgtIbyEHEwJOO3UibHo1CrTlQvXbbS/"},
		{"'mypassword' | password_hash('sha256', 'abcdefghijklmnop', rounds=5000)",
			"$5$abcdefghijklmnop$oZAI4Z3YFTVIrKPkvxU2vFozcTT4/RqEMnF1aR4uWP3"},
		{"'secret' | password_hash('sha512', 'saltsalt', rounds=10000)",
			"$6$rounds=10000$saltsalt$WowrPBpEDVlCoruBosYlrZycTCx3//TyDHYqEhX9DUHHt0XTztUqzQDDUuvUGRA8aUe9p55hcAxeGcu58sm3u."},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
}

func TestPasswordHashGensalt(t *testing.T) {
	withCryptGensalt(t, true)
	// Oracle values from ansible-core 2.21 on glibc Linux (libxcrypt): the
	// given salt is crypt_gensalt's random input, not the salt itself.
	cases := []struct {
		expr string
		want any
	}{
		{"'mypassword' | password_hash('sha512', 'abcdefghijklmnop')",
			"$6$rounds=656000$V7qMYJaNbVKOeh4P$Bw7twflTtNzN63J28ZPlFpgy6I8ze13f5FcXiATdX1Qrs/QXsWaYXZKF6QPqamwtSvQZ0Ei5NK970D5Sm7lN2."},
		{"'mypassword' | password_hash('sha512', 'abcdefghijklmnop', rounds=5000)",
			"$6$V7qMYJaNbVKOeh4P$wBeDZdD1X0zRkWwOc..unOHiRrDYJU1TADvPNX2f3zS0zTorB8eowFCyDI7qYifCMYdB4dQHKrsfwjgafYgjk1"},
		{"'mypassword' | password_hash('sha512', 'saltsalt', rounds=10000)",
			"$6$rounds=10000$n34PoBLM$kWnIfXbGjJL7RrDYsokap4W7J8amDeEMYv8rYdOpIII1423Y08xThs2CZT8vgY52iCwu8BRRKI.sxkBydu8Vr1"},
		{"'mypassword' | password_hash('sha512', 'abcd')",
			"$6$rounds=656000$V7qM$oPJphU8zLNCxVF7mysypeUveskFj3aGkeJzJgVp00D.3aGBBiwCaP/i9Bqt8yIY4J3.Jjc0kbUIql3Xm4arBf/"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
}

func TestGroupBy(t *testing.T) {
	vars := map[string]any{
		"servers": []any{
			map[string]any{"name": "web1", "role": "web"},
			map[string]any{"name": "db1", "role": "db"},
			map[string]any{"name": "web2", "role": "web"},
		},
		"mixed": []any{
			map[string]any{"k": "Web"},
			map[string]any{"k": "web"},
		},
	}
	cases := []struct {
		expr string
		want any
	}{
		// Groups sorted by key; grouper via numeric attribute on the pair.
		{"servers | groupby('role') | map(attribute='0') | list", []any{"db", "web"}},
		// Grouped rows keep source order.
		{"servers | groupby('role') | selectattr('0', 'eq', 'web') | map(attribute='1') | first | map(attribute='name') | list",
			[]any{"web1", "web2"}},
		// Case-insensitive by default: Web/web fold, first-seen grouper kept.
		{"mixed | groupby('k') | map(attribute='0') | list", []any{"Web"}},
		// case_sensitive=true keeps distinct-case keys apart, ASCII-sorted.
		{"mixed | groupby('k', case_sensitive=true) | map(attribute='0') | list", []any{"Web", "web"}},
		// extractAttr indexes lists: attribute='1' is the group's row list length.
		{"servers | groupby('role') | map(attribute='1') | map('length') | list", []any{int64(1), int64(2)}},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, vars), c.want, c.expr)
	}
}

func TestSerializationFilters(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{"{'b': 1, 'a': [2]} | to_json", `{"b": 1, "a": [2]}`},                                       // insertion order, Python separators
		{"{'b': 1, 'a': [2]} | to_nice_json", "{\n    \"a\": [\n        2\n    ],\n    \"b\": 1\n}"}, // sort_keys=True
		{`'{"x": 5}' | from_json`, map[string]any{"x": int64(5)}},
		{`'a: 1' | from_yaml`, map[string]any{"a": int64(1)}},
		{"'hello' | b64encode", "aGVsbG8="},
		{"'aGVsbG8=' | b64decode", "hello"},
		{"'abc' | hash('sha256')", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"'x' | quote", "'x'"},
		{"\"it's\" | quote", `'it'"'"'s'`},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
	// to_yaml round-trips through our own parser.
	out := evalExpr(t, "{'a': [1, 2]} | to_yaml", nil).(string)
	back := evalExpr(t, "v | from_yaml", map[string]any{"v": out})
	expectEq(t, back, map[string]any{"a": []any{int64(1), int64(2)}}, "to_yaml round trip")
}

// to_yaml/to_nice_yaml take yaml.dump's keyword arguments (the layout
// itself is checked against PyYAML in the yaml package's dump corpus).
func TestToYAMLArguments(t *testing.T) {
	x := []any{int64(1), int64(2)}
	cases := []struct {
		expr string
		want string
	}{
		{"{'a': x} | to_yaml", "a: [1, 2]\n"},
		{"{'a': x} | to_nice_yaml", "a:\n- 1\n- 2\n"},
		{"{'a': [x]} | to_nice_yaml(2)", "a:\n- - 1\n  - 2\n"},
		{"{'a': [x]} | to_nice_yaml(indent=2.5)", "a:\n- - 1\n  - 2\n"},
		{"{'a': x} | to_yaml(5, 7)", "a: [1, 2]\n"},
		{"{'a': x} | to_nice_yaml(default_flow_style=none)", "a: [1, 2]\n"},
		{"{'a': x} | to_yaml(default_style='x')", "a:\n- 1\n- 2\n"},
		{"{'b': 1, 'a': x} | to_yaml(sort_keys=0, explicit_start=1)", "---\nb: 1\na: [1, 2]\n"},
		{"{'a': x, 'b': x} | to_yaml", "a: &id001 [1, 2]\nb: *id001\n"},
		{"x | to_yaml(encoding='utf-8', vault_behavior='redact')", "[1, 2]\n"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, map[string]any{"x": x}), c.want, c.expr)
	}
	for expr, want := range map[string]string{
		"x | to_yaml(allow_unicode=False)":    "yaml.dump() got multiple values for keyword argument 'allow_unicode'",
		"x | to_yaml(bogus=1)":                "dump_all() got an unexpected keyword argument 'bogus'",
		"x | to_yaml(indent='3')":             "an integer is required",
		"x | to_yaml(vault_behavior='other')": "The vault parameter must be one of decrypt, keep_encrypted, redact, fail",
		"range(3) | to_yaml":                  "('cannot represent an object', range(0, 3))",
	} {
		_, err := New().EvalExpression(expr, MapVars(map[string]any{"x": x}), testPos)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", expr, err, want)
		}
	}
}

func TestPathAndMiscFilters(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{"'/etc/nginx/nginx.conf' | basename", "nginx.conf"},
		{"'/etc/nginx/nginx.conf' | dirname", "/etc/nginx"},
		{"'file.tar.gz' | splitext", []any{"file.tar", ".gz"}},
		{"['/etc', 'nginx', 'conf.d'] | path_join", "/etc/nginx/conf.d"},
		{"true | ternary('yes', 'no')", "yes"},
		{"false | ternary('yes', 'no')", "no"},
		{"-5 | abs", int64(5)},
		{"2.567 | round(2)", 2.57},
		{"2.5 | round", 2.0}, // banker's rounding (half to even), matching Python
		{"3.5 | round", 4.0},
		{"'a' | extract({'a': 42})", int64(42)},
		{"'x\\ny' | indent(2)", "x\n  y"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
	// The famous cross-host pattern: extract over hostvars-like maps.
	vars := map[string]any{
		"hv":    map[string]any{"h1": map[string]any{"ip": "10.0.0.1"}, "h2": map[string]any{"ip": "10.0.0.2"}},
		"names": []any{"h1", "h2"},
	}
	expectEq(t,
		evalExpr(t, "names | map('extract', hv, 'ip') | list", vars),
		[]any{"10.0.0.1", "10.0.0.2"},
		"map extract hostvars")
}

func TestRegexFilters(t *testing.T) {
	cases := []struct {
		expr string
		want any
	}{
		{`'ansible-2.16' | regex_replace('^ansible-', '')`, "2.16"},
		{`'hello world' | regex_replace('(\\w+) (\\w+)', '\\2 \\1')`, "world hello"},
		{`'server01' | regex_search('\\d+')`, "01"},
		{`'no digits here' | regex_search('\\d+')`, nil},
		{`'a1b2c3' | regex_findall('\\d')`, []any{"1", "2", "3"}},
		{`'key=val' | regex_search('(\\w+)=(\\w+)', '\\1', '\\2')`, []any{"key", "val"}},
		{`'a.b' | regex_escape`, `a\.b`},
		{`'Version 1.2' | regex_replace('(?i)version', 'v')`, "v 1.2"},
	}
	for _, c := range cases {
		expectEq(t, evalExpr(t, c.expr, nil), c.want, c.expr)
	}
	// RE2-unsupported constructs fail loudly.
	e := New()
	for _, expr := range []string{
		`'x' | regex_search('(?=lookahead)')`,
		`'x' | regex_replace('(a)\\1', 'b')`,
	} {
		_, err := e.EvalExpression(expr, nil, testPos)
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: expected loud rejection, got %v", expr, err)
		}
	}
}

func TestVersionAndResultTests(t *testing.T) {
	vars := map[string]any{
		"okRes":   map[string]any{"failed": false, "changed": true, "skipped": false},
		"badRes":  map[string]any{"failed": true, "changed": false},
		"skipRes": map[string]any{"skipped": true, "changed": false, "failed": false},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"'2.16.0' is version('2.10', '>=')", true},
		{"'1.9' is version('1.10', '<')", true}, // numeric segments: 9 < 10
		{"'1.10' is version('1.9', '>')", true},
		{"'20.04' is version('18.04', '>=')", true},
		{"'1.0.0' is version('1.0', '==')", true}, // missing segment = 0
		{"okRes is success", true},
		{"okRes is succeeded", true},
		{"okRes is changed", true},
		{"okRes is not failed", true},
		{"badRes is failed", true},
		{"badRes is not success", true},
		{"skipRes is skipped", true},
		{"'web01' is match('web')", true},
		{"'web01' is match('01')", false}, // match anchors at start
		{"'web01' is search('01')", true},
		{"[1, 2] is subset([1, 2, 3])", true},
		{"[1, 2, 3] is superset([1, 2])", true},
		{"[false, true] is any", true},
		{"[true, true] is all", true},
		{"[true, false] is not all", true},
	}
	for _, c := range cases {
		got := evalExpr(t, c.expr, vars)
		if got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}
}
