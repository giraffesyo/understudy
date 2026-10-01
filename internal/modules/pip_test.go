package modules

import (
	"reflect"
	"testing"
)

// _recover_package_name's docstring examples, and a bracketed extras
// list split over items.
func TestPipRecoverPackageNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"django>1.11.1", "<1.11.3", "ipaddress", "simpleproject>1.1.0", "<2.0.0"},
			[]string{"django>1.11.1,<1.11.3", "ipaddress", "simpleproject>1.1.0,<2.0.0"}},
		{[]string{"django>1.11.1,<1.11.3,ipaddress", "simpleproject>1.1.0,<2.0.0"},
			[]string{"django>1.11.1,<1.11.3", "ipaddress", "simpleproject>1.1.0,<2.0.0"}},
		{[]string{"x[a", "b]>1", "<3"}, []string{"x[a,b]>1,<3"}},
	}
	for _, c := range cases {
		if got := pipRecoverPackageNames(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("pipRecoverPackageNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// _is_venv_command: pyvenv, or -m venv as argparse reads it.
func TestPipIsVenvCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"pyvenv":                   true,
		"python3 -m venv":          true,
		"python3 -mvenv":           true,
		"python3 -m=venv":          true,
		"/usr/bin/python3 -m venv": true,
		"python3 -m virtualenv":    false,
		"virtualenv":               false,
		"python3 -- -m venv":       false,
		"python3 -m venv -m other": false,
	} {
		if got := pipIsVenvCommand(cmd); got != want {
			t.Errorf("pipIsVenvCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// int(umask, 8).
func TestPyIntBase8(t *testing.T) {
	for in, want := range map[string]int64{"022": 18, " 0o0_22 ": 18, "+7": 7, "0O17": 15, "1_0": 8} {
		if got, ok := pyIntBase8(in); !ok || got != want {
			t.Errorf("pyIntBase8(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "8", "0o8", "_1", "1_", "1__0", "abc", "0x1"} {
		if _, ok := pyIntBase8(in); ok {
			t.Errorf("pyIntBase8(%q) accepted", in)
		}
	}
}
