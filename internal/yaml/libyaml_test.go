package yaml

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestLibyamlCorpus checks the scanner, parser and composer against libyaml
// 0.2.5 as ansible-core drives it (PyYAML 6's CBaseLoader.get_single_node):
// testdata/libyaml.json holds inputs, many of them malformed, with the node
// tree libyaml composes or the error it raises (context, problem, and the
// problem mark ansible-core reports).
func TestLibyamlCorpus(t *testing.T) {
	path := "testdata/libyaml.json"
	if p := os.Getenv("LIBYAML_CORPUS"); p != "" {
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		In  string   `json:"in"`
		Out []string `json:"out"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	fails := 0
	for _, c := range cases {
		got := composeCanonical(c.In)
		want := strings.Join(c.Out, "\n")
		if got != want {
			fails++
			if fails <= 30 {
				g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
				i := 0
				for i < len(g) && i < len(w) && g[i] == w[i] {
					i++
				}
				t.Errorf("input %q\nfirst difference at output line %d:\n got: %s\nwant: %s", c.In, i+1, at(g, i), at(w, i))
			}
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d cases differ", fails, len(cases))
	}
}

// composeCanonical renders what composing src yields in the corpus format.
func composeCanonical(src string) string {
	var doc *Node
	err := func() error {
		c, err := newComposer([]byte(src), "t")
		if err != nil {
			return err
		}
		if !c.nextEvent() || !c.nextEvent() {
			return c.err()
		}
		if c.ev.kind == evStreamEnd {
			return nil
		}
		if doc, err = c.composeDocument(); err != nil {
			return err
		}
		if !c.nextEvent() {
			return c.err()
		}
		if c.ev.kind != evStreamEnd {
			return c.composerError("expected a single document in the stream", "but found another document", c.ev.start)
		}
		return nil
	}()
	if err != nil {
		e := err.(*Error)
		return fmt.Sprintf("ERR %d:%d %s|%s", e.Line, e.Col, e.Context, e.Problem)
	}
	if doc == nil {
		return "EMPTY"
	}
	var lines []string
	dumpCanonical(doc, 0, &lines)
	return strings.Join(lines, "\n")
}

func dumpCanonical(n *Node, depth int, out *[]string) {
	n = n.resolveAlias()
	pad := strings.Repeat(" ", depth)
	if depth > 40 {
		*out = append(*out, pad+"DEEP")
		return
	}
	tag := ""
	switch n.Tag {
	case "", "!", tagStr, tagSeq, tagMap:
		// libyaml's "!" and untagged nodes resolve to the defaults.
	default:
		tag = " <" + n.Tag + ">"
	}
	pos := fmt.Sprintf("%d:%d", n.Line, n.Column)
	switch n.Kind {
	case ScalarNode:
		style := map[Style]string{Plain: "plain", SingleQuoted: "single", DoubleQuoted: "double", Literal: "literal", Folded: "folded"}[n.Style]
		v, _ := json.Marshal(n.Value)
		*out = append(*out, fmt.Sprintf("%s=VAL %s%s %s %s", pad, pos, tag, style, pyJSON(string(v))))
	case SequenceNode, MappingNode:
		kind, style := "+SEQ", "block"
		if n.Kind == MappingNode {
			kind = "+MAP"
		}
		if n.Style == FlowStyle {
			style = "flow"
		}
		*out = append(*out, fmt.Sprintf("%s%s %s%s %s", pad, kind, pos, tag, style))
		for _, c := range n.Content {
			dumpCanonical(c, depth+1, out)
		}
	}
}

// pyJSON rewrites Go's JSON string escapes as Python's json.dumps writes
// them (ensure_ascii).
func pyJSON(s string) string {
	s = strings.NewReplacer("\\"+"u003c", "<", "\\"+"u003e", ">", "\\"+"u0026", "&", "\\"+"u0008", "\\b", "\\"+"u000c", "\\f").Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		case r > 0x7F:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "(end)"
}
