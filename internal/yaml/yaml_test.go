package yaml

import (
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustUnmarshal(t *testing.T, src string) any {
	t.Helper()
	v, err := Unmarshal([]byte(src), "test.yml")
	if err != nil {
		t.Fatalf("Unmarshal(%q): %v", src, err)
	}
	// Normalize *OMap to plain maps so structure/value assertions work; key
	// order is verified separately in TestKeyOrderPreserved.
	return AsMap(v)
}

func eq(t *testing.T, src string, want any) {
	t.Helper()
	got := AsMap(mustUnmarshal(t, src))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmarshal(%q)\n got: %#v\nwant: %#v", src, got, want)
	}
}

func mapv(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestScalarResolution11(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		// Booleans: the exact PyYAML word list.
		{"yes", true}, {"Yes", true}, {"YES", true}, {"no", false},
		{"true", true}, {"True", true}, {"on", true}, {"off", false},
		{"Off", false}, {"FALSE", false},
		// Single y/n are NOT booleans (PyYAML quirk).
		{"y", "y"}, {"n", "n"}, {"Y", "Y"}, {"N", "N"},
		// Null.
		{"~", nil}, {"null", nil}, {"Null", nil}, {"NULL", nil},
		// Integers.
		{"0", int64(0)}, {"42", int64(42)}, {"-17", int64(-17)}, {"+8", int64(8)},
		{"1_000", int64(1000)},
		{"0644", int64(420)},  // legacy octal
		{"0o755", "0o755"},    // 1.2-style octal: a string to PyYAML
		{"0x1F", int64(31)},   // hex
		{"0b1010", int64(10)}, // binary
		{"9999999999999999999999", testBigInt("9999999999999999999999")}, // Python ints have no size limit
		// Floats.
		{"1.5", 1.5}, {"1.10", 1.1}, {"-2.0", -2.0}, {".5", 0.5},
		{"1.5e+3", 1500.0}, {"1.5E-2", 0.015},
		{"1e5", "1e5"},     // PyYAML quirk: exponent requires a sign
		{"1.5e3", "1.5e3"}, // same
		{".inf", math.Inf(1)}, {"-.inf", math.Inf(-1)},
		// Timestamps: datetime.date and datetime.datetime.
		{"2024-01-15", Date{T: time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)}},
		{"2024-1-5 1:02:03", Datetime{T: time.Date(2024, 1, 5, 1, 2, 3, 0, time.UTC)}},
		{"2001-12-14t21:59:43.10-05:00", Datetime{T: time.Date(2001, 12, 14, 21, 59, 43, 100000000, time.UTC), TZ: &TZ{Offset: -5 * time.Hour}}},
		{"2001-12-14 21:59:43.1234567 Z", Datetime{T: time.Date(2001, 12, 14, 21, 59, 43, 123456000, time.UTC), TZ: UTC}},
		{"2024-1-15", "2024-1-15"}, // a date needs two-digit months and days
		// Sexagesimals: base 60 ints and floats.
		{"1:30", int64(90)}, {"-1:20:30", int64(-4830)}, {"1:20.5", 80.5},
		{"1:60", "1:60"}, {"1_0:2_0", "1_0:2_0"},
		// Version-number strings.
		{"1.2.3", "1.2.3"},
		// Plain strings.
		{"hello", "hello"}, {"hello world", "hello world"},
	}
	for _, c := range cases {
		eq(t, c.src, c.want)
	}
	// NaN needs special comparison.
	if v := mustUnmarshal(t, ".nan"); v != v {
		// ok: NaN != NaN
	} else {
		t.Errorf(".nan did not decode to NaN: %#v", v)
	}
}

func TestQuotedScalarsStayStrings(t *testing.T) {
	eq(t, `"yes"`, "yes")
	eq(t, `'0644'`, "0644")
	eq(t, `"1.10"`, "1.10")
	eq(t, `"null"`, "null")
	eq(t, `""`, "")
}

func TestDoubleQuotedEscapes(t *testing.T) {
	eq(t, `"a\nb"`, "a\nb")
	eq(t, `"tab\there"`, "tab\there")
	eq(t, `"\x41B"`, "AB")
	eq(t, `"back\\slash"`, "back\\slash")
	eq(t, `"quote\"inside"`, "quote\"inside")
	eq(t, `'it''s'`, "it's")
}

