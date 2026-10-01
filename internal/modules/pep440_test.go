package modules

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestPackagingGroundTruth holds pep440.go / pep508.go to what real
// packaging releases answer (testdata/packaging_ground_truth.json, made
// by testdata/packaging_probe.py).
func TestPackagingGroundTruth(t *testing.T) {
	data, err := os.ReadFile("testdata/packaging_ground_truth.json")
	if err != nil {
		t.Fatal(err)
	}
	var gt struct {
		Reqs     [][]json.RawMessage `json:"reqs"`
		Contains [][]json.RawMessage `json:"contains"`
	}
	if err := json.Unmarshal(data, &gt); err != nil {
		t.Fatal(err)
	}
	type group struct {
		value    any
		versions []string
	}
	groups := func(raw json.RawMessage) []group {
		var gs [][]json.RawMessage
		if err := json.Unmarshal(raw, &gs); err != nil {
			t.Fatal(err)
		}
		var out []group
		for _, g := range gs {
			var v any
			var vs []string
			json.Unmarshal(g[0], &v)
			json.Unmarshal(g[1], &vs)
			out = append(out, group{v, vs})
		}
		return out
	}
	for _, c := range gt.Reqs {
		var in string
		json.Unmarshal(c[0], &in)
		for _, g := range groups(c[1]) {
			for _, version := range g.versions {
				r, ok := parseRequirement(in, pkgFlavorOf(version))
				got := []any{ok}
				if ok {
					got = append(got, r.String(), canonicalizeName(r.name), r.hasSpecifier())
				}
				if fmt.Sprint(got) != fmt.Sprint(g.value) {
					t.Errorf("packaging %s: Requirement(%q) = %v, want %v", version, in, got, g.value)
				}
			}
		}
	}
	t.Logf("%d requirement and %d contains cases", len(gt.Reqs), len(gt.Contains))
	for _, c := range gt.Contains {
		var spec, item string
		json.Unmarshal(c[0], &spec)
		json.Unmarshal(c[1], &item)
		for _, g := range groups(c[2]) {
			for _, version := range g.versions {
				f := pkgFlavorOf(version)
				var got any = "invalid-req"
				if r, ok := parseRequirement("x"+spec, f); ok {
					in, err := r.spec.contains(item)
					switch {
					case err != nil:
						got = "raise:InvalidVersion"
					default:
						got = in
					}
				}
				if fmt.Sprint(got) != fmt.Sprint(g.value) {
					t.Errorf("packaging %s: SpecifierSet(%q).contains(%q) = %v, want %v", version, spec, item, got, g.value)
				}
			}
		}
	}
}
