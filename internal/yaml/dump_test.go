package yaml

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// dumpCase is one entry of testdata/dump.json (see dump_oracle.py): what
// ansible-core 2.21's to_yaml/to_nice_yaml filters (PyYAML 6 over libyaml
// 0.2.5) produce for the input.
type dumpCase struct {
	Name   string         `json:"name"`
	Filter string         `json:"filter"`
	Kwargs map[string]any `json:"kwargs"`
	In     json.RawMessage
	Out    string `json:"out"`
}

// taggedValue is the corpus' typed encoding of a Python value; "id" marks
// a container referenced more than once (a "ref" points back at it).
type taggedValue struct {
	T  string          `json:"t"`
	V  json.RawMessage `json:"v"`
	ID int             `json:"id"`
}

func decodeTagged(t *testing.T, raw json.RawMessage, refs map[int]any) any {
	var tv taggedValue
	if err := json.Unmarshal(raw, &tv); err != nil {
		t.Fatal(err)
	}
	switch tv.T {
	case "none":
		return nil
	case "bool":
		var b bool
		_ = json.Unmarshal(tv.V, &b)
		return b
	case "int", "float", "str":
		var s string
		_ = json.Unmarshal(tv.V, &s)
		switch tv.T {
		case "int":
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		case "float":
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
		return s
	case "ref":
		return refs[tv.ID]
	case "list":
		var items []json.RawMessage
		_ = json.Unmarshal(tv.V, &items)
		out := make([]any, len(items), max(len(items), 1)) // an identity even when empty
		if tv.ID != 0 {
			refs[tv.ID] = out
		}
		for i, item := range items {
			out[i] = decodeTagged(t, item, refs)
		}
		return out
	case "dict":
		var pairs [][2]json.RawMessage
		_ = json.Unmarshal(tv.V, &pairs)
		m := NewOMap()
		if tv.ID != 0 {
			refs[tv.ID] = m
		}
		for _, p := range pairs {
			var k string
			_ = json.Unmarshal(p[0], &k)
			m.Set(k, decodeTagged(t, p[1], refs))
		}
		return m
	}
	t.Fatalf("unknown tagged type %q", tv.T)
	return nil
}

// dumpOptionsFor mirrors the filters: to_yaml is yaml.dump(allow_unicode=
// True, default_flow_style=None); to_nice_yaml adds indent=4 and
// default_flow_style=False.
func dumpOptionsFor(t *testing.T, filter string, kwargs map[string]any) DumpOptions {
	opts := DumpOptions{SortKeys: true, AllowUnicode: true}
	if filter == "to_nice_yaml" {
		opts.Indent = 4
		f := false
		opts.DefaultFlowStyle = &f
	}
	for k, v := range kwargs {
		switch k {
		case "indent":
			opts.Indent = int(v.(float64))
		case "width":
			opts.Width = int(v.(float64))
		case "sort_keys":
			opts.SortKeys = v.(bool)
		case "default_flow_style":
			b := v.(bool)
			opts.DefaultFlowStyle = &b
		case "explicit_start":
			opts.ExplicitStart = v.(bool)
		case "explicit_end":
			opts.ExplicitEnd = v.(bool)
		case "canonical":
			opts.Canonical = v.(bool)
		case "default_style":
			opts.DefaultStyle = v.(string)[0]
		case "line_break":
			opts.LineBreak = v.(string)
		default:
			t.Fatalf("unhandled kwarg %q", k)
		}
	}
	return opts
}

func TestDumpMatchesPyYAML(t *testing.T) {
	data, err := os.ReadFile("testdata/dump.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []dumpCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range cases {
		in := decodeTagged(t, c.In, map[int]any{})
		got, err := Dump(in, dumpOptionsFor(t, c.Filter, c.Kwargs))
		if err != nil {
			t.Errorf("%s (%s %v): %v", c.Name, c.Filter, c.Kwargs, err)
			continue
		}
		if got != c.Out {
			if failed++; failed <= 25 {
				t.Errorf("%s (%s %v):\n  want %q\n  got  %q", c.Name, c.Filter, c.Kwargs, c.Out, got)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d cases differ", failed, len(cases))
	}
}
