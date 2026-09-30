package yaml

import "fmt"

// parser builds the *Node tree from the scanner's token stream. Because the
// scanner already emits block structure tokens, this is a plain grammar walk.
type parser struct {
	name    string
	scan    *scanner // kept for error snippets
	tokens  []token
	pos     int
	anchors map[string]*Node
	filling map[*Node]bool // nodes whose Content is still being parsed (cycle detection)
}

// Parse parses all documents in src.
func Parse(src []byte, filename string) (*File, error) {
	s := newScanner(src, filename)
	tokens, err := s.scan()
	if err != nil {
		return nil, err
	}
	p := &parser{
		name:    filename,
		scan:    s,
		tokens:  tokens,
		anchors: map[string]*Node{},
		filling: map[*Node]bool{},
	}
	return p.parseStream()
}

// Unmarshal parses src and decodes the first document. An empty stream
// decodes to nil.
func Unmarshal(src []byte, filename string) (any, error) {
	f, err := Parse(src, filename)
	if err != nil {
		return nil, err
	}
	if len(f.Docs) == 0 {
		return nil, nil
	}
	return f.Docs[0].Decode()
}

func (p *parser) peek() token   { return p.tokens[p.pos] }
func (p *parser) next() token   { t := p.tokens[p.pos]; p.pos++; return t }
func (p *parser) kind() tokKind { return p.tokens[p.pos].kind }

func (p *parser) errf(t token, format string, args ...any) *Error {
	return &Error{
		File:    p.name,
		Line:    t.line,
		Col:     t.col,
		Msg:     fmt.Sprintf(format, args...),
		Snippet: p.scan.sourceLine(t.line),
	}
}

func (p *parser) parseStream() (*File, error) {
	f := &File{Name: p.name}
	for {
		switch p.kind() {
		case tokStreamEnd:
			return f, nil
		case tokDocEnd:
			p.next()
		case tokDocStart:
			p.next()
			// An empty document ("--- ---" or "---" at EOF) is a null doc.
			if k := p.kind(); k == tokDocStart || k == tokStreamEnd || k == tokDocEnd {
				t := p.peek()
				f.Docs = append(f.Docs, &Node{Kind: ScalarNode, Style: Plain, Line: t.line, Column: t.col})
				continue
			}
			node, err := p.parseNode(false)
			if err != nil {
				return nil, err
			}
			f.Docs = append(f.Docs, node)
		default:
			// Bare document (content before any ---).
			before := p.pos
			node, err := p.parseNode(false)
			if err != nil {
				return nil, err
			}
			if p.pos == before {
				// parseNode produced an empty node without consuming anything:
				// the next token cannot start a document.
				return nil, p.errf(p.peek(), "unexpected %s at the top level", p.kind())
			}
			f.Docs = append(f.Docs, node)
		}
	}
}

// parseNode parses one node. allowIndentless permits an indentless block
// sequence (a tokBlockEntry with no preceding tokBlockSeqStart), which is
// only legal as a block mapping value.
func (p *parser) parseNode(allowIndentless bool) (*Node, error) {
	anchor, tag := "", ""
	var anchorTok token
	for {
		if k := p.kind(); k == tokAnchor && anchor == "" {
			anchorTok = p.next()
			anchor = anchorTok.val
		} else if k == tokTag && tag == "" {
			tag = p.next().val
		} else {
			break
		}
	}

	t := p.peek()
	var node *Node
	var err error
	switch t.kind {
	case tokScalar:
		p.next()
		node = &Node{Kind: ScalarNode, Value: t.val, Style: t.style, Line: t.line, Column: t.col}
		recordOrigin(t.val, p.name, t.line, t.col)
	case tokAlias:
		p.next()
		if anchor != "" || tag != "" {
			return nil, p.errf(t, "an alias node cannot have an anchor or tag")
		}
		target, ok := p.anchors[t.val]
		if !ok {
			return nil, p.errf(t, "undefined alias %q", t.val)
		}
		if p.filling[target] {
			return nil, p.errf(t, "circular reference to alias %q", t.val)
		}
		return &Node{Kind: AliasNode, Value: t.val, Target: target, Line: t.line, Column: t.col}, nil
	case tokFlowSeqStart:
		node, err = p.parseFlowSeq(anchor)
	case tokFlowMapStart:
		node, err = p.parseFlowMap(anchor)
	case tokBlockSeqStart:
		node, err = p.parseBlockSeq(anchor)
	case tokBlockMapStart:
		node, err = p.parseBlockMap(anchor)
	case tokBlockEntry:
		if !allowIndentless {
			return nil, p.errf(t, "unexpected block sequence entry")
		}
		node, err = p.parseIndentlessSeq(anchor)
	case tokKey, tokValue, tokBlockEnd, tokFlowEntry, tokFlowSeqEnd, tokFlowMapEnd,
		tokDocStart, tokDocEnd, tokStreamEnd:
		// Empty node (e.g. "key:" with no value): a plain null scalar.
		node = &Node{Kind: ScalarNode, Style: Plain, Line: t.line, Column: t.col}
	default:
		return nil, p.errf(t, "unexpected %s while parsing a node", t.kind)
	}
	if err != nil {
		return nil, err
	}
	node.Tag = tag
	if anchor != "" {
		node.Anchor = anchor
		p.anchors[anchor] = node
	}
	return node, nil
}

