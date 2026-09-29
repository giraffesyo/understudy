package modules

import (
	"regexp"
	"testing"
)

// Expected messages are CPython 3.14's re.error texts.
func TestPyRegexSyntaxError(t *testing.T) {
	cases := map[string]string{
		`(`:              "missing ), unterminated subpattern at position 0",
		`((a)`:           "missing ), unterminated subpattern at position 0",
		`a(b(c`:          "missing ), unterminated subpattern at position 3",
		`[a`:             "unterminated character set at position 0",
		`*a`:             "nothing to repeat at position 0",
		`a**`:            "multiple repeat at position 2",
		`a)`:             "unbalanced parenthesis at position 1",
		`\`:              "bad escape (end of pattern) at position 0",
		`a{2,1}`:         "min repeat greater than max repeat at position 2",
		`(?P<1>a)`:       "bad character in group name '1' at position 4",
		`(?<x`:           "unknown extension ?<x at position 1",
		`\q`:             "bad escape \\q at position 0",
		`a*?+`:           "multiple repeat at position 3",
		`(?z)`:           "unknown extension ?z at position 1",
		`[z-a]`:          "bad character range z-a at position 1",
		`^*`:             "nothing to repeat at position 1",
		`a++`:            "",
		`a*?`:            "",
		`(?i)abc`:        "",
		`[]a]`:           "",
		`a{,3}`:          "",
		`x{a}`:           "",
		`(?P<n>a)(?P=n)`: "",
		`^\s*#`:          "",
	}
	for pat, want := range cases {
		if got := pyRegexSyntaxError(pat); got != want {
			t.Errorf("pyRegexSyntaxError(%q) = %q, want %q", pat, got, want)
		}
	}
}

func TestParsePyTemplate(t *testing.T) {
	re := regexp.MustCompile(`(?P<n>a)`)
	cases := map[string]string{
		`\9`:     "invalid group reference 9 at position 1",
		`x\12`:   "invalid group reference 12 at position 2",
		`\g<x>`:  "unknown group name 'x'",
		`\g<1`:   "missing >, unterminated name at position 3",
		`\q`:     "bad escape \\q at position 0",
		`\`:      "bad escape (end of pattern) at position 0",
		`\g<9>`:  "invalid group reference 9 at position 3",
		`\g<-1>`: "bad character in group name '-1' at position 3",
		`\g<>`:   "missing group name at position 3",
	}
	for repl, want := range cases {
		_, err := parsePyTemplate(re, repl)
		if err == nil || err.Error() != want {
			t.Errorf("parsePyTemplate(%q) = %v, want %q", repl, err, want)
		}
	}
	out, _, err := pySubn(re, `[\1|\g<n>|\g<0>]\n\-`, "xax")
	if err != nil || out != "x[a|a|a]\n\\-x" {
		t.Errorf("pySubn = %q, %v", out, err)
	}
}
