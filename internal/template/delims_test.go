package template

import "testing"

// Expected outputs were produced by ansible-core 2.21's template module
// (Jinja2 3.1) with the same environment overrides.
func TestRenderFileOverrides(t *testing.T) {
	vars := MapVars{"name": "world", "flag": true}
	custom := DefaultOptions()
	custom.BlockStart, custom.BlockEnd = "[%", "%]"
	custom.VariableStart, custom.VariableEnd = "[[", "]]"
	custom.CommentStart, custom.CommentEnd = "<#", "#>"
	custom.NewlineSequence = "\n"
	noTrim := DefaultOptions()
	noTrim.TrimBlocks, noTrim.LstripBlocks = false, true
	crlf := DefaultOptions()
	crlf.NewlineSequence = "\r\n"
	lstrip := DefaultOptions()
	lstrip.LstripBlocks = true
	cases := []struct {
		name, src string
		opts      Options
		want      string
	}{
		{"custom", "[% if flag %]\n  on: [[ name ]] {{ literal }} {% raw %}\n[% endif %]\n<# comment #>\n  [% for i in [1,2] %]\n    item [[ i ]]\n  [%- endfor %]\n[% raw %][[ keep ]][% endraw %]\ntail\n",
			custom, "  on: world {{ literal }} {% raw %}\n      item 1    item 2[[ keep ]]tail\n"},
		{"trim off, lstrip on", "{% if flag %}\n    {% if flag %}\n  x={{ name }}\n    {% endif %}\n{% endif %}\nend\n",
			noTrim, "\n\n  x=world\n\n\nend\n"},
		{"crlf", "crlf\r\nline {{ name }}\r\n", crlf, "crlf\r\nline world\r\n"},
		{"comments trim", "a\n{# c #}\nb\n    {# indented #}\nc {# inline #} d\n", DefaultOptions(), "a\nb\n    c  d\n"},
		{"comments lstrip", "a\n{# c #}\nb\n    {# indented #}\nc {# inline #} d\n", lstrip, "a\nb\nc  d\n"},
	}
	for _, c := range cases {
		got, err := New().WithOptions(c.opts).RenderFile(c.src, vars, Position{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}
