package yaml

// parser turns the token stream into events with libyaml's state machine
// (parser.c), so a malformed document fails at the same token, with the
// same context and problem, as it does under ansible-core.
type parser struct {
	s      *scanner
	state  parserState
	states []parserState
	marks  []mark
	tags   []tagDirective // %TAG directives in effect (plus the defaults)
	done   bool
}

type parserState uint8

const (
	parseStreamStart parserState = iota
	parseImplicitDocumentStart
	parseDocumentStart
	parseDocumentContent
	parseDocumentEnd
	parseBlockNode
	parseBlockNodeOrIndentlessSequence
	parseFlowNode
	parseBlockSequenceFirstEntry
	parseBlockSequenceEntry
	parseIndentlessSequenceEntry
	parseBlockMappingFirstKey
	parseBlockMappingKey
	parseBlockMappingValue
	parseFlowSequenceFirstEntry
	parseFlowSequenceEntry
	parseFlowSequenceEntryMappingKey
	parseFlowSequenceEntryMappingValue
	parseFlowSequenceEntryMappingEnd
	parseFlowMappingFirstKey
	parseFlowMappingKey
	parseFlowMappingValue
	parseFlowMappingEmptyValue
	parseEnd
)

type tagDirective struct{ handle, prefix string }

var defaultTagDirectives = []tagDirective{{"!", "!"}, {"!!", "tag:yaml.org,2002:"}}

type eventKind uint8

const (
	evStreamStart eventKind = iota
	evStreamEnd
	evDocumentStart
	evDocumentEnd
	evAlias
	evScalar
	evSequenceStart
	evSequenceEnd
	evMappingStart
	evMappingEnd
)

type event struct {
	kind       eventKind
	start, end mark
	anchor     string
	tag        string // resolved; "" when none
	value      string
	style      Style // scalars; FlowStyle for flow collections
}

func (p *parser) peek() *token {
	t := p.s.peek()
	return t
}

func (p *parser) skip() { p.s.skipToken() }

func (p *parser) pop() parserState {
	st := p.states[len(p.states)-1]
	p.states = p.states[:len(p.states)-1]
	return st
}

func (p *parser) popMark() mark {
	m := p.marks[len(p.marks)-1]
	p.marks = p.marks[:len(p.marks)-1]
	return m
}

// parserError records a parser error; ok is always false.
func (p *parser) parserError(context, problem string, problemMark mark) bool {
	p.s.err = &Error{File: p.s.name, Context: context, Problem: problem,
		Line: problemMark.line + 1, Col: problemMark.column + 1, src: p.s.text}
	return false
}

// next produces the next event (yaml_parser_parse).
func (p *parser) next() (event, bool) {
	if p.s.err != nil {
		return event{}, false
	}
	if p.done || p.state == parseEnd {
		return event{kind: evStreamEnd}, true
	}
	var ev event
	ok := p.stateMachine(&ev)
	if ok && ev.kind == evStreamEnd {
		p.done = true
	}
	return ev, ok
}

