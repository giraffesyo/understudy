package pyre

import (
	"regexp"
	"strings"
	"testing"
)

func sub(re *regexp.Regexp, repl, s string) (string, int) {
	var b strings.Builder
	last := 0
	ms := FindAllSubmatchIndex(re, s, -1)
	for _, m := range ms {
		b.WriteString(s[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), len(ms)
}

// Expected values are CPython 3.14's re.subn(pattern, '-', s).
func TestPythonSubSemantics(t *testing.T) {
	cases := []struct {
		pattern, s, want string
		n                int
	}{
		{`x*`, "abxd", "-a-b--d-", 5},
		{`\s*$`, "ab  ", "ab--", 2},
		{`\b`, "ab cd", "-ab- -cd-", 4},
		{`(?m)^`, "a\nb", "-a\n-b", 2},
		{`(?m)$`, "a\nb\n", "a-\nb-\n-", 3},
		{`a*`, "baaac", "-b--c-", 4},
		{``, "abc", "-a-b-c-", 4},
		{`b*`, "abc", "-a--c-", 4},
		{`\d*`, "a12b", "-a--b-", 4},
		{`(?m)^\s*`, "  a\n  b", "-a\n-b", 2},
		{`é*`, "aéb", "-a--b-", 4},
		{`(a)|b*`, "xab", "-x---", 4},
		{`^`, "abc", "-abc", 1},
		{`\Bb`, "abc", "a-c", 1},
		{`o`, "foo", "f--", 2},
	}
	for _, c := range cases {
		re := regexp.MustCompile(c.pattern)
		got, n := sub(re, "-", c.s)
		if got != c.want || n != c.n {
			t.Errorf("sub(%q, %q) = %q, %d; want %q, %d", c.pattern, c.s, got, n, c.want, c.n)
		}
	}
}

func TestLimit(t *testing.T) {
	re := regexp.MustCompile(`x*`)
	if got := FindAllSubmatchIndex(re, "abxd", 3); len(got) != 3 || got[2][0] != 2 || got[2][1] != 3 {
		t.Fatalf("limited matches = %v", got)
	}
}
