package inventory

import (
	"reflect"
	"testing"

	"github.com/giraffesyo/understudy/internal/yaml"
)

func TestShlexSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  string
	}{
		{`h1 a=1 b="two words" c='x y'`, []string{"h1", "a=1", "b=two words", "c=x y"}, ""},
		{`h1 a=1 # trailing comment`, []string{"h1", "a=1"}, ""},
		{`h1 a=b#c`, []string{"h1", "a=b"}, ""},
		{`h1 e=""`, []string{"h1", "e="}, ""},
		{`h1 p="a\"b" q='a\b' r=a\ b`, []string{"h1", `p=a"b`, `q=a\b`, "r=a b"}, ""},
		{`h1 m="unterminated`, nil, "No closing quotation"},
		{`h1 x\`, nil, "No escaped character"},
	}
	for _, c := range cases {
		got, err := shlexSplit(c.in)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("shlexSplit(%q) error = %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("shlexSplit(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestLiteralEval(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"1", int64(1)},
		{"-5", int64(-5)},
		{"0x1F", int64(31)},
		{"1_000", int64(1000)},
		{"1.5", 1.5},
		{"1e3", 1000.0},
		{".5", 0.5},
		{"True", true},
		{"None", nil},
		{"'single'", "single"},
		{`"a" 'b'`, "ab"},
		{`"tab\tx"`, "tab\tx"},
		{"[1, 'a', [2]]", []any{int64(1), "a", []any{int64(2)}}},
		{"(1, 2)", []any{int64(1), int64(2)}},
		{"1, 2", []any{int64(1), int64(2)}},
		{"()", []any{}},
		{"set()", []any{}},
		{"1j", "1j"},
		{"1+2j", "(1+2j)"},
		{"...", "..."},
		{"5 # comment", int64(5)},
	}
	for _, c := range cases {
		got, ok := literalEval(c.in)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("literalEval(%q) = %#v, %v; want %#v", c.in, got, ok, c.want)
		}
	}
	d, ok := literalEval("{'b': 1, 'a': [2]}")
	if om, isOM := d.(*yaml.OMap); !ok || !isOM || !reflect.DeepEqual(om.Keys(), []string{"b", "a"}) {
		t.Errorf("dict literal = %#v", d)
	}
	for _, bad := range []string{"yes", "", "hello world", "01", "1 + 2", "--5", "f'x'", "{k: v}", "[1,", "None2", "{[1]: 2}", "1x", "x=1"} {
		if v, ok := literalEval(bad); ok {
			t.Errorf("literalEval(%q) = %#v, want a failure", bad, v)
		}
	}
}

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
		ok   bool
	}{
		{"web1", "web1", -1, true},
		{"web1:2222", "web1", 2222, true},
		{"10.0.0.1:22", "10.0.0.1", 22, true},
		{"[::1]:22", "::1", 22, true},
		{"::1", "::1", -1, true},
		{"web[1:3].example.com", "web[1:3].example.com", -1, true},
		{"all:", "", -1, false},
		{"bad host", "", -1, false},
		{"under_", "", -1, false},
		{"_ok", "_ok", -1, true},
	}
	for _, c := range cases {
		host, port, err := parseAddress(c.in, true)
		if (err == nil) != c.ok || host != c.host || port != c.port {
			t.Errorf("parseAddress(%q) = %q, %d, %v", c.in, host, port, err)
		}
	}
	if _, _, err := parseAddress("w[1:2]", false); err == nil {
		t.Error("ranges must be rejected when not allowed")
	}
}
