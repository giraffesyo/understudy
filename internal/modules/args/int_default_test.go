package args

import "testing"

func TestIntDefaultsAreUsable(t *testing.T) {
	p, err := Spec{"timeout": {Type: "int", Default: 300}}.Parse(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Int("timeout"); got != 300 {
		t.Fatalf("Int default = %d, want 300", got)
	}
}
