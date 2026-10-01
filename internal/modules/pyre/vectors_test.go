package pyre

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The vectors are CPython 3.14's re results (testdata/gen_vectors.py).

type pyExc struct {
	Exc string `json:"exc"`
	Msg string `json:"msg"`
}

type vectors struct {
	Compile []struct {
		Pattern string `json:"pattern"`
		Flags   Flag   `json:"flags"`
		Bytes   bool   `json:"bytes"`
		Result  struct {
			pyExc
			Groups     int            `json:"groups"`
			GroupIndex map[string]int `json:"groupindex"`
			Flags      Flag           `json:"flags"`
		} `json:"result"`
	} `json:"compile"`
	Match []struct {
		Pattern   string  `json:"pattern"`
		Flags     Flag    `json:"flags"`
		Bytes     bool    `json:"bytes"`
		Subject   string  `json:"subject"`
		Search    []int   `json:"search"`
		Match     []int   `json:"match"`
		FullMatch []int   `json:"fullmatch"`
		FindIter  [][]int `json:"finditer"`
		FindAll   []any   `json:"findall"`
		Subs      []struct {
			Repl   string `json:"repl"`
			Count  int    `json:"count"`
			Result string `json:"result"`
			N      int    `json:"n"`
			Error  *pyExc `json:"error"`
		} `json:"subs"`
	} `json:"match"`
	TemplateErrors []struct {
		Pattern string `json:"pattern"`
		Bytes   bool   `json:"bytes"`
		Repl    string `json:"repl"`
		Result  string `json:"result"`
		Error   *pyExc `json:"error"`
	} `json:"template_errors"`
	Split []struct {
		Pattern  string    `json:"pattern"`
		Subject  string    `json:"subject"`
		MaxSplit int       `json:"maxsplit"`
		Result   []*string `json:"result"`
	} `json:"split"`
	Escape []struct {
		S      string `json:"s"`
		Result string `json:"result"`
	} `json:"escape"`
	Pos []struct {
		Pattern   string `json:"pattern"`
		Subject   string `json:"subject"`
		Pos       int    `json:"pos"`
		EndPos    int    `json:"endpos"`
		Search    []int  `json:"search"`
		Match     []int  `json:"match"`
		FullMatch []int  `json:"fullmatch"`
	} `json:"pos"`
	Classes    map[string][][2]rune `json:"classes"`
	IgnoreCase []struct {
		C     string `json:"c"`
		Cands string `json:"cands"`
		Lit   string `json:"lit"`
		Cls   string `json:"cls"`
		ALit  string `json:"alit"`
		BRef  string `json:"bref"`
	} `json:"ignorecase"`
}