func (p *parser) stateMachine(ev *event) bool {
	switch p.state {
	case parseStreamStart:
		return p.parseStreamStart(ev)
	case parseImplicitDocumentStart:
		return p.parseDocumentStart(ev, true)
	case parseDocumentStart:
		return p.parseDocumentStart(ev, false)
	case parseDocumentContent:
		return p.parseDocumentContent(ev)
	case parseDocumentEnd:
		return p.parseDocumentEnd(ev)
	case parseBlockNode:
		return p.parseNode(ev, true, false)
	case parseBlockNodeOrIndentlessSequence:
		return p.parseNode(ev, true, true)
	case parseFlowNode:
		return p.parseNode(ev, false, false)
	case parseBlockSequenceFirstEntry:
		return p.parseBlockSequenceEntry(ev, true)
	case parseBlockSequenceEntry:
		return p.parseBlockSequenceEntry(ev, false)
	case parseIndentlessSequenceEntry:
		return p.parseIndentlessSequenceEntry(ev)
	case parseBlockMappingFirstKey:
		return p.parseBlockMappingKey(ev, true)
	case parseBlockMappingKey:
		return p.parseBlockMappingKey(ev, false)
	case parseBlockMappingValue:
		return p.parseBlockMappingValue(ev)
	case parseFlowSequenceFirstEntry:
		return p.parseFlowSequenceEntry(ev, true)
	case parseFlowSequenceEntry:
		return p.parseFlowSequenceEntry(ev, false)
	case parseFlowSequenceEntryMappingKey:
		return p.parseFlowSequenceEntryMappingKey(ev)
	case parseFlowSequenceEntryMappingValue:
		return p.parseFlowSequenceEntryMappingValue(ev)
	case parseFlowSequenceEntryMappingEnd:
		return p.parseFlowSequenceEntryMappingEnd(ev)
	case parseFlowMappingFirstKey:
		return p.parseFlowMappingKey(ev, true)
	case parseFlowMappingKey:
		return p.parseFlowMappingKey(ev, false)
	case parseFlowMappingValue:
		return p.parseFlowMappingValue(ev, false)
	case parseFlowMappingEmptyValue:
		return p.parseFlowMappingValue(ev, true)
	}
	panic("yaml: invalid parser state")
}

// stream ::= STREAM-START implicit_document? explicit_document* STREAM-END
func (p *parser) parseStreamStart(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	p.state = parseImplicitDocumentStart
	*ev = event{kind: evStreamStart, start: t.start, end: t.end}
	p.skip()
	return true
}

// implicit_document ::= block_node DOCUMENT-END*
// explicit_document ::= DIRECTIVE* DOCUMENT-START block_node? DOCUMENT-END*
func (p *parser) parseDocumentStart(ev *event, implicit bool) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	// Extra document end indicators.
	if !implicit {
		for t.kind == tokDocEnd {
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
		}
	}
	switch {
	case implicit && t.kind != tokVersionDirective && t.kind != tokTagDirective &&
		t.kind != tokDocStart && t.kind != tokStreamEnd:
		// An implicit document.
		if !p.processDirectives() {
			return false
		}
		p.states = append(p.states, parseDocumentEnd)
		p.state = parseBlockNode
		*ev = event{kind: evDocumentStart, start: t.start, end: t.end}
	case t.kind != tokStreamEnd:
		// An explicit document.
		start := t.start
		if !p.processDirectives() {
			return false
		}
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokDocStart {
			return p.parserError("", "did not find expected <document start>", t.start)
		}
		p.states = append(p.states, parseDocumentEnd)
		p.state = parseDocumentContent
		*ev = event{kind: evDocumentStart, start: start, end: t.end}
		p.skip()
	default:
		p.state = parseEnd
		*ev = event{kind: evStreamEnd, start: t.start, end: t.end}
		p.skip()
	}
	return true
}

func (p *parser) parseDocumentContent(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	switch t.kind {
	case tokVersionDirective, tokTagDirective, tokDocStart, tokDocEnd, tokStreamEnd:
		p.state = p.pop()
		*ev = emptyScalar(t.start)
		return true
	}
	return p.parseNode(ev, true, false)
}

func (p *parser) parseDocumentEnd(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	start, end := t.start, t.start
	if t.kind == tokDocEnd {
		end = t.end
		p.skip()
	}
	p.tags = p.tags[:0]
	p.state = parseDocumentStart
	*ev = event{kind: evDocumentEnd, start: start, end: end}
	return true
}

