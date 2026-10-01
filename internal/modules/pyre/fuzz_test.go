package pyre

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// Random patterns and subjects (gen_vectors.py's fuzz section).
func TestVectorsFuzz(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Fuzz []struct {
			Pattern   string  `json:"pattern"`
			Flags     Flag    `json:"flags"`
			Error     *pyExc  `json:"error"`
			Subject   string  `json:"subject"`
			Search    []int   `json:"search"`
			FullMatch []int   `json:"fullmatch"`
			FindIter  [][]int `json:"finditer"`
			Sub       string  `json:"sub"`
			Repl      string  `json:"repl"`
		} `json:"fuzz"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	fails := 0
	for _, c := range v.Fuzz {
		if fails > 30 {
			t.Fatal("too many failures")
		}
		p, err := Compile(c.Pattern, c.Flags)
		if c.Error != nil {
			if err == nil || excOf(err) != *c.Error {
				t.Errorf("compile(%q): %v, want %v", c.Pattern, err, *c.Error)
				fails++
			}
			continue
		}
		if err != nil {
			t.Errorf("compile(%q): %v", c.Pattern, err)
			fails++
			continue
		}
		name := fmt.Sprintf("%q flags=%d on %q", c.Pattern, c.Flags, c.Subject)
		if got := p.Search(c.Subject, 0, -1); !reflect.DeepEqual(got, c.Search) {
			t.Errorf("%s: search %v, want %v", name, got, c.Search)
			fails++
		}
		if got := p.FullMatch(c.Subject, 0, -1); !reflect.DeepEqual(got, c.FullMatch) {
			t.Errorf("%s: fullmatch %v, want %v", name, got, c.FullMatch)
			fails++
		}
		if got := p.FindAllSubmatchIndex(c.Subject, -1); len(got) != len(c.FindIter) || len(got) > 0 && !reflect.DeepEqual(got, c.FindIter) {
			t.Errorf("%s: finditer %v, want %v", name, got, c.FindIter)
			fails++
		}
		if got, _, err := p.Sub(c.Repl, c.Subject, 0); err != nil || got != c.Sub {
			t.Errorf("%s: sub(%q) = %q, %v; want %q", name, c.Repl, got, err, c.Sub)
			fails++
		}
	}
	t.Logf("%d fuzz cases", len(v.Fuzz))
}
