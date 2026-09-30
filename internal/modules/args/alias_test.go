package args

import (
	"reflect"
	"testing"
)

// An option set along with its aliases: the last alias set wins, and each
// alias set after the option (or an earlier alias) warns, as
// _handle_aliases does.
func TestAliasesOverrideTheOption(t *testing.T) {
	spec := Spec{
		"path": {Required: true, Aliases: []string{"dest", "destfile", "name"}},
		"mode": {},
	}
	cases := []struct {
		raw   map[string]any
		want  string
		warns []string
	}{
		{map[string]any{"path": "p"}, "p", nil},
		{map[string]any{"dest": "d"}, "d", nil},
		{map[string]any{"path": "p", "dest": "d"}, "d",
			[]string{"Both option path and its alias dest are set."}},
		{map[string]any{"path": "p", "dest": "d", "name": "n"}, "n",
			[]string{"Both option path and its alias dest are set.", "Both option path and its alias name are set."}},
		{map[string]any{"dest": "d", "destfile": "f"}, "f",
			[]string{"Both option path and its alias destfile are set."}},
	}
	for _, c := range cases {
		p, err := spec.Parse(c.raw)
		if err != nil {
			t.Fatalf("%v: %v", c.raw, err)
		}
		if got := p.Str("path"); got != c.want {
			t.Errorf("%v: path = %q, want %q", c.raw, got, c.want)
		}
		if got := spec.AliasWarnings(c.raw); !reflect.DeepEqual(got, c.warns) {
			t.Errorf("%v: warnings %q, want %q", c.raw, got, c.warns)
		}
		resolved := spec.ResolveAliases(c.raw)
		if resolved["path"] != c.want || len(resolved) != 1 {
			t.Errorf("%v: resolved %v", c.raw, resolved)
		}
	}
}
