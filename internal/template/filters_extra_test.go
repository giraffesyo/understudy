package template

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// TestFiltersMatchAnsible renders templates over the filters and tests
// added after Jinja2's and ansible-core's first ones and holds them to
// what ansible-core's templar renders (testdata/filters.json, from
// testdata/filters_oracle.py): the value, or the error.
func TestFiltersMatchAnsible(t *testing.T) {
	data, err := os.ReadFile("testdata/filters.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := omap.UnmarshalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	root := doc.(*yaml.OMap)
	vars := MapVars{}
	vm := root.Get("vars").(*yaml.OMap)
	for _, k := range vm.Keys() {
		vars[k] = vm.Get(k)
	}
	e := New()
	for _, c := range root.Get("cases").([]any) {
		cm := c.(*yaml.OMap)
		tpl := cm.Get("template").(string)
		got, err := e.RenderTemplate(tpl, vars, Position{})
		if want, ok := cm.GetItem("error"); ok {
			msg := ""
			if err != nil {
				msg, _ = Cause(err)
				msg = strings.TrimPrefix(msg, "Error rendering template: ")
			}
			if msg != want {
				t.Errorf("%s:\n got %v, %q\nwant error %q", tpl, got, msg, want)
			}
			continue
		}
		want := PyJSON(cm.Get("value"), 0, false, false)
		if err != nil {
			t.Errorf("%s:\n got error %v\nwant %s", tpl, err, want)
			continue
		}
		if g := PyJSON(got, 0, false, false); g != want {
			t.Errorf("%s:\n got %s\nwant %s", tpl, g, want)
		}
	}
}

// TestEveryBuiltinPlugin holds the registries to every ansible.builtin
// filter and test (testdata/plugin_names.json, from
// testdata/plugin_names.py), each with its Python signature.
func TestEveryBuiltinPlugin(t *testing.T) {
	data, err := os.ReadFile("testdata/plugin_names.json")
	if err != nil {
		t.Fatal(err)
	}
	var names struct{ Filters, Tests []string }
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatal(err)
	}
	e := New()
	for _, n := range names.Filters {
		if _, ok := e.Filters[n]; !ok {
			t.Errorf("no filter %s", n)
		}
		if _, ok := filterSignatures[n]; !ok {
			t.Errorf("no signature for the filter %s", n)
		}
	}
	for _, n := range names.Tests {
		if _, ok := e.Tests[n]; !ok {
			t.Errorf("no test %s", n)
		}
		if _, ok := testSignatures[n]; !ok {
			t.Errorf("no signature for the test %s", n)
		}
	}
}
