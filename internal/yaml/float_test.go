package yaml

import "testing"

// PyYAML's represent_float: repr(f) with ".0" kept in the mantissa.
func TestFormatFloat(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{1790738388.9217196, "1790738388.9217196"},
		{2.0, "2.0"},
		{1e16, "1.0e+16"},
		{1.5e-05, "1.5e-05"},
		{1e-05, "1.0e-05"},
		{1e15, "1000000000000000.0"},
		{1.2345678901234568e+16, "1.2345678901234568e+16"},
	} {
		if got := formatFloat(c.f); got != c.want {
			t.Errorf("formatFloat(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}
