package template

import "testing"

// Expected values are Python 3 repr() output.
func TestPyStrRepr(t *testing.T) {
	for in, want := range map[string]string{
		"abc":              `'abc'`,
		"it's":             `"it's"`,
		`say "hi"`:         `'say "hi"'`,
		`both ' and "`:     `'both \' and "'`,
		`name = 'new\'s'`:  `"name = 'new\\'s'"`,
		"tab\there\nnl\\":  `'tab\there\nnl\\'`,
		"bell\x07 del\x7f": `'bell\x07 del\x7f'`,
		"caf\u00e9 \u200b": "'café \\u200b'",
		"":                 `''`,
	} {
		if got := pyRepr(in); got != want {
			t.Errorf("repr(%q) = %s, want %s", in, got, want)
		}
	}
	if got := toStr([]any{"it's", "x"}); got != `["it's", 'x']` {
		t.Errorf("list str = %s", got)
	}
}