func loadVectors(t *testing.T) *vectors {
	t.Helper()
	data, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func excOf(err error) pyExc {
	e := err.(*Error)
	return pyExc{Exc: e.ExcName(), Msg: e.Error()}
}

func TestVectorsCompile(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Compile {
		p, err := compile(c.Pattern, c.Flags, c.Bytes)
		if c.Result.Exc != "" {
			if err == nil {
				t.Errorf("compile(%q): no error, want %s: %s", c.Pattern, c.Result.Exc, c.Result.Msg)
				continue
			}
			if got := excOf(err); got != c.Result.pyExc {
				t.Errorf("compile(%q): %s: %s, want %s: %s", c.Pattern, got.Exc, got.Msg, c.Result.Exc, c.Result.Msg)
			}
			continue
		}
		if err != nil {
			t.Errorf("compile(%q): %v", c.Pattern, err)
			continue
		}
		if p.Groups() != c.Result.Groups || p.Flags() != c.Result.Flags {
			t.Errorf("compile(%q): groups %d flags %d, want %d %d", c.Pattern, p.Groups(), p.Flags(), c.Result.Groups, c.Result.Flags)
		}
		if len(c.Result.GroupIndex) > 0 && !reflect.DeepEqual(p.GroupIndex(), c.Result.GroupIndex) {
			t.Errorf("compile(%q): groupindex %v, want %v", c.Pattern, p.GroupIndex(), c.Result.GroupIndex)
		}
	}
}

func findAllAny(p *Pattern, s string) []any {
	var out []any
	for _, x := range p.FindAll(s) {
		if gs, ok := x.([]string); ok {
			var l []any
			for _, g := range gs {
				l = append(l, g)
			}
			out = append(out, l)
		} else {
			out = append(out, x)
		}
	}
	return out
}

func TestVectorsMatch(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Match {
		p, err := cachedCompile(c.Pattern, c.Flags, c.Bytes)
		if err != nil {
			t.Errorf("compile(%q): %v", c.Pattern, err)
			continue
		}
		name := fmt.Sprintf("%q flags=%d bytes=%v on %q", c.Pattern, c.Flags, c.Bytes, c.Subject)
		if got := p.Search(c.Subject, 0, -1); !reflect.DeepEqual(got, c.Search) {
			t.Errorf("%s: search %v, want %v", name, got, c.Search)
		}
		if got := p.Match(c.Subject, 0, -1); !reflect.DeepEqual(got, c.Match) {
			t.Errorf("%s: match %v, want %v", name, got, c.Match)
		}
		if got := p.FullMatch(c.Subject, 0, -1); !reflect.DeepEqual(got, c.FullMatch) {
			t.Errorf("%s: fullmatch %v, want %v", name, got, c.FullMatch)
		}
		got := p.FindAllSubmatchIndex(c.Subject, -1)
		if len(got) != len(c.FindIter) || len(got) > 0 && !reflect.DeepEqual(got, c.FindIter) {
			t.Errorf("%s: finditer %v, want %v", name, got, c.FindIter)
		}
		if fa := findAllAny(p, c.Subject); len(fa) != len(c.FindAll) || len(fa) > 0 && !reflect.DeepEqual(fa, c.FindAll) {
			t.Errorf("%s: findall %#v, want %#v", name, fa, c.FindAll)
		}
		for _, s := range c.Subs {
			r, n, err := p.Sub(s.Repl, c.Subject, s.Count)
			if s.Error != nil {
				if err == nil || excOf(err) != *s.Error {
					t.Errorf("%s: sub(%q): %v, want %v", name, s.Repl, err, *s.Error)
				}
				continue
			}
			if err != nil || r != s.Result || n != s.N {
				t.Errorf("%s: subn(%q, count=%d) = %q, %d, %v; want %q, %d", name, s.Repl, s.Count, r, n, err, s.Result, s.N)
			}
		}
	}
}

func TestVectorsTemplates(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.TemplateErrors {
		p, err := cachedCompile(c.Pattern, 0, c.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := p.Sub(c.Repl, "xab", 0)
		if c.Error != nil {
			if err == nil || excOf(err) != *c.Error {
				t.Errorf("sub(%q, %q): %v, want %v", c.Pattern, c.Repl, err, *c.Error)
			}
			continue
		}
		if err != nil || r != c.Result {
			t.Errorf("sub(%q, %q) = %q, %v; want %q", c.Pattern, c.Repl, r, err, c.Result)
		}
	}
}

func TestVectorsSplitEscapePos(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Split {
		got := MustCompile(c.Pattern, 0).Split(c.Subject, c.MaxSplit)
		if !reflect.DeepEqual(got, c.Result) {
			show := func(l []*string) string {
				var parts []string
				for _, s := range l {
					if s == nil {
						parts = append(parts, "None")
					} else {
						parts = append(parts, fmt.Sprintf("%q", *s))
					}
				}
				return "[" + strings.Join(parts, " ") + "]"
			}
			t.Errorf("split(%q, %q, %d) = %s, want %s", c.Pattern, c.Subject, c.MaxSplit, show(got), show(c.Result))
		}
	}
	for _, c := range v.Escape {
		if got := Escape(c.S); got != c.Result {
			t.Errorf("escape(%q) = %q, want %q", c.S, got, c.Result)
		}
	}
	for _, c := range v.Pos {
		p := MustCompile(c.Pattern, 0)
		name := fmt.Sprintf("%q on %q [%d:%d]", c.Pattern, c.Subject, c.Pos, c.EndPos)
		if got := p.Search(c.Subject, c.Pos, c.EndPos); !reflect.DeepEqual(got, c.Search) {
			t.Errorf("%s: search %v, want %v", name, got, c.Search)
		}
		if got := p.Match(c.Subject, c.Pos, c.EndPos); !reflect.DeepEqual(got, c.Match) {
			t.Errorf("%s: match %v, want %v", name, got, c.Match)
		}
		if got := p.FullMatch(c.Subject, c.Pos, c.EndPos); !reflect.DeepEqual(got, c.FullMatch) {
			t.Errorf("%s: fullmatch %v, want %v", name, got, c.FullMatch)
		}
	}
}

func TestVectorsClasses(t *testing.T) {
	v := loadVectors(t)
	pats := map[string]string{"w": `\w`, "d": `\d`, "s": `\s`, "aw": `(?a)\w`, "ad": `(?a)\d`, "as": `(?a)\s`, "dot": `.`}
	for name, want := range v.Classes {
		p := MustCompile(pats[name], 0)
		u := p.prog.insts[0].u
		var got [][2]rune
		start := rune(-1)
		for cp := rune(0); cp <= 0x10ffff; cp++ {
			ok := cp >= 0xd800 && cp <= 0xdfff || u.match(cp)
			if ok && start < 0 {
				start = cp
			} else if !ok && start >= 0 {
				got = append(got, [2]rune{start, cp - 1})
				start = -1
			}
		}
		if start >= 0 {
			got = append(got, [2]rune{start, 0x10ffff})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("class %s: %d ranges, want %d", name, len(got), len(want))
		}
	}
}

func TestVectorsIgnoreCase(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.IgnoreCase {
		lit := MustCompile("(?i)"+Escape(c.C), 0)
		cls := MustCompile("(?i)["+Escape(c.C)+"]", 0)
		alit := MustCompile("(?ai)"+Escape(c.C), 0)
		bref := MustCompile("(?i)("+Escape(c.C)+")\\1", 0)
		var gl, gc, ga, gb strings.Builder
		for _, x := range c.Cands {
			xs := string(x)
			if lit.FullMatch(xs, 0, -1) != nil {
				gl.WriteRune(x)
			}
			if cls.FullMatch(xs, 0, -1) != nil {
				gc.WriteRune(x)
			}
			if alit.FullMatch(xs, 0, -1) != nil {
				ga.WriteRune(x)
			}
			if bref.FullMatch(c.C+xs, 0, -1) != nil {
				gb.WriteRune(x)
			}
		}
		if gl.String() != c.Lit || gc.String() != c.Cls || ga.String() != c.ALit || gb.String() != c.BRef {
			t.Errorf("ignorecase %q (%U) over %q: lit %q cls %q alit %q bref %q; want %q %q %q %q",
				c.C, []rune(c.C)[0], c.Cands, gl.String(), gc.String(), ga.String(), gb.String(), c.Lit, c.Cls, c.ALit, c.BRef)
		}
	}
}
