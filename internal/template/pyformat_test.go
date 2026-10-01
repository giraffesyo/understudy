package template

import "testing"

// Expected values are CPython 3.14's str % tuple.
func TestPyPercentFormat(t *testing.T) {
	cases := []struct {
		format string
		args   []any
		want   string
		err    string
	}{
		{"%s and %r", []any{"a", "b"}, "a and 'b'", ""},
		{"%5d|%-5d|%05d|%+d", []any{int64(42), int64(42), int64(-42), int64(7)}, "   42|42   |-0042|+7", ""},
		{"%x %X %#x %#o %o", []any{int64(255), int64(255), int64(255), int64(8), int64(-8)}, "ff FF 0xff 0o10 -10", ""},
		{"%.2f %e %g %G", []any{3.14159, 12345.678, 0.00001234, 1e20}, "3.14 1.234568e+04 1.234e-05 1E+20", ""},
		{"%d", []any{3.99}, "3", ""},
		{"%.3s|%c|%c", []any{"abcdef", int64(65), "z"}, "abc|A|z", ""},
		{"%%%s", []any{int64(1)}, "%1", ""},
		{"%d", []any{"x"}, "", "%d format: a real number is required, not str"},
		{"%x", []any{1.5}, "", "%x format: an integer is required, not float"},
		{"%s %s", []any{"a"}, "", "not enough arguments for format string"},
		{"%s", []any{"a", "b"}, "", "not all arguments converted during string formatting"},
		{"abc", []any{"a"}, "", "not all arguments converted during string formatting"},
		{"%z", []any{"a"}, "", "unsupported format character 'z' (0x7a) at index 1"},
		{"%", nil, "", "incomplete format"},
		{"%c", []any{"ab"}, "", "%c requires an int or a unicode character, not a string of length 2"},
		{"%5.1f%%", []any{int64(3)}, "  3.0%", ""},
	}
	for _, c := range cases {
		got, err := pyPercentFormat(c.format, c.args, nil)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%q %% %v: err %v, want %q", c.format, c.args, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q %% %v = %q, %v; want %q", c.format, c.args, got, err, c.want)
		}
	}
	got, err := pyPercentFormat("%(a)s-%(b)d", nil, map[string]any{"a": "x", "b": int64(2)})
	if err != nil || got != "x-2" {
		t.Errorf("mapping format = %q, %v", got, err)
	}
}
