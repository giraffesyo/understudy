package template

import (
	"reflect"
	"testing"
)

// keepVars is MapVars asking for deprecated values to be kept.
type keepVars struct{ MapVars }

func (keepVars) KeepDeprecated() bool { return true }

func TestDeprecatedValues(t *testing.T) {
	dep := Deprecated{Value: "old", Msg: "The 'old' value is deprecated.", Version: "2.24"}
	vars := MapVars{
		"r":     map[string]any{"new": "old", "old": dep, "n": []any{1}},
		"fact":  Deprecated{Value: "Linux", Msg: "fact", Version: "2.24"},
		"plain": "x",
	}
	cases := []struct {
		src   string
		want  any
		warns int
	}{
		{"{{ r.new }}", "old", 0},
		{"{{ r.old }}", "old", 1},
		{"{{ r['old'] }}", "old", 1},
		{"x {{ r.old }} {{ r.old }}", "x old old", 2},
		{"{{ r }}", map[string]any{"new": "old", "old": "old", "n": []any{1}}, 1},
		{"{{ r.n }}", []any{1}, 0},
		{"{{ r | default({}) }}", map[string]any{"new": "old", "old": "old", "n": []any{1}}, 1},
		{"{{ (r | default({})).n | length }}", int64(1), 0},
		{"{{ r | to_json }}", `{"n": [1], "new": "old", "old": "old"}`, 1},
		{"{{ fact }}", "Linux", 1},
		{"{{ fact | lower }}", "linux", 1},
		{"{{ plain }}", "x", 0},
	}
	for _, c := range cases {
		e := New()
		warns := 0
		e.Deprecation = func(pos Position, d Deprecated) {
			warns++
			if pos != testPos {
				t.Errorf("%s: warned at %v", c.src, pos)
			}
		}
		got, err := e.RenderTemplate(c.src, vars, testPos)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.src, got, c.want)
		}
		if warns != c.warns {
			t.Errorf("%s: %d warnings, want %d", c.src, warns, c.warns)
		}
	}

	// Kept: a copy of a deprecated value stays deprecated, a derived
	// value does not.
	e := New()
	got, _ := e.RenderTemplate("{{ fact }}", keepVars{vars}, testPos)
	if d, ok := got.(Deprecated); !ok || d.Value != "Linux" {
		t.Errorf("kept fact = %#v", got)
	}
	got, _ = e.RenderTemplate("{{ fact | lower }}", keepVars{vars}, testPos)
	if got != "linux" {
		t.Errorf("derived value = %#v", got)
	}
	got, _ = e.RenderTemplate("{{ r }}", keepVars{vars}, testPos)
	if m := got.(map[string]any); !reflect.DeepEqual(m["old"], dep) {
		t.Errorf("kept dict = %#v", got)
	}
	if b, err := e.EvalBool("r.old == 'old' and fact", vars, testPos); err != nil || !b {
		t.Errorf("EvalBool = %v, %v", b, err)
	}
}
