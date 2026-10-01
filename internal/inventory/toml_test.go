package inventory

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func TestParseTOMLValues(t *testing.T) {
	doc := `# comment
a = 0xff
b = 1_000
c = 0o755
d = 0b1010
e = -1.5e-3
f = 1979-05-27T07:32:00Z
g = 1979-05-27T00:32:00.999999-07:00
h = 1979-05-27
i = 07:32:00.5
j = """
one \
   two"""
k = 'C:\x'
l = [1, [2, "x"], {y = 1}, ]
m = { n.o = 1 }
"q k" = "\u00e9\t"
big = 123456789012345678901234567890

[t]
x.y = 1
[[arr]]
z = 1
[[arr]]
z = 2
`
	got, err := parseTOML(doc)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	want := `{"a":255,"arr":[{"z":1},{"z":2}],"b":1000,"big":123456789012345678901234567890,"c":493,"d":10,` +
		`"e":-0.0015,"f":"1979-05-27T07:32:00+00:00","g":"1979-05-27T00:32:00.999999-07:00","h":"1979-05-27",` +
		`"i":"07:32:00.500000","j":"one two","k":"C:\\x","l":[1,[2,"x"],{"y":1}],"m":{"n":{"o":1}},"q k":"é\t",` +
		`"t":{"x":{"y":1}}}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if keys := strings.Join(got.Keys(), ","); keys != "a,b,c,d,e,f,g,h,i,j,k,l,m,q k,big,t,arr" {
		t.Errorf("key order %s", keys)
	}
	if _, ok := got.Get("big").(*big.Int); !ok {
		t.Errorf("big: %T", got.Get("big"))
	}
}

// The messages are tomllib's (Python 3.14).
func TestParseTOMLErrors(t *testing.T) {
	cases := map[string]string{
		"a = 1\na = 2\n":           "Cannot overwrite a value (at line 2, column 6)",
		"x = \"abc\n":              `Illegal character '\n' (at line 1, column 9)`,
		"x = {a = 1, a = 2}\n":     "Duplicate inline table key 'a' (at line 1, column 18)",
		"x = [1, 2\n":              "Unclosed array (at end of document)",
		"x = 1979-13-01\n":         "Expected newline or end of document after a statement (at line 1, column 9)",
		"x = 1979-02-30\n":         "Invalid date or datetime (at line 1, column 5)",
		"x = \"\\q\"\n":            `Unescaped '\' in a string (at line 1, column 8)`,
		"[a]\n[a]\n":               "Cannot declare ('a',) twice (at line 2, column 3)",
		"big = 0x_ff\n":            "Expected newline or end of document after a statement (at line 1, column 8)",
		"= 1\n":                    "Invalid statement (at line 1, column 1)",
		"a.b = 1\n[a]\n":           "Cannot declare ('a',) twice (at line 2, column 3)",
		"a = {b = 1}\na.c = 2\n":   "Cannot mutate immutable namespace ('a',) (at line 2, column 8)",
		"x = nope\n":               "Invalid value (at line 1, column 5)",
		"x = 'a\x01'\n":            `Found invalid character '\x01' (at line 1, column 7)`,
		"x = \"\\uD800\"\n":        "Escaped character is not a Unicode scalar value (at line 1, column 12)",
		"[[a]]\n[a.b]\nc=1\n[a]\n": "Cannot declare ('a',) twice (at line 4, column 3)",
	}
	for doc, want := range cases {
		_, err := parseTOML(doc)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", doc, err, want)
		}
	}
}
