package executor

import (
	"reflect"
	"testing"
)

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a' + i))
	}
	return out
}

func TestBatchHosts(t *testing.T) {
	hosts := names(10) // a..j

	cases := []struct {
		serial []any
		want   [][]string
	}{
		{nil, [][]string{{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}}},
		{[]any{int64(1)}, [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}, {"f"}, {"g"}, {"h"}, {"i"}, {"j"}}},
		{[]any{int64(3)}, [][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"g", "h", "i"}, {"j"}}},
		// Progressive: 1, then 2, then the rest in 5s.
		{[]any{int64(1), int64(2), int64(5)}, [][]string{{"a"}, {"b", "c"}, {"d", "e", "f", "g", "h"}, {"i", "j"}}},
		// Percentage: 30% of 10 = 3.
		{[]any{"30%"}, [][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"g", "h", "i"}, {"j"}}},
		// A count larger than the host list is one batch.
		{[]any{int64(100)}, [][]string{{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}}},
	}
	for _, c := range cases {
		got := batchHosts(hosts, c.serial)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("batchHosts(%v)\n got: %v\nwant: %v", c.serial, got, c.want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	cases := []struct {
		s     string
		total int
		want  int
		ok    bool
	}{
		{"50%", 10, 5, true},
		{"33%", 9, 2, true}, // floor(2.97)
		{"1%", 10, 1, true}, // rounds up to minimum 1
		{"5", 10, 0, false}, // not a percentage
		{"100%", 4, 4, true},
	}
	for _, c := range cases {
		got, ok := parsePercent(c.s, c.total)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parsePercent(%q, %d) = %d, %v; want %d, %v", c.s, c.total, got, ok, c.want, c.ok)
		}
	}
}