// registerEarly makes an anchored container visible to aliases inside itself
// so that self-references are detected as cycles rather than "undefined".
func (p *parser) registerEarly(anchor string, node *Node) {
	if anchor != "" {
		p.anchors[anchor] = node
	}
	p.filling[node] = true
}

func (p *parser) parseBlockSeq(anchor string) (*Node, error) {
	start := p.next() // tokBlockSeqStart
	node := &Node{Kind: SequenceNode, Line: start.line, Column: start.col}
	p.registerEarly(anchor, node)
	defer delete(p.filling, node)
	for {
		switch p.kind() {
		case tokBlockEntry:
			entry := p.next()
			// An empty entry ("- " followed by another entry or dedent).
			if k := p.kind(); k == tokBlockEntry || k == tokBlockEnd {
				node.Content = append(node.Content,
					&Node{Kind: ScalarNode, Style: Plain, Line: entry.line, Column: entry.col})
				continue
			}
			item, err := p.parseNode(false)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, item)
		case tokBlockEnd:
			p.next()
			return node, nil
		default:
			return nil, p.errf(p.peek(), "expected a block sequence entry, found %s", p.kind())
		}
	}
}

func (p *parser) parseIndentlessSeq(anchor string) (*Node, error) {
	start := p.peek()
	node := &Node{Kind: SequenceNode, Line: start.line, Column: start.col}
	p.registerEarly(anchor, node)
	defer delete(p.filling, node)
	for p.kind() == tokBlockEntry {
		entry := p.next()
		if k := p.kind(); k == tokBlockEntry || k == tokBlockEnd || k == tokKey {
			node.Content = append(node.Content,
				&Node{Kind: ScalarNode, Style: Plain, Line: entry.line, Column: entry.col})
			continue
		}
		item, err := p.parseNode(false)
		if err != nil {
			return nil, err
		}
		node.Content = append(node.Content, item)
	}
	return node, nil
}

func (p *parser) parseBlockMap(anchor string) (*Node, error) {
	start := p.next() // tokBlockMapStart
	node := &Node{Kind: MappingNode, Line: start.line, Column: start.col}
	p.registerEarly(anchor, node)
	defer delete(p.filling, node)
	for {
		switch p.kind() {
		case tokKey:
			p.next()
			var key *Node
			var err error
			if p.kind() == tokValue {
				kt := p.peek()
				key = &Node{Kind: ScalarNode, Style: Plain, Line: kt.line, Column: kt.col}
			} else {
				key, err = p.parseNode(false)
				if err != nil {
					return nil, err
				}
			}
			var value *Node
			if p.kind() == tokValue {
				p.next()
				value, err = p.parseNode(true)
				if err != nil {
					return nil, err
				}
			} else {
				// Key with no ':' value token (rare): null value.
				value = &Node{Kind: ScalarNode, Style: Plain, Line: key.Line, Column: key.Column}
			}
			node.Content = append(node.Content, key, value)
		case tokValue:
			// A value with an empty key ("- : b" style). Null key.
			vt := p.next()
			key := &Node{Kind: ScalarNode, Style: Plain, Line: vt.line, Column: vt.col}
			value, err := p.parseNode(true)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, key, value)
		case tokBlockEnd:
			p.next()
			return node, nil
		default:
			return nil, p.errf(p.peek(), "did not find expected key (found %s)", p.kind())
		}
	}
}

