package template

import (
	"encoding/json"
	"os"
	"testing"
)

// TestJinjaSyntaxErrors compiles each template of testdata/jinja_syntax.json
// (generated with Jinja2 3.1: an Environment with trim_blocks, no
// extensions, the AnsibleLexer's backslash escaping, and the jinja
// builtin filters and tests plus "combine" and "version") and expects
// Jinja's TemplateSyntaxError: message, line and the exception it was
// raised from.
func TestJinjaSyntaxErrors(t *testing.T) {
	data, err := os.ReadFile("testdata/jinja_syntax.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Src  string
		Expr bool
		Want *struct {
			Msg   string
			Line  int
			Cause string
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	e := New()
	jinjaFilters := map[string]bool{"upper": true, "default": true, "combine": true}
	e.Filters = map[string]FilterFunc{}
	for name := range jinjaFilters {
		e.Filters[name] = nil
	}
	e.Tests = map[string]TestFunc{"defined": nil, "version": nil}
	opts := DefaultOptions()
	for _, c := range cases {
		got := e.jinjaCompile(c.Src, opts, c.Expr, !c.Expr)
		switch {
		case c.Want == nil && got != nil:
			t.Errorf("%q (expr=%v): unexpected error %q (line %d)", c.Src, c.Expr, got.msg, got.line)
		case c.Want != nil && got == nil:
			t.Errorf("%q (expr=%v): no error, want %q", c.Src, c.Expr, c.Want.Msg)
		case c.Want != nil && (got.msg != c.Want.Msg || got.line != c.Want.Line || got.cause != c.Want.Cause):
			t.Errorf("%q (expr=%v):\n got %q line %d cause %q\nwant %q line %d cause %q", c.Src, c.Expr,
				got.msg, got.line, got.cause, c.Want.Msg, c.Want.Line, c.Want.Cause)
		}
	}
}