// parseNode parses
//
//	block_node_or_indentless_sequence ::= ALIAS | properties (block_content | indentless_block_sequence)? | block_content | indentless_block_sequence
//	block_node ::= ALIAS | properties block_content? | block_content
//	flow_node  ::= ALIAS | properties flow_content? | flow_content
//	properties ::= TAG ANCHOR? | ANCHOR TAG?
func (p *parser) parseNode(ev *event, block, indentlessSequence bool) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind == tokAlias {
		p.state = p.pop()
		*ev = event{kind: evAlias, start: t.start, end: t.end, anchor: t.val}
		p.skip()
		return true
	}

	start, end := t.start, t.start
	var anchor, tagHandle, tagSuffix string
	var tagMark mark
	hasTag := false
	if t.kind == tokAnchor {
		anchor = t.val
		start, end = t.start, t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind == tokTag {
			hasTag = true
			tagHandle, tagSuffix, tagMark, end = t.val, t.suffix, t.start, t.end
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
		}
	} else if t.kind == tokTag {
		hasTag = true
		tagHandle, tagSuffix = t.val, t.suffix
		start, tagMark, end = t.start, t.start, t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind == tokAnchor {
			anchor, end = t.val, t.end
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
		}
	}

	tag := ""
	if hasTag {
		if tagHandle == "" {
			tag = tagSuffix
		} else {
			found := false
			for _, d := range p.tags {
				if d.handle == tagHandle {
					tag, found = d.prefix+tagSuffix, true
					break
				}
			}
			if !found {
				return p.parserError("while parsing a node", "found undefined tag handle", tagMark)
			}
		}
	}

	if indentlessSequence && t.kind == tokBlockEntry {
		p.state = parseIndentlessSequenceEntry
		*ev = event{kind: evSequenceStart, start: start, end: t.end, anchor: anchor, tag: tag}
		return true
	}
	switch {
	case t.kind == tokScalar:
		p.state = p.pop()
		*ev = event{kind: evScalar, start: start, end: t.end, anchor: anchor, tag: tag, value: t.val, style: t.style}
		p.skip()
		return true
	case t.kind == tokFlowSeqStart:
		p.state = parseFlowSequenceFirstEntry
		*ev = event{kind: evSequenceStart, start: start, end: t.end, anchor: anchor, tag: tag, style: FlowStyle}
		return true
	case t.kind == tokFlowMapStart:
		p.state = parseFlowMappingFirstKey
		*ev = event{kind: evMappingStart, start: start, end: t.end, anchor: anchor, tag: tag, style: FlowStyle}
		return true
	case block && t.kind == tokBlockSeqStart:
		p.state = parseBlockSequenceFirstEntry
		*ev = event{kind: evSequenceStart, start: start, end: t.end, anchor: anchor, tag: tag}
		return true
	case block && t.kind == tokBlockMapStart:
		p.state = parseBlockMappingFirstKey
		*ev = event{kind: evMappingStart, start: start, end: t.end, anchor: anchor, tag: tag}
		return true
	case anchor != "" || tag != "":
		// Properties with no content: an empty scalar.
		p.state = p.pop()
		*ev = event{kind: evScalar, start: start, end: end, anchor: anchor, tag: tag}
		return true
	}
	context := "while parsing a flow node"
	if block {
		context = "while parsing a block node"
	}
	return p.parserError(context, "did not find expected node content", t.start)
}

// block_sequence ::= BLOCK-SEQUENCE-START (BLOCK-ENTRY block_node?)* BLOCK-END
func (p *parser) parseBlockSequenceEntry(ev *event, first bool) bool {
	if first {
		t := p.peek()
		p.marks = append(p.marks, t.start)
		p.skip()
	}
	t := p.peek()
	if t == nil {
		return false
	}
	switch t.kind {
	case tokBlockEntry:
		m := t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokBlockEntry && t.kind != tokBlockEnd {
			p.states = append(p.states, parseBlockSequenceEntry)
			return p.parseNode(ev, true, false)
		}
		p.state = parseBlockSequenceEntry
		*ev = emptyScalar(m)
		return true
	case tokBlockEnd:
		p.state = p.pop()
		p.popMark()
		*ev = event{kind: evSequenceEnd, start: t.start, end: t.end}
		p.skip()
		return true
	}
	p.popMark()
	return p.parserError("while parsing a block collection", "did not find expected '-' indicator", t.start)
}