func TestBlockMapping(t *testing.T) {
	eq(t, "a: 1\nb: two\nc: true\n", mapv("a", int64(1), "b", "two", "c", true))
	eq(t, "a:\n  b:\n    c: deep\n", mapv("a", mapv("b", mapv("c", "deep"))))
	eq(t, "empty:\nafter: 1\n", mapv("empty", nil, "after", int64(1)))
}

func TestBlockSequence(t *testing.T) {
	eq(t, "- 1\n- 2\n- 3\n", []any{int64(1), int64(2), int64(3)})
	eq(t, "- a\n-\n- c\n", []any{"a", nil, "c"})
	eq(t, "a:\n  - 1\n  - 2\n", mapv("a", []any{int64(1), int64(2)}))
	// Indentless sequence: entries at the same column as the key.
	eq(t, "a:\n- 1\n- 2\nb: x\n", mapv("a", []any{int64(1), int64(2)}, "b", "x"))
}

func TestCompactNesting(t *testing.T) {
	// The universal Ansible task-list shape.
	src := `
- name: first task
  command: echo hi
  vars:
    x: 1
- name: second
  debug:
    msg: hello
`
	want := []any{
		mapv("name", "first task", "command", "echo hi", "vars", mapv("x", int64(1))),
		mapv("name", "second", "debug", mapv("msg", "hello")),
	}
	eq(t, src, want)
}

func TestFlowCollections(t *testing.T) {
	eq(t, "[1, 2, 3]", []any{int64(1), int64(2), int64(3)})
	eq(t, "{a: 1, b: 2}", mapv("a", int64(1), "b", int64(2)))
	eq(t, "loop: [a, b]", mapv("loop", []any{"a", "b"}))
	eq(t, "{a: [1, {b: 2}], c: {}}", mapv("a", []any{int64(1), mapv("b", int64(2))}, "c", mapv()))
	eq(t, "[]", []any{})
	// JSON is valid YAML.
	eq(t, `{"a": [1, 2.5, true, null]}`, mapv("a", []any{int64(1), 2.5, true, nil}))
	// Trailing comma tolerance and nested breaks.
	eq(t, "[1,\n 2]", []any{int64(1), int64(2)})
	// YAML 1.1: a:b without space is one scalar in flow.
	eq(t, "[a:b]", []any{"a:b"})
	// Implicit single-pair mapping in a flow sequence.
	eq(t, "[a: 1]", []any{mapv("a", int64(1))})
	// URLs survive as plain scalars.
	eq(t, "url: http://example.com/x\n", mapv("url", "http://example.com/x"))
}

func TestMultiLinePlain(t *testing.T) {
	eq(t, "key: this is\n  a folded value\n", mapv("key", "this is a folded value"))
	eq(t, "key: line one\n\n  after blank\n", mapv("key", "line one\nafter blank"))
}

func TestLiteralBlockScalar(t *testing.T) {
	eq(t, "s: |\n  line1\n  line2\n", mapv("s", "line1\nline2\n"))
	eq(t, "s: |-\n  line1\n  line2\n", mapv("s", "line1\nline2"))
	eq(t, "s: |+\n  line1\n\n\n", mapv("s", "line1\n\n\n"))
	// Interior blank lines preserved.
	eq(t, "s: |\n  a\n\n  b\n", mapv("s", "a\n\nb\n"))
	// More-indented lines keep their extra spaces.
	eq(t, "s: |\n  a\n    indented\n  b\n", mapv("s", "a\n  indented\nb\n"))
	// Explicit indentation indicator.
	eq(t, "s: |2\n    a\n", mapv("s", "  a\n"))
	// Content after the scalar continues the mapping.
	eq(t, "s: |\n  x\nnext: 1\n", mapv("s", "x\n", "next", int64(1)))
}

func TestFoldedBlockScalar(t *testing.T) {
	eq(t, "s: >\n  one\n  two\n", mapv("s", "one two\n"))
	eq(t, "s: >\n  one\n\n  two\n", mapv("s", "one\ntwo\n"))
	eq(t, "s: >-\n  one\n  two\n", mapv("s", "one two"))
	// More-indented lines are not folded.
	eq(t, "s: >\n  one\n    literal\n  two\n", mapv("s", "one\n  literal\ntwo\n"))
}

