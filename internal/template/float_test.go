package template

import (
	"math"
	"testing"
)

// Python's repr(float) (and so json.dumps) stays positional below 1e16.
func TestPyFloatRepr(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{1790738388.9217196, "1790738388.9217196"},
		{2.0, "2.0"},
		{0.1, "0.1"},
		{1e16, "1e+16"},
		{1.5e-05, "1.5e-05"},
		{0.0001, "0.0001"},
		{1e15, "1000000000000000.0"},
		{1234567890123456.8, "1234567890123456.8"},
		{1.2345678901234568e+16, "1.2345678901234568e+16"},
		{-3.25, "-3.25"},
		{5e-324, "5e-324"},
		{1.7976931348623157e+308, "1.7976931348623157e+308"},
		{math.Copysign(0, -1), "-0.0"},
	} {
		if got := pyFloatStr(c.f); got != c.want {
			t.Errorf("pyFloatStr(%v) = %q, want %q", c.f, got, c.want)
		}
	}
}