// indentless_sequence ::= (BLOCK-ENTRY block_node?)+
func (p *parser) parseIndentlessSequenceEntry(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind == tokBlockEntry {
		m := t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokBlockEntry && t.kind != tokKey && t.kind != tokValue && t.kind != tokBlockEnd {
			p.states = append(p.states, parseIndentlessSequenceEntry)
			return p.parseNode(ev, true, false)
		}
		p.state = parseIndentlessSequenceEntry
		*ev = emptyScalar(m)
		return true
	}
	p.state = p.pop()
	*ev = event{kind: evSequenceEnd, start: t.start, end: t.start}
	return true
}

// block_mapping ::= BLOCK-MAPPING_START
//
//	((KEY block_node_or_indentless_sequence?)?
//	(VALUE block_node_or_indentless_sequence?)?)*
//	BLOCK-END
func (p *parser) parseBlockMappingKey(ev *event, first bool) bool {
	if first {
		t := p.peek()
		p.marks = append(p.marks, t.start)
		p.skip()
	}
	t := p.peek()
	if t == nil {
		return false
	}
	switch t.kind {
	case tokKey:
		m := t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokKey && t.kind != tokValue && t.kind != tokBlockEnd {
			p.states = append(p.states, parseBlockMappingValue)
			return p.parseNode(ev, true, true)
		}
		p.state = parseBlockMappingValue
		*ev = emptyScalar(m)
		return true
	case tokBlockEnd:
		p.state = p.pop()
		p.popMark()
		*ev = event{kind: evMappingEnd, start: t.start, end: t.end}
		p.skip()
		return true
	}
	p.popMark()
	return p.parserError("while parsing a block mapping", "did not find expected key", t.start)
}

func (p *parser) parseBlockMappingValue(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind == tokValue {
		m := t.end
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokKey && t.kind != tokValue && t.kind != tokBlockEnd {
			p.states = append(p.states, parseBlockMappingKey)
			return p.parseNode(ev, true, true)
		}
		p.state = parseBlockMappingKey
		*ev = emptyScalar(m)
		return true
	}
	p.state = parseBlockMappingKey
	*ev = emptyScalar(t.start)
	return true
}

// flow_sequence ::= FLOW-SEQUENCE-START (flow_sequence_entry FLOW-ENTRY)* flow_sequence_entry? FLOW-SEQUENCE-END
// flow_sequence_entry ::= flow_node | KEY flow_node? (VALUE flow_node?)?
func (p *parser) parseFlowSequenceEntry(ev *event, first bool) bool {
	if first {
		t := p.peek()
		p.marks = append(p.marks, t.start)
		p.skip()
	}
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind != tokFlowSeqEnd {
		if !first {
			if t.kind != tokFlowEntry {
				p.popMark()
				return p.parserError("while parsing a flow sequence", "did not find expected ',' or ']'", t.start)
			}
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
		}
		if t.kind == tokKey {
			p.state = parseFlowSequenceEntryMappingKey
			*ev = event{kind: evMappingStart, start: t.start, end: t.end, style: FlowStyle}
			p.skip()
			return true
		}
		if t.kind != tokFlowSeqEnd {
			p.states = append(p.states, parseFlowSequenceEntry)
			return p.parseNode(ev, false, false)
		}
	}
	p.state = p.pop()
	p.popMark()
	*ev = event{kind: evSequenceEnd, start: t.start, end: t.end}
	p.skip()
	return true
}

func (p *parser) parseFlowSequenceEntryMappingKey(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind != tokValue && t.kind != tokFlowEntry && t.kind != tokFlowSeqEnd {
		p.states = append(p.states, parseFlowSequenceEntryMappingValue)
		return p.parseNode(ev, false, false)
	}
	m := t.end
	p.skip()
	p.state = parseFlowSequenceEntryMappingValue
	*ev = emptyScalar(m)
	return true
}

