package yaml

import (
	"strings"
	"testing"
)

// TestRecursiveAlias: an alias inside the collection it names builds a
// recursive structure, as PyYAML does; every alias is the same value.
func TestRecursiveAlias(t *testing.T) {
	v, err := Unmarshal([]byte("m: &m\n  name: top\n  self: *m\nl: &l\n  - 1\n  - *l\nboth: [*m, *l]\n"), "rec.yml")
	if err != nil {
		t.Fatal(err)
	}
	root := v.(*OMap)
	m := root.Get("m").(*OMap)
	if m.Get("self") != m || m.Get("self").(*OMap).Get("self").(*OMap).Get("name") != "top" {
		t.Errorf("m = %#v", m)
	}
	l := root.Get("l").([]any)
	inner := l[1].([]any)
	if len(inner) != 2 || &inner[0] != &l[0] || inner[1].([]any)[0] != int64(1) {
		t.Errorf("l = %#v", l)
	}
	both := root.Get("both").([]any)
	if both[0] != m || &both[1].([]any)[0] != &l[0] {
		t.Errorf("aliases are not the anchored values: %#v", both)
	}
	if _, err := Unmarshal([]byte("a: &a\n  k: 1\n  <<: *a\n"), "rec.yml"); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Errorf("a mapping merging itself: %v", err)
	}
}
