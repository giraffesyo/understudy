package pyre

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

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
		// After an empty match, a non-empty one at the same position is
		// found by backtracking into a lower-priority alternative.
		{`|a`, "aa", "-----", 5},
		{`(?=a)|a`, "aa", "----", 4},
	}
	for _, c := range cases {
		got, n, err := MustCompile(c.pattern, 0).Sub("-", c.s, 0)
		if err != nil || got != c.want || n != c.n {
			t.Errorf("sub(%q, %q) = %q, %d, %v; want %q, %d", c.pattern, c.s, got, n, err, c.want, c.n)
		}
	}
}

func TestLimit(t *testing.T) {
	if got := MustCompile(`x*`, 0).FindAllSubmatchIndex("abxd", 3); len(got) != 3 || got[2][0] != 2 || got[2][1] != 3 {
		t.Fatalf("limited matches = %v", got)
	}
}

func TestByteOffsets(t *testing.T) {
	p := MustCompile(`(é)(\w)`, 0)
	s := "aébc日é本"
	if got, want := p.FindAllSubmatchIndex(s, -1), [][]int{{1, 4, 1, 3, 3, 4}, {8, 13, 8, 10, 10, 13}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans = %v, want %v", got, want)
	}
	// pos/endpos are byte offsets too.
	if got := MustCompile(`\w`, 0).Search(s, 3, -1); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Fatalf("search from 3 = %v", got)
	}
	// Invalid UTF-8 bytes are single code points (U+FFFD).
	if got := MustCompile(`.b`, 0).Search("a\xffb", 0, -1); !reflect.DeepEqual(got, []int{1, 3}) {
		t.Fatalf("invalid utf-8 = %v", got)
	}
}

func TestErrorFormat(t *testing.T) {
	_, err := Compile("a\nb\n(", 0)
	if err == nil || err.Error() != "missing ), unterminated subpattern at position 4 (line 3, column 1)" {
		t.Fatalf("err = %v", err)
	}
	e := err.(*Error)
	if e.Msg != "missing ), unterminated subpattern" || e.Pos != 4 || e.ExcName() != "PatternError" {
		t.Fatalf("err = %#v", e)
	}
	if _, err := Compile(`(?<=a+)`, 0); err == nil || err.Error() != "look-behind requires fixed-width pattern" {
		t.Fatalf("err = %v", err)
	}
	if _, err := Compile(`a{4294967295}`, 0); err == nil || err.(*Error).ExcName() != "OverflowError" {
		t.Fatalf("err = %v", err)
	}
}

func TestNames(t *testing.T) {
	for name, want := range map[string]rune{
		"EM DASH": '—', "latin small letter a": 'a', "CJK UNIFIED IDEOGRAPH-4E2D": '中',
		"HANGUL SYLLABLE GAG": '각', "HANGUL SYLLABLE A": '아', "LINE FEED": '\n',
	} {
		p, err := Compile(`\N{`+name+`}`, 0)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.FullMatch(string(want), 0, -1) == nil {
			t.Errorf("%s does not match %q", name, want)
		}
	}
}

func TestLongInputs(t *testing.T) {
	s := strings.Repeat("ab", 1<<19) // 1 MiB
	for _, pat := range []string{`(a|b)*`, `(?:a|b)*`, `(?:ab)*?$`, `(a|b)*?$`, `(?>a|b)*`, `(?:a|b)++`, `.*`, `(.)*`} {
		m := MustCompile(pat, 0).Match(s, 0, -1)
		if m == nil || m[1] != len(s) {
			t.Errorf("%s: %v", pat, m[:2])
		}
	}
}

func TestBacktrackingBound(t *testing.T) {
	// Exponential in CPython as well; a short subject finishes.
	start := time.Now()
	if MustCompile(`(x+x+)+y`, 0).Search(strings.Repeat("x", 16), 0, -1) != nil {
		t.Fatal("matched")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestConcurrent(t *testing.T) {
	p := MustCompile(`(\w+)@(\w+)\.com`, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if out, _, _ := p.Sub(`\2/\1`, "x a@b.com y c@d.com", 0); out != "x b/a y d/c" {
					t.Errorf("got %q", out)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestAPI(t *testing.T) {
	p := MustCompile(`(?P<a>x)(y)(?P<c>z)?`, IGNORECASE)
	if p.Groups() != 3 || p.SubexpIndex("c") != 3 || p.SubexpIndex("y") != -1 || p.Flags() != IGNORECASE|UNICODE {
		t.Fatalf("groups %d flags %d", p.Groups(), p.Flags())
	}
	if got := p.SubexpNames(); !reflect.DeepEqual(got, []string{"", "a", "", "c"}) {
		t.Fatalf("names %q", got)
	}
	if got := p.FindAll("xy XYZ"); !reflect.DeepEqual(got, []any{[]string{"x", "y", ""}, []string{"X", "Y", "Z"}}) {
		t.Fatalf("findall %#v", got)
	}
	parts, err := ParseTemplate(p, `<\g<c>\1>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ExpandTemplate(parts, "xy", p.Search("xy", 0, -1)); got != "<x>" {
		t.Fatalf("expand %q", got)
	}
	if _, err := ParseTemplate(p, `\g<nope>`); err == nil || err.Error() != "unknown group name 'nope'" || err.(*Error).ExcName() != "IndexError" {
		t.Fatalf("err %v", err)
	}
}
