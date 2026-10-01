package pyre

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The vectors are CPython 3.14's fnmatch results (testdata/gen_fnmatch.py).
func TestFnmatchVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/fnmatch.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Names []string `json:"names"`
		Cases []struct {
			Pattern   string   `json:"pattern"`
			Translate string   `json:"translate"`
			Matches   []string `json:"matches"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		if got := FnmatchTranslate(c.Pattern); got != c.Translate {
			t.Errorf("translate(%q) = %q, want %q", c.Pattern, got, c.Translate)
			continue
		}
		var got []string
		for _, n := range v.Names {
			if Fnmatch(n, c.Pattern) {
				got = append(got, n)
			}
		}
		if len(got) == 0 && len(c.Matches) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.Matches) {
			t.Errorf("fnmatchcase(*, %q) matched %q, want %q", c.Pattern, got, c.Matches)
		}
	}
}
