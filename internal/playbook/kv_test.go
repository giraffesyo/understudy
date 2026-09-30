package playbook

import (
	"reflect"
	"testing"
)

func TestParseKVJinja(t *testing.T) {
	cases := map[string]map[string]any{
		"name={{ item }} state=present":                   {"name": "{{ item }}", "state": "present"},
		"path={{ a | default('x y') }} mode=0644":         {"path": "{{ a | default('x y') }}", "mode": "0644"},
		`msg="hello {{ who }}" x={% if a %}1{% endif %}`: {"msg": "hello {{ who }}", "x": "{% if a %}1{% endif %}"},
		"a=1 b='two words'":                               {"a": "1", "b": "two words"},
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