func TestComments(t *testing.T) {
	eq(t, "# leading comment\na: 1 # trailing\n# footer\n", mapv("a", int64(1)))
	eq(t, "a: value # not: a: mapping\n", mapv("a", "value"))
}

func TestAnchorsAndAliases(t *testing.T) {
	eq(t, "a: &x hello\nb: *x\n", mapv("a", "hello", "b", "hello"))
	src := `
defaults: &defaults
  user: admin
  port: 22
server:
  <<: *defaults
  port: 2222
`
	eq(t, src, mapv(
		"defaults", mapv("user", "admin", "port", int64(22)),
		"server", mapv("user", "admin", "port", int64(2222)),
	))
}

func TestMergeMultiple(t *testing.T) {
	src := `
a: &a {x: 1, y: 1}
b: &b {y: 2, z: 2}
merged:
  <<: [*a, *b]
  w: 0
`
	// Earlier merge sources win: y comes from *a.
	eq(t, src, mapv(
		"a", mapv("x", int64(1), "y", int64(1)),
		"b", mapv("y", int64(2), "z", int64(2)),
		"merged", mapv("x", int64(1), "y", int64(1), "z", int64(2), "w", int64(0)),
	))
}

func TestMultiDocument(t *testing.T) {
	f, err := Parse([]byte("---\na: 1\n---\nb: 2\n"), "t.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(f.Docs))
	}
	// Leading --- with a directive.
	eq(t, "%YAML 1.1\n---\na: 1\n", mapv("a", int64(1)))
	// --- header alone (the universal playbook opener).
	eq(t, "---\n- name: t\n  ping:\n", []any{mapv("name", "t", "ping", nil)})
}

func TestTags(t *testing.T) {
	v := mustUnmarshal(t, "secret: !vault |\n  $ANSIBLE_VAULT;1.1;AES256\n  61626364\n")
	m := v.(map[string]any)
	vs, ok := m["secret"].(VaultedString)
	if !ok {
		t.Fatalf("expected VaultedString, got %T", m["secret"])
	}
	if !strings.HasPrefix(vs.Ciphertext, "$ANSIBLE_VAULT;1.1;AES256\n") {
		t.Errorf("ciphertext = %q", vs.Ciphertext)
	}

	v = mustUnmarshal(t, "raw: !unsafe '{{ not_a_template }}'\n")
	if got := v.(map[string]any)["raw"]; got != UnsafeString("{{ not_a_template }}") {
		t.Errorf("unsafe = %#v", got)
	}

	eq(t, "n: !!str 123\n", mapv("n", "123"))
	eq(t, "n: !!int '42'\n", mapv("n", int64(42)))
}

func TestPositions(t *testing.T) {
	f, err := Parse([]byte("a: 1\nb:\n  - x\n"), "pos.yml")
	if err != nil {
		t.Fatal(err)
	}
	root := f.Docs[0]
	if root.Line != 1 || root.Column != 1 {
		t.Errorf("root at %d:%d, want 1:1", root.Line, root.Column)
	}
	bVal := root.MapGet("b")
	if bVal == nil || bVal.Line != 3 || bVal.Column != 3 {
		t.Errorf("b's value at %v, want line 3 col 3", bVal)
	}
}