func (p *parser) parseFlowSeq(anchor string) (*Node, error) {
	start := p.next() // tokFlowSeqStart
	node := &Node{Kind: SequenceNode, Style: FlowStyle, Line: start.line, Column: start.col}
	p.registerEarly(anchor, node)
	defer delete(p.filling, node)
	for {
		switch p.kind() {
		case tokFlowSeqEnd:
			p.next()
			return node, nil
		case tokStreamEnd:
			return nil, p.errf(p.peek(), "unexpected end of stream inside a flow sequence (missing ']')")
		case tokKey:
			// Implicit single-pair mapping: [a: b].
			kt := p.next()
			pair := &Node{Kind: MappingNode, Style: FlowStyle, Line: kt.line, Column: kt.col}
			key, err := p.parseFlowMapKey()
			if err != nil {
				return nil, err
			}
			var value *Node
			if p.kind() == tokValue {
				p.next()
				value, err = p.parseFlowMapValue()
				if err != nil {
					return nil, err
				}
			} else {
				value = &Node{Kind: ScalarNode, Style: Plain, Line: key.Line, Column: key.Column}
			}
			pair.Content = append(pair.Content, key, value)
			node.Content = append(node.Content, pair)
			if err := p.expectFlowSeparator(tokFlowSeqEnd); err != nil {
				return nil, err
			}
		default:
			item, err := p.parseNode(false)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, item)
			if err := p.expectFlowSeparator(tokFlowSeqEnd); err != nil {
				return nil, err
			}
		}
	}
}

func (p *parser) parseFlowMap(anchor string) (*Node, error) {
	start := p.next() // tokFlowMapStart
	node := &Node{Kind: MappingNode, Style: FlowStyle, Line: start.line, Column: start.col}
	p.registerEarly(anchor, node)
	defer delete(p.filling, node)
	for {
		switch p.kind() {
		case tokFlowMapEnd:
			p.next()
			return node, nil
		case tokStreamEnd:
			return nil, p.errf(p.peek(), "unexpected end of stream inside a flow mapping (missing '}')")
		default:
			if p.kind() == tokKey {
				p.next()
			}
			key, err := p.parseFlowMapKey()
			if err != nil {
				return nil, err
			}
			var value *Node
			if p.kind() == tokValue {
				p.next()
				value, err = p.parseFlowMapValue()
				if err != nil {
					return nil, err
				}
			} else {
				// Bare entry: {a} -> a: null.
				value = &Node{Kind: ScalarNode, Style: Plain, Line: key.Line, Column: key.Column}
			}
			node.Content = append(node.Content, key, value)
			if err := p.expectFlowSeparator(tokFlowMapEnd); err != nil {
				return nil, err
			}
		}
	}
}

// parseFlowMapKey parses a key node, or an empty scalar if the key is absent.
func (p *parser) parseFlowMapKey() (*Node, error) {
	if k := p.kind(); k == tokValue {
		t := p.peek()
		return &Node{Kind: ScalarNode, Style: Plain, Line: t.line, Column: t.col}, nil
	}
	return p.parseNode(false)
}

// parseFlowMapValue parses a value node, or an empty scalar if absent
// (before ',' or a closing bracket).
func (p *parser) parseFlowMapValue() (*Node, error) {
	if k := p.kind(); k == tokFlowEntry || k == tokFlowMapEnd || k == tokFlowSeqEnd {
		t := p.peek()
		return &Node{Kind: ScalarNode, Style: Plain, Line: t.line, Column: t.col}, nil
	}
	return p.parseNode(false)
}

// expectFlowSeparator consumes a ',' if present; otherwise the next token
// must be the collection's closing bracket.
func (p *parser) expectFlowSeparator(closer tokKind) error {
	switch p.kind() {
	case tokFlowEntry:
		p.next()
		return nil
	case closer:
		return nil
	case tokStreamEnd:
		return p.errf(p.peek(), "unexpected end of stream inside a flow collection (missing %s)",
			map[tokKind]string{tokFlowSeqEnd: "']'", tokFlowMapEnd: "'}'"}[closer])
	default:
		return p.errf(p.peek(), "expected ',' or %s in flow collection, found %s", closer, p.kind())
	}
}
