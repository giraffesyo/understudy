package modules

import (
	"reflect"
	"testing"
)

func TestAKParseOptions(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := map[string]akOptions{
		`no-pty,command="echo a,b"`: {{key: "no-pty"}, {key: "command", value: str(`"echo a,b"`)}},
		`from="a,b",x`:              {{key: "from", value: str(`"a,b"`)}, {key: "x"}},
		// re.split keeps the separators: ",," is a bare option.
		`a,,b`: {{key: "a"}, {key: ",,"}, {key: "b"}},
	}
	for in, want := range cases {
		if got := akParseOptions(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
	if !akParseOptions("a=1,b").equal(akParseOptions("b,a=1")) {
		t.Error("option order must not matter")
	}
	if akParseOptions("a=1,a=2").equal(akParseOptions("a=2,a=1")) {
		t.Error("repeated option values keep their order")
	}
}

func TestAKSerializeKeepsJunkAndRanks(t *testing.T) {
	in := "# c\n\nssh-rsa AAA x\nno-pty ssh-ed25519 BBB\njunk\nssh-rsa AAA dup\n"
	keys, ok := akParseKeys(in)
	if !ok {
		t.Fatal("parse failed")
	}
	// The duplicate keeps its first position but the last line's rank.
	want := "# c\n\nno-pty ssh-ed25519 BBB \njunk\nssh-rsa AAA dup\n"
	if got := akSerialize(keys); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestPySplitlinesKeep(t *testing.T) {
	got := pySplitlinesKeep("a\r\nb\rc\nd\x0be f")
	want := []string{"a\r\n", "b\r", "c\n", "d\x0b", "e ", "f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}
