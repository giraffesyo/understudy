package yaml

import (
	"reflect"
	"testing"
)

// Round-trip: Marshal output must re-parse to the same value.
func TestMarshalRoundTrip(t *testing.T) {
	cases := []any{
		nil,
		true,
		int64(42),
		1.5,
		"plain",
		"needs: quoting",
		"0644",
		"yes",
		"multi\nline",
		"- leading dash",
		"trailing space ",
		[]any{int64(1), "two", nil},
		map[string]any{"b": int64(2), "a": "one"},
		map[string]any{
			"name": "web",
			"vars": map[string]any{"port": int64(80), "opts": []any{"a", "b"}},
			"list": []any{
				map[string]any{"x": int64(1)},
				[]any{int64(1), int64(2)},
			},
			"empty_list": []any{},
			"empty_map":  map[string]any{},
		},
	}
	for _, v := range cases {
		out, err := Marshal(v, 2)
		if err != nil {
			t.Fatalf("Marshal(%#v): %v", v, err)
		}
		back, err := Unmarshal(out, "roundtrip.yml")
		if err != nil {
			t.Fatalf("re-parse of %q (from %#v): %v", out, v, err)
		}
		back = AsMap(back) // normalize *OMap -> map for value comparison
		if !reflect.DeepEqual(back, v) {
			t.Errorf("round trip failed:\n  in:   %#v\n  yaml: %q\n  out:  %#v", v, out, back)
		}
	}
}

func TestMarshalShape(t *testing.T) {
	out, _ := Marshal(map[string]any{"a": int64(1), "b": []any{"x"}}, 2)
	want := "a: 1\nb:\n  - x\n"
	if string(out) != want {
		t.Errorf("Marshal = %q, want %q", out, want)
	}
}