func (p *parser) parseFlowSequenceEntryMappingValue(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind == tokValue {
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokFlowEntry && t.kind != tokFlowSeqEnd {
			p.states = append(p.states, parseFlowSequenceEntryMappingEnd)
			return p.parseNode(ev, false, false)
		}
	}
	p.state = parseFlowSequenceEntryMappingEnd
	*ev = emptyScalar(t.start)
	return true
}

func (p *parser) parseFlowSequenceEntryMappingEnd(ev *event) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	p.state = parseFlowSequenceEntry
	*ev = event{kind: evMappingEnd, start: t.start, end: t.start}
	return true
}

// flow_mapping ::= FLOW-MAPPING-START (flow_mapping_entry FLOW-ENTRY)* flow_mapping_entry? FLOW-MAPPING-END
// flow_mapping_entry ::= flow_node | KEY flow_node? (VALUE flow_node?)?
func (p *parser) parseFlowMappingKey(ev *event, first bool) bool {
	if first {
		t := p.peek()
		p.marks = append(p.marks, t.start)
		p.skip()
	}
	t := p.peek()
	if t == nil {
		return false
	}
	if t.kind != tokFlowMapEnd {
		if !first {
			if t.kind != tokFlowEntry {
				p.popMark()
				return p.parserError("while parsing a flow mapping", "did not find expected ',' or '}'", t.start)
			}
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
		}
		if t.kind == tokKey {
			p.skip()
			if t = p.peek(); t == nil {
				return false
			}
			if t.kind != tokValue && t.kind != tokFlowEntry && t.kind != tokFlowMapEnd {
				p.states = append(p.states, parseFlowMappingValue)
				return p.parseNode(ev, false, false)
			}
			p.state = parseFlowMappingValue
			*ev = emptyScalar(t.start)
			return true
		}
		if t.kind != tokFlowMapEnd {
			p.states = append(p.states, parseFlowMappingEmptyValue)
			return p.parseNode(ev, false, false)
		}
	}
	p.state = p.pop()
	p.popMark()
	*ev = event{kind: evMappingEnd, start: t.start, end: t.end}
	p.skip()
	return true
}

func (p *parser) parseFlowMappingValue(ev *event, empty bool) bool {
	t := p.peek()
	if t == nil {
		return false
	}
	if empty {
		p.state = parseFlowMappingKey
		*ev = emptyScalar(t.start)
		return true
	}
	if t.kind == tokValue {
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
		if t.kind != tokFlowEntry && t.kind != tokFlowMapEnd {
			p.states = append(p.states, parseFlowMappingKey)
			return p.parseNode(ev, false, false)
		}
	}
	p.state = parseFlowMappingKey
	*ev = emptyScalar(t.start)
	return true
}

func emptyScalar(m mark) event {
	return event{kind: evScalar, start: m, end: m}
}

// processDirectives consumes %YAML and %TAG directives and installs the
// document's tag handles.
func (p *parser) processDirectives() bool {
	t := p.peek()
	if t == nil {
		return false
	}
	seenVersion := false
	for t.kind == tokVersionDirective || t.kind == tokTagDirective {
		if t.kind == tokVersionDirective {
			if seenVersion {
				return p.parserError("", "found duplicate %YAML directive", t.start)
			}
			if t.major != 1 || (t.minor != 1 && t.minor != 2) {
				return p.parserError("", "found incompatible YAML document", t.start)
			}
			seenVersion = true
		} else if !p.appendTagDirective(tagDirective{t.val, t.suffix}, false, t.start) {
			return false
		}
		p.skip()
		if t = p.peek(); t == nil {
			return false
		}
	}
	for _, d := range defaultTagDirectives {
		p.appendTagDirective(d, true, t.start)
	}
	return true
}

func (p *parser) appendTagDirective(d tagDirective, allowDuplicates bool, m mark) bool {
	for _, have := range p.tags {
		if have.handle == d.handle {
			if allowDuplicates {
				return true
			}
			return p.parserError("", "found duplicate %TAG directive", m)
		}
	}
	p.tags = append(p.tags, d)
	return true
}