func TestNodeNavigation(t *testing.T) {
	f, err := Parse([]byte("name: test\nitems:\n  - 1\n  - 2\n"), "nav.yml")
	if err != nil {
		t.Fatal(err)
	}
	root := f.Docs[0]
	if got := root.MapKeys(); !reflect.DeepEqual(got, []string{"name", "items"}) {
		t.Errorf("MapKeys = %v", got)
	}
	if s, ok := root.MapGet("name").Str(); !ok || s != "test" {
		t.Errorf("MapGet(name).Str() = %q, %v", s, ok)
	}
	if items, ok := root.MapGet("items").Seq(); !ok || len(items) != 2 {
		t.Errorf("items Seq: %v %v", items, ok)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantSub string
	}{
		{"a: b\n c: d\n", "err.yml:2:3: YAML parsing failed: Mapping values are not allowed in this context."},
		{"? [a]\n: key\n", "err.yml:1:3: YAML parsing failed: While constructing a mapping found unhashable key."},
		{"a: *nope\n", "err.yml:1:4: YAML parsing failed: Found undefined alias."},
		{"\ta: 1\n", "err.yml:1:1: YAML parsing failed: Tabs are usually invalid in YAML."},
		{"a: [1, 2\n", "err.yml:2:1: YAML parsing failed: While parsing a flow sequence did not find expected ',' or ']'."},
		{"a: 1\n---\nb: 2\n", "err.yml:2:1: YAML parsing failed: Expected a single document in the stream but found another document."},
		{"a: !foo 1\n", "err.yml:1:4: YAML parsing failed: Could not determine a constructor for the tag '!foo'."},
		{"a: !!int x\n", "err.yml: YAML parsing failed: invalid literal for int() with base 10: 'x'"},
	}
	for _, c := range cases {
		_, err := Unmarshal([]byte(c.src), "err.yml")
		if err == nil {
			t.Errorf("Unmarshal(%q): expected error containing %q, got nil", c.src, c.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("Unmarshal(%q) error = %q, want substring %q", c.src, err.Error(), c.wantSub)
		}
	}
}

func TestDuplicateKeysLastWinsWithWarning(t *testing.T) {
	var warned []Warning
	OnWarning = func(w Warning) { warned = append(warned, w) }
	defer func() { OnWarning = nil }()
	eq(t, "a: 1\na: 2\n", mapv("a", int64(2)))
	want := Warning{Msg: "Found duplicate mapping key 'a'.", Help: "Using last defined value only.", File: "test.yml", Line: 2, Col: 1, Key: "a"}
	if len(warned) != 1 || warned[0] != want {
		t.Errorf("warnings = %+v", warned)
	}
}

func TestRealPlaybookShape(t *testing.T) {
	src := `---
- name: Configure webservers
  hosts: webservers
  become: yes
  vars:
    http_port: 80
    max_clients: 200
  tasks:
    - name: Install nginx
      apt:
        name: nginx
        state: present
      notify: restart nginx
    - name: Write config
      template: src=nginx.conf.j2 dest=/etc/nginx/nginx.conf mode=0644
      when: ansible_os_family == "Debian"
  handlers:
    - name: restart nginx
      service:
        name: nginx
        state: restarted
`
	v := mustUnmarshal(t, src)
	plays := v.([]any)
	play := plays[0].(map[string]any)
	if play["become"] != true {
		t.Errorf("become = %#v, want true", play["become"])
	}
	tasks := play["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	apt := tasks[0].(map[string]any)["apt"].(map[string]any)
	if apt["name"] != "nginx" || apt["state"] != "present" {
		t.Errorf("apt args = %#v", apt)
	}
	if w := tasks[1].(map[string]any)["when"]; w != `ansible_os_family == "Debian"` {
		t.Errorf("when = %#v", w)
	}
}

func FuzzParse(f *testing.F) {
	seeds := []string{
		"a: 1\nb:\n  - x\n", "{a: [1, 2]}", "s: |\n  x\n", "s: >\n  x\n",
		"a: &x 1\nb: *x\n", "<<: *x\n", "---\na\n---\nb\n", "'quoted'", `"dq\n"`,
		"- - - nested\n", "a:\n- 1\n- 2\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		f, err := Parse(data, "fuzz.yml")
		if err != nil {
			return
		}
		for _, d := range f.Docs {
			d.Decode() // must not panic; errors are fine
		}
	})
}

func TestKeyOrderPreserved(t *testing.T) {
	v, err := Unmarshal([]byte("z: 1\na: 2\nm: 3\nb: 4\n"), "order.yml")
	if err != nil {
		t.Fatal(err)
	}
	om, ok := v.(*OMap)
	if !ok {
		t.Fatalf("expected *OMap, got %T", v)
	}
	if got := om.Keys(); !reflect.DeepEqual(got, []string{"z", "a", "m", "b"}) {
		t.Errorf("key order = %v, want source order [z a m b]", got)
	}
	// Merged keys come first, then the mapping's own (PyYAML's flatten_mapping).
	v2, _ := Unmarshal([]byte("base: &b {x: 1, y: 2}\nchild:\n  <<: *b\n  first: 0\n"), "m.yml")
	child := v2.(*OMap).Get("child").(*OMap)
	if got := child.Keys(); !reflect.DeepEqual(got, []string{"x", "y", "first"}) {
		t.Errorf("merge order = %v, want [x y first]", got)
	}
}

func testBigInt(s string) *big.Int {
	b, _ := new(big.Int).SetString(s, 10)
	return b
}
