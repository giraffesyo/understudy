package playbook

import (
	"reflect"
	"testing"
)

func TestParseKVJinja(t *testing.T) {
	cases := map[string]map[string]any{
		"name={{ item }} state=present":                  {"name": "{{ item }}", "state": "present"},
		"path={{ a | default('x y') }} mode=0644":        {"path": "{{ a | default('x y') }}", "mode": "0644"},
		`msg="hello {{ who }}" x={% if a %}1{% endif %}`: {"msg": "hello {{ who }}", "x": "{% if a %}1{% endif %}"},
		"a=1 b='two words'":                              {"a": "1", "b": "two words"},
	}
	for in, want := range cases {
		got, err := parseKV(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

// Free-form command lines keep their spacing and newlines, as ansible-core's
// split_args/join_args rebuild them; option words are taken out anywhere.
func TestSplitFreeFormKeepsWhitespace(t *testing.T) {
	cases := []struct {
		in, free string
		kv       map[string]any
	}{
		{"  echo hi  ", "  echo hi  ", nil},
		{"echo a\necho b\n", "echo a\necho b\n", nil},
		{"\n\necho a", "\n\necho a", nil},
		{"echo 'a  b' \"c  d\"", "echo 'a  b' \"c  d\"", nil},
		{"echo {{ x  }} y", "echo {{ x  }} y", nil},
		{"echo a  chdir=/tmp  \n", "echo a ", map[string]any{"chdir": "/tmp"}},
		{"iptables -F creates=/etc/x", "iptables -F", map[string]any{"creates": "/etc/x"}},
		{"echo a=b", "echo a=b", nil},
	}
	for _, c := range cases {
		free, kv := splitFreeForm(c.in, "shell")
		if free != c.free || !reflect.DeepEqual(kv, c.kv) {
			t.Errorf("%q: got %q %v, want %q %v", c.in, free, kv, c.free, c.kv)
		}
	}
}

// ParseKV is parse_kv as -e reads key=value words: quotes keep spaces
// together, escapes decode, other words join as _raw_params, and
// unbalanced quotes or blocks fail.
func TestParseKVExtraVars(t *testing.T) {
	cases := []struct {
		in   string
		keys []string
		vals map[string]string
	}{
		{`x="1 == 1"`, []string{"x"}, map[string]string{"x": "1 == 1"}},
		{`a='b c' d=2 free  words`, []string{"a", "d", "_raw_params"}, map[string]string{"a": "b c", "d": "2", "_raw_params": "free  words"}},
		{`e=a\=b f="q\"q" g=\x41\n h= i`, []string{"e", "f", "g", "h", "_raw_params"}, map[string]string{"e": `a\=b`, "f": `q"q`, "g": "A", "h": "", "_raw_params": "i"}},
		{`=x x=1 x=2`, []string{"x", "_raw_params"}, map[string]string{"_raw_params": "=x", "x": "2"}},
		{`p=1 \ q=2`, []string{"p", "q"}, map[string]string{"p": "1", "q": "2"}},
	}
	for _, c := range cases {
		keys, vals, err := ParseKV(c.in)
		if err != nil || !reflect.DeepEqual(keys, c.keys) || !reflect.DeepEqual(vals, c.vals) {
			t.Errorf("%s: got %q %q %v", c.in, keys, vals, err)
		}
	}
	for _, in := range []string{`x="unbal`, `x={{ foo`, `x="a\\"`} {
		if _, _, err := ParseKV(in); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}
