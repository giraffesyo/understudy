package omap

import (
	"math/big"
	"testing"
)

// TestUnmarshalJSONErrors expects CPython 3.14's json.loads errors.
func TestUnmarshalJSONErrors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nope", "Expecting value: line 1 column 1 (char 0)"},
		{"{\"a\": 1", "Expecting ',' delimiter: line 1 column 8 (char 7)"},
		{"[1, 2", "Expecting ',' delimiter: line 1 column 6 (char 5)"},
		{"", "Expecting value: line 1 column 1 (char 0)"},
		{"{\"a\": 1}x", "Extra data: line 1 column 9 (char 8)"},
		{"{'a': 1}", "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"},
		{"{\"a\" 1}", "Expecting ':' delimiter: line 1 column 6 (char 5)"},
		{"{\"a\": 1,}", "Illegal trailing comma before end of object: line 1 column 8 (char 7)"},
		{"[1,]", "Illegal trailing comma before end of array: line 1 column 3 (char 2)"},
		{"[1 2]", "Expecting ',' delimiter: line 1 column 4 (char 3)"},
		{"\"abc", "Unterminated string starting at: line 1 column 1 (char 0)"},
		{"\"a\\qb\"", "Invalid \\escape: line 1 column 3 (char 2)"},
		{"\"a\\u12\"", "Invalid \\uXXXX escape: line 1 column 4 (char 3)"},
		{"\"a\\u12zz\"", "Invalid \\uXXXX escape: line 1 column 4 (char 3)"},
		{"\"a\u0001b\"", "Invalid control character at: line 1 column 3 (char 2)"},
		{"01", "Extra data: line 1 column 2 (char 1)"},
		{"-", "Expecting value: line 1 column 1 (char 0)"},
		{"1.", "Extra data: line 1 column 2 (char 1)"},
		{"1e", "Extra data: line 1 column 2 (char 1)"},
		{"[", "Expecting value: line 1 column 2 (char 1)"},
		{"{", "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"},
		{"{\"a\":", "Expecting value: line 1 column 6 (char 5)"},
		{"  ", "Expecting value: line 1 column 3 (char 2)"},
		{"\n\n  x", "Expecting value: line 3 column 3 (char 4)"},
		{"tru", "Expecting value: line 1 column 1 (char 0)"},
		{"nul", "Expecting value: line 1 column 1 (char 0)"},
		{"[1, 2]  \n  ]", "Extra data: line 2 column 3 (char 11)"},
		{"{\"a\": [1, {\"b\": }]}", "Expecting value: line 1 column 17 (char 16)"},
		{"\"\\", "Unterminated string starting at: line 1 column 1 (char 0)"},
		{"\"\\u\"", "Invalid \\uXXXX escape: line 1 column 3 (char 2)"},
		{"1 2", "Extra data: line 1 column 3 (char 2)"},
		{"{\"a\"", "Expecting ':' delimiter: line 1 column 5 (char 4)"},
		{"{1: 2}", "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"},
		{"[,]", "Expecting value: line 1 column 2 (char 1)"},
		{"\"\t\"", "Invalid control character at: line 1 column 2 (char 1)"},
		{"\ufeff[]", "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)"},
		{"[1]\u0000", "Extra data: line 1 column 4 (char 3)"},
	}
	for _, c := range cases {
		_, err := UnmarshalJSON([]byte(c.in))
		if err == nil || err.Error() != c.want {
			t.Errorf("UnmarshalJSON(%q) = %v, want %q", c.in, err, c.want)
		}
	}
}

func TestUnmarshalJSONValues(t *testing.T) {
	v, err := UnmarshalJSON([]byte(`{"b": [1, 2.5, -0, 123456789012345678901234567890], "a": "é😀", "b": null}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(*OMap)
	if keys := m.Keys(); len(keys) != 2 || keys[0] != "b" || keys[1] != "a" {
		t.Fatalf("keys = %v", keys)
	}
	if m.Get("b") != nil || m.Get("a") != "é😀" {
		t.Fatalf("values = %v", m.AsMap())
	}
	v, _ = UnmarshalJSON([]byte(`[1, 2.5, -0, 123456789012345678901234567890]`))
	list := v.([]any)
	want, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	if list[0] != int64(1) || list[1] != 2.5 || list[2] != int64(0) || list[3].(*big.Int).Cmp(want) != 0 {
		t.Fatalf("numbers = %#v", list)
	}
}
