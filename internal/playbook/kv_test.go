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
