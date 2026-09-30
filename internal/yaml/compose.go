package yaml

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

// composer builds Node trees from parser events, as PyYAML's C binding
// (_yaml.pyx) composes them for ansible-core: anchors are per document, an
// undefined alias or a redefined anchor is an error.
type composer struct {
	p       *parser
	anchors map[string]*Node
	filling map[*Node]bool // collections still being composed (alias cycles)
	ev      event          // the current event
}

// newComposer reads src as ansible-core's loader does; a reader error is
// returned before any scanning.
func newComposer(src []byte, filename string) (*composer, error) {
	stream, err := readSource(src, filename)
	if err != nil {
		return nil, err
	}
	return &composer{p: &parser{s: newScanner(stream, src, filename)}}, nil
}

// readSource does what happens to a file's bytes before libyaml scans
// them: ansible-core decodes them as UTF-8 (undecodable bytes become
// surrogates, which PyYAML then cannot encode), and libyaml's reader skips
// a byte order mark and rejects control characters.
func readSource(src []byte, filename string) ([]byte, error) {
	chars := 0
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			return nil, &Error{File: filename, Msg: fmt.Sprintf(
				"'utf-8' codec can't encode character '\\udc%02x' in position %d: surrogates not allowed", src[i], chars)}
		}
		i += size
		chars++
	}
	offset := 0
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		src, offset = src[3:], 3
	}
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if !(r == 0x09 || r == 0x0A || r == 0x0D || (r >= 0x20 && r <= 0x7E) || r == 0x85 ||
			(r >= 0xA0 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)) {
			return nil, &Error{File: filename, Msg: fmt.Sprintf(
				"unacceptable character #x%04x: control characters are not allowed\n  in \"<unicode string>\", position %d", r, offset+i)}
		}
		i += size
	}
	return src, nil
}

func (c *composer) err() error { return c.p.s.err }

func (c *composer) nextEvent() bool {
	ev, ok := c.p.next()
	c.ev = ev
	return ok
}

func (c *composer) composerError(context, problem string, m mark) error {
	s := c.p.s
	s.err = &Error{File: s.name, Context: context, Problem: problem,
		Line: m.line + 1, Col: m.column + 1, src: s.text}
	return s.err
}

// Parse parses every document in src (PyYAML's compose_all).
func Parse(src []byte, filename string) (*File, error) {
	c, err := newComposer(src, filename)
	if err != nil {
		return nil, err
	}
	f := &File{Name: filename}
	if !c.nextEvent() { // STREAM-START
		return nil, c.err()
	}
	for {
		if !c.nextEvent() {
			return nil, c.err()
		}
		if c.ev.kind == evStreamEnd {
			return f, nil
		}
		doc, err := c.composeDocument()
		if err != nil {
			return nil, err
		}
		if err := construct(doc, filename, src); err != nil {
			return nil, err
		}
		f.Docs = append(f.Docs, doc)
	}
}

// ParseSingle parses a stream that must hold at most one document, as
// ansible-core's loader does (PyYAML's get_single_node): nil for an empty
// stream, and an error if a second document starts.
func ParseSingle(src []byte, filename string) (*Node, error) {
	c, err := newComposer(src, filename)
	if err != nil {
		return nil, err
	}
	if !c.nextEvent() { // STREAM-START
		return nil, c.err()
	}
	if !c.nextEvent() {
		return nil, c.err()
	}
	var doc *Node
	if c.ev.kind != evStreamEnd {
		if doc, err = c.composeDocument(); err != nil {
			return nil, err
		}
		if !c.nextEvent() {
			return nil, c.err()
		}
		if c.ev.kind != evStreamEnd {
			return nil, c.composerError("expected a single document in the stream",
				"but found another document", c.ev.start)
		}
		if err := construct(doc, filename, src); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// Unmarshal parses a single-document stream and decodes it. An empty
// stream decodes to nil.
func Unmarshal(src []byte, filename string) (any, error) {
	doc, err := ParseSingle(src, filename)
	if err != nil || doc == nil {
		return nil, err
	}
	return doc.Decode()
}

// composeDocument composes the document whose DOCUMENT-START is the current
// event, consuming through its DOCUMENT-END.
func (c *composer) composeDocument() (*Node, error) {
	c.anchors = map[string]*Node{}
	c.filling = map[*Node]bool{}
	if !c.nextEvent() {
		return nil, c.err()
	}
	node, err := c.composeNode(c.ev)
	if err != nil {
		return nil, err
	}
	if !c.nextEvent() { // DOCUMENT-END
		return nil, c.err()
	}
	return node, nil
}

func nodeAt(kind Kind, ev event) *Node {
	return &Node{Kind: kind, Tag: ev.tag, Anchor: ev.anchor, Line: ev.start.line + 1, Column: ev.start.column + 1}
}

// composeNode composes the node that ev starts.
func (c *composer) composeNode(ev event) (*Node, error) {
	if ev.kind == evAlias {
		target, ok := c.anchors[ev.anchor]
		if !ok {
			return nil, c.composerError("", "found undefined alias", ev.start)
		}
		if c.filling[target] {
			// PyYAML builds a recursive structure here; understudy's values
			// cannot hold one, so decoding it fails.
			target.recursive = true
		}
		return &Node{Kind: AliasNode, Value: ev.anchor, Target: target,
			Line: ev.start.line + 1, Column: ev.start.column + 1}, nil
	}
	if ev.anchor != "" {
		if _, dup := c.anchors[ev.anchor]; dup {
			return nil, c.composerError("found duplicate anchor; first occurrence", "second occurrence", ev.start)
		}
	}
	var n *Node
	var end eventKind
	switch ev.kind {
	case evScalar:
		n = nodeAt(ScalarNode, ev)
		n.Value, n.Style = ev.value, ev.style
		if ev.anchor != "" {
			c.anchors[ev.anchor] = n
		}
		return n, nil
	case evSequenceStart:
		n, end = nodeAt(SequenceNode, ev), evSequenceEnd
	case evMappingStart:
		n, end = nodeAt(MappingNode, ev), evMappingEnd
	default:
		panic("yaml: unexpected event while composing")
	}
	n.Style = ev.style
	if ev.anchor != "" {
		c.anchors[ev.anchor] = n
	}
	c.filling[n] = true
	for {
		if !c.nextEvent() {
			return nil, c.err()
		}
		if c.ev.kind == end {
			break
		}
		item, err := c.composeNode(c.ev)
		if err != nil {
			return nil, err
		}
		n.Content = append(n.Content, item)
	}
	delete(c.filling, n)
	return n, nil
}
