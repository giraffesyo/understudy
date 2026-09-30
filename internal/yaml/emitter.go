package yaml

import (
	"strings"
	"unicode/utf8"
)

// emitter writes an emitEvent stream as YAML text exactly as libyaml 0.2.5's
// emitter (emitter.c) does: ansible-core's to_yaml/to_nice_yaml dump
// through PyYAML's CSafeDumper, so the scalar style choices, line folding
// at the best width, indentation and the flow/block layout are libyaml's.
//
// It is a port of the C state machine. libyaml accumulates a few events of
// lookahead before emitting (to decide empty collections and simple keys);
// here the whole stream is built first and the emitter peeks ahead in it.

type emitKind int

const (
	eeStreamStart emitKind = iota
	eeStreamEnd
	eeDocStart
	eeDocEnd
	eeAlias
	eeScalar
	eeSeqStart
	eeSeqEnd
	eeMapStart
	eeMapEnd
)

type scalarStyle int

const (
	styleAny scalarStyle = iota
	stylePlain
	styleSingle
	styleDouble
	styleLiteral
	styleFolded
)

type emitEvent struct {
	kind           emitKind
	anchor         string
	tag            string
	value          string
	implicit       bool // plain_implicit for scalars; implicit otherwise
	quotedImplicit bool
	style          scalarStyle
	flow           bool // collections: flow style requested
}

type emitState int

const (
	stStreamStart emitState = iota
	stFirstDocStart
	stDocStart
	stDocContent
	stDocEnd
	stFlowSeqFirstItem
	stFlowSeqItem
	stFlowMapFirstKey
	stFlowMapKey
	stFlowMapSimpleValue
	stFlowMapValue
	stBlockSeqFirstItem
	stBlockSeqItem
	stBlockMapFirstKey
	stBlockMapKey
	stBlockMapSimpleValue
	stBlockMapValue
	stEnd
)

type emitter struct {
	out strings.Builder

	canonical  bool
	bestIndent int
	bestWidth  int
	unicode    bool
	lineBreak  string

	events []emitEvent
	head   int

	states []emitState
	state  emitState

	indents []int
	indent  int

	flowLevel int

	rootContext      bool
	sequenceContext  bool
	mappingContext   bool
	simpleKeyContext bool

	line       int
	column     int
	whitespace bool
	indention  bool
	openEnded  int

	anchor struct {
		anchor string
		alias  bool
	}
	tagData struct{ handle, suffix string }
	scalar  struct {
		value               string
		multiline           bool
		flowPlainAllowed    bool
		blockPlainAllowed   bool
		singleQuotedAllowed bool
		blockAllowed        bool
		style               scalarStyle
	}
	tagDirectives []tagDirective
}

type emitterError string

func (e emitterError) Error() string { return string(e) }

// emit runs the whole emitEvent stream through the state machine.
func (e *emitter) emit(events []emitEvent) (string, error) {
	e.events = events
	for e.head = 0; e.head < len(e.events); e.head++ {
		ev := &e.events[e.head]
		if err := e.analyzeEvent(ev); err != nil {
			return "", err
		}
		if err := e.stateMachine(ev); err != nil {
			return "", err
		}
	}
	return e.out.String(), nil
}

// ---- output primitives ------------------------------------------------

func (e *emitter) put(c byte) {
	e.out.WriteByte(c)
	e.column++
}

func (e *emitter) putBreak() {
	e.out.WriteString(e.lineBreak)
	e.column = 0
	e.line++
}

// write copies one character of s at i, returning its width.
func (e *emitter) write(s string, i int) int {
	w := charWidth(s[i])
	e.out.WriteString(s[i : i+w])
	e.column++
	return w
}

func (e *emitter) writeAll(s string) {
	for i := 0; i < len(s); {
		i += e.write(s, i)
	}
}

// writeBreak copies a line break character from s at i.
func (e *emitter) writeBreak(s string, i int) int {
	if s[i] == '\n' {
		e.putBreak()
		return 1
	}
	w := e.write(s, i)
	e.column = 0
	e.line++
	return w
}

func (e *emitter) increaseIndent(flow, indentless bool) {
	e.indents = append(e.indents, e.indent)
	if e.indent < 0 {
		if flow {
			e.indent = e.bestIndent
		} else {
			e.indent = 0
		}
	} else if !indentless {
		e.indent += e.bestIndent
	}
}

func (e *emitter) popIndent() {
	e.indent = e.indents[len(e.indents)-1]
	e.indents = e.indents[:len(e.indents)-1]
}

func (e *emitter) popState() {
	e.state = e.states[len(e.states)-1]
	e.states = e.states[:len(e.states)-1]
}

// ---- state machine ----------------------------------------------------

func (e *emitter) stateMachine(ev *emitEvent) error {
	switch e.state {
	case stStreamStart:
		return e.emitStreamStart(ev)
	case stFirstDocStart:
		return e.emitDocumentStart(ev, true)
	case stDocStart:
		return e.emitDocumentStart(ev, false)
	case stDocContent:
		e.states = append(e.states, stDocEnd)
		return e.emitNode(ev, true, false, false, false)
	case stDocEnd:
		return e.emitDocumentEnd(ev)
	case stFlowSeqFirstItem:
		return e.emitFlowSequenceItem(ev, true)
	case stFlowSeqItem:
		return e.emitFlowSequenceItem(ev, false)
	case stFlowMapFirstKey:
		return e.emitFlowMappingKey(ev, true)
	case stFlowMapKey:
		return e.emitFlowMappingKey(ev, false)
	case stFlowMapSimpleValue:
		return e.emitFlowMappingValue(ev, true)
	case stFlowMapValue:
		return e.emitFlowMappingValue(ev, false)
	case stBlockSeqFirstItem:
		return e.emitBlockSequenceItem(ev, true)
	case stBlockSeqItem:
		return e.emitBlockSequenceItem(ev, false)
	case stBlockMapFirstKey:
		return e.emitBlockMappingKey(ev, true)
	case stBlockMapKey:
		return e.emitBlockMappingKey(ev, false)
	case stBlockMapSimpleValue:
		return e.emitBlockMappingValue(ev, true)
	case stBlockMapValue:
		return e.emitBlockMappingValue(ev, false)
	}
	return emitterError("expected nothing after STREAM-END")
}

func (e *emitter) emitStreamStart(ev *emitEvent) error {
	if ev.kind != eeStreamStart {
		return emitterError("expected STREAM-START")
	}
	if e.bestIndent < 2 || e.bestIndent > 9 {
		e.bestIndent = 2
	}
	if e.bestWidth >= 0 && e.bestWidth <= e.bestIndent*2 {
		e.bestWidth = 80
	}
	if e.bestWidth < 0 {
		e.bestWidth = 1<<31 - 1
	}
	if e.lineBreak == "" {
		e.lineBreak = "\n"
	}
	e.indent = -1
	e.whitespace = true
	e.indention = true
	e.state = stFirstDocStart
	return nil
}

func (e *emitter) emitDocumentStart(ev *emitEvent, first bool) error {
	switch ev.kind {
	case eeDocStart:
		e.tagDirectives = append(e.tagDirectives[:0], defaultTagDirectives...)
		implicit := ev.implicit
		if !first || e.canonical {
			implicit = false
		}
		if !implicit {
			e.writeIndent()
			e.writeIndicator("---", true, false, false)
			if e.canonical {
				e.writeIndent()
			}
		}
		e.state = stDocContent
		e.openEnded = 0
		return nil
	case eeStreamEnd:
		// A block scalar with trailing empty lines ("|+") at the end of
		// the stream.
		if e.openEnded == 2 {
			e.writeIndicator("...", true, false, false)
			e.openEnded = 0
			e.writeIndent()
		}
		e.state = stEnd
		return nil
	}
	return emitterError("expected DOCUMENT-START or STREAM-END")
}

func (e *emitter) emitDocumentEnd(ev *emitEvent) error {
	if ev.kind != eeDocEnd {
		return emitterError("expected DOCUMENT-END")
	}
	e.writeIndent()
	if !ev.implicit {
		e.writeIndicator("...", true, false, false)
		e.openEnded = 0
		e.writeIndent()
	}
	e.state = stDocStart
	return nil
}

func (e *emitter) emitFlowSequenceItem(ev *emitEvent, first bool) error {
	if first {
		e.writeIndicator("[", true, true, false)
		e.increaseIndent(true, false)
		e.flowLevel++
	}
	if ev.kind == eeSeqEnd {
		e.flowLevel--
		e.popIndent()
		if e.canonical && !first {
			e.writeIndicator(",", false, false, false)
			e.writeIndent()
		}
		e.writeIndicator("]", false, false, false)
		e.popState()
		return nil
	}
	if !first {
		e.writeIndicator(",", false, false, false)
	}
	if e.canonical || e.column > e.bestWidth {
		e.writeIndent()
	}
	e.states = append(e.states, stFlowSeqItem)
	return e.emitNode(ev, false, true, false, false)
}

func (e *emitter) emitFlowMappingKey(ev *emitEvent, first bool) error {
	if first {
		e.writeIndicator("{", true, true, false)
		e.increaseIndent(true, false)
		e.flowLevel++
	}
	if ev.kind == eeMapEnd {
		e.flowLevel--
		e.popIndent()
		if e.canonical && !first {
			e.writeIndicator(",", false, false, false)
			e.writeIndent()
		}
		e.writeIndicator("}", false, false, false)
		e.popState()
		return nil
	}
	if !first {
		e.writeIndicator(",", false, false, false)
	}
	if e.canonical || e.column > e.bestWidth {
		e.writeIndent()
	}
	if !e.canonical && e.checkSimpleKey() {
		e.states = append(e.states, stFlowMapSimpleValue)
		return e.emitNode(ev, false, false, true, true)
	}
	e.writeIndicator("?", true, false, false)
	e.states = append(e.states, stFlowMapValue)
	return e.emitNode(ev, false, false, true, false)
}

func (e *emitter) emitFlowMappingValue(ev *emitEvent, simple bool) error {
	if simple {
		e.writeIndicator(":", false, false, false)
	} else {
		if e.canonical || e.column > e.bestWidth {
			e.writeIndent()
		}
		e.writeIndicator(":", true, false, false)
	}
	e.states = append(e.states, stFlowMapKey)
	return e.emitNode(ev, false, false, true, false)
}

func (e *emitter) emitBlockSequenceItem(ev *emitEvent, first bool) error {
	if first {
		e.increaseIndent(false, e.mappingContext && !e.indention)
	}
	if ev.kind == eeSeqEnd {
		e.popIndent()
		e.popState()
		return nil
	}
	e.writeIndent()
	e.writeIndicator("-", true, false, true)
	e.states = append(e.states, stBlockSeqItem)
	return e.emitNode(ev, false, true, false, false)
}

func (e *emitter) emitBlockMappingKey(ev *emitEvent, first bool) error {
	if first {
		e.increaseIndent(false, false)
	}
	if ev.kind == eeMapEnd {
		e.popIndent()
		e.popState()
		return nil
	}
	e.writeIndent()
	if e.checkSimpleKey() {
		e.states = append(e.states, stBlockMapSimpleValue)
		return e.emitNode(ev, false, false, true, true)
	}
	e.writeIndicator("?", true, false, true)
	e.states = append(e.states, stBlockMapValue)
	return e.emitNode(ev, false, false, true, false)
}

func (e *emitter) emitBlockMappingValue(ev *emitEvent, simple bool) error {
	if simple {
		e.writeIndicator(":", false, false, false)
	} else {
		e.writeIndent()
		e.writeIndicator(":", true, false, true)
	}
	e.states = append(e.states, stBlockMapKey)
	return e.emitNode(ev, false, false, true, false)
}

func (e *emitter) emitNode(ev *emitEvent, root, sequence, mapping, simpleKey bool) error {
	e.rootContext = root
	e.sequenceContext = sequence
	e.mappingContext = mapping
	e.simpleKeyContext = simpleKey
	switch ev.kind {
	case eeAlias:
		e.processAnchor()
		if e.simpleKeyContext {
			e.put(' ')
		}
		e.popState()
		return nil
	case eeScalar:
		if err := e.selectScalarStyle(ev); err != nil {
			return err
		}
		e.processAnchor()
		e.processTag()
		e.increaseIndent(true, false)
		e.processScalar()
		e.popIndent()
		e.popState()
		return nil
	case eeSeqStart:
		e.processAnchor()
		e.processTag()
		if e.flowLevel > 0 || e.canonical || ev.flow || e.checkEmpty(eeSeqStart, eeSeqEnd) {
			e.state = stFlowSeqFirstItem
		} else {
			e.state = stBlockSeqFirstItem
		}
		return nil
	case eeMapStart:
		e.processAnchor()
		e.processTag()
		if e.flowLevel > 0 || e.canonical || ev.flow || e.checkEmpty(eeMapStart, eeMapEnd) {
			e.state = stFlowMapFirstKey
		} else {
			e.state = stBlockMapFirstKey
		}
		return nil
	}
	return emitterError("expected SCALAR, SEQUENCE-START, MAPPING-START, or ALIAS")
}

func (e *emitter) checkEmpty(start, end emitKind) bool {
	return e.head+1 < len(e.events) && e.events[e.head].kind == start && e.events[e.head+1].kind == end
}

func (e *emitter) checkSimpleKey() bool {
	ev := &e.events[e.head]
	length := 0
	switch ev.kind {
	case eeAlias:
		length += len(e.anchor.anchor)
	case eeScalar:
		if e.scalar.multiline {
			return false
		}
		length += len(e.anchor.anchor) + len(e.tagData.handle) + len(e.tagData.suffix) + len(e.scalar.value)
	case eeSeqStart:
		if !e.checkEmpty(eeSeqStart, eeSeqEnd) {
			return false
		}
		length += len(e.anchor.anchor) + len(e.tagData.handle) + len(e.tagData.suffix)
	case eeMapStart:
		if !e.checkEmpty(eeMapStart, eeMapEnd) {
			return false
		}
		length += len(e.anchor.anchor) + len(e.tagData.handle) + len(e.tagData.suffix)
	default:
		return false
	}
	return length <= 128
}

func (e *emitter) selectScalarStyle(ev *emitEvent) error {
	noTag := e.tagData.handle == "" && e.tagData.suffix == ""
	if noTag && !ev.implicit && !ev.quotedImplicit {
		return emitterError("neither tag nor implicit flags are specified")
	}
	style := ev.style
	if style == styleAny {
		style = stylePlain
	}
	if e.canonical {
		style = styleDouble
	}
	if e.simpleKeyContext && e.scalar.multiline {
		style = styleDouble
	}
	if style == stylePlain {
		if (e.flowLevel > 0 && !e.scalar.flowPlainAllowed) || (e.flowLevel == 0 && !e.scalar.blockPlainAllowed) {
			style = styleSingle
		}
		if e.scalar.value == "" && (e.flowLevel > 0 || e.simpleKeyContext) {
			style = styleSingle
		}
		if noTag && !ev.implicit {
			style = styleSingle
		}
	}
	if style == styleSingle && !e.scalar.singleQuotedAllowed {
		style = styleDouble
	}
	if style == styleLiteral || style == styleFolded {
		if !e.scalar.blockAllowed || e.flowLevel > 0 || e.simpleKeyContext {
			style = styleDouble
		}
	}
	if noTag && !ev.quotedImplicit && style != stylePlain {
		e.tagData.handle = "!"
	}
	e.scalar.style = style
	return nil
}

func (e *emitter) processAnchor() {
	if e.anchor.anchor == "" {
		return
	}
	ind := "&"
	if e.anchor.alias {
		ind = "*"
	}
	e.writeIndicator(ind, true, false, false)
	e.writeAll(e.anchor.anchor)
	e.whitespace = false
	e.indention = false
}

func (e *emitter) processTag() {
	if e.tagData.handle == "" && e.tagData.suffix == "" {
		return
	}
	if e.tagData.handle != "" {
		e.writeTagHandle(e.tagData.handle)
		if e.tagData.suffix != "" {
			e.writeTagContent(e.tagData.suffix, false)
		}
		return
	}
	e.writeIndicator("!<", true, false, false)
	e.writeTagContent(e.tagData.suffix, false)
	e.writeIndicator(">", false, false, false)
}

func (e *emitter) processScalar() {
	v := e.scalar.value
	switch e.scalar.style {
	case stylePlain:
		e.writePlain(v, !e.simpleKeyContext)
	case styleSingle:
		e.writeSingleQuoted(v, !e.simpleKeyContext)
	case styleDouble:
		e.writeDoubleQuoted(v, !e.simpleKeyContext)
	case styleLiteral:
		e.writeLiteral(v)
	case styleFolded:
		e.writeFolded(v)
	}
}

// ---- analysis ----------------------------------------------------------

func (e *emitter) analyzeEvent(ev *emitEvent) error {
	e.anchor.anchor = ""
	e.anchor.alias = false
	e.tagData.handle = ""
	e.tagData.suffix = ""
	e.scalar.value = ""
	switch ev.kind {
	case eeAlias:
		e.anchor.anchor = ev.anchor
		e.anchor.alias = true
	case eeScalar:
		e.anchor.anchor = ev.anchor
		if ev.tag != "" && (e.canonical || (!ev.implicit && !ev.quotedImplicit)) {
			e.analyzeTag(ev.tag)
		}
		e.analyzeScalar(ev.value)
	case eeSeqStart, eeMapStart:
		e.anchor.anchor = ev.anchor
		if ev.tag != "" && (e.canonical || !ev.implicit) {
			e.analyzeTag(ev.tag)
		}
	}
	return nil
}

func (e *emitter) analyzeTag(tag string) {
	for _, d := range e.tagDirectives {
		if len(d.prefix) < len(tag) && strings.HasPrefix(tag, d.prefix) {
			e.tagData.handle = d.handle
			e.tagData.suffix = tag[len(d.prefix):]
			return
		}
	}
	e.tagData.suffix = tag
}

func (e *emitter) analyzeScalar(value string) {
	s := &e.scalar
	s.value = value
	if value == "" {
		s.multiline = false
		s.flowPlainAllowed = false
		s.blockPlainAllowed = true
		s.singleQuotedAllowed = true
		s.blockAllowed = false
		return
	}
	var blockIndicators, flowIndicators, lineBreaks, specialCharacters bool
	var leadingSpace, leadingBreak, trailingSpace, trailingBreak bool
	var breakSpace, spaceBreak, previousSpace, previousBreak bool

	if strings.HasPrefix(value, "---") || strings.HasPrefix(value, "...") {
		blockIndicators = true
		flowIndicators = true
	}
	precededByWhitespace := true
	followedByWhitespace := isBlankz(value, charWidth(value[0]))
	for i := 0; i < len(value); {
		c := value[i]
		if i == 0 {
			switch c {
			case '#', ',', '[', ']', '{', '}', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
				flowIndicators = true
				blockIndicators = true
			case '?', ':':
				flowIndicators = true
				if followedByWhitespace {
					blockIndicators = true
				}
			case '-':
				if followedByWhitespace {
					flowIndicators = true
					blockIndicators = true
				}
			}
		} else {
			switch c {
			case ',', '?', '[', ']', '{', '}':
				flowIndicators = true
			case ':':
				flowIndicators = true
				if followedByWhitespace {
					blockIndicators = true
				}
			case '#':
				if precededByWhitespace {
					flowIndicators = true
					blockIndicators = true
				}
			}
		}
		if !isPrintable(value, i) || (!isASCII(value, i) && !e.unicode) {
			specialCharacters = true
		}
		if isBreak(value, i) {
			lineBreaks = true
		}
		w := charWidth(c)
		if isSpace(value, i) {
			if i == 0 {
				leadingSpace = true
			}
			if i+w == len(value) {
				trailingSpace = true
			}
			if previousBreak {
				breakSpace = true
			}
			previousSpace = true
			previousBreak = false
		} else if isBreak(value, i) {
			if i == 0 {
				leadingBreak = true
			}
			if i+w == len(value) {
				trailingBreak = true
			}
			if previousSpace {
				spaceBreak = true
			}
			previousSpace = false
			previousBreak = true
		} else {
			previousSpace = false
			previousBreak = false
		}
		precededByWhitespace = isBlankz(value, i)
		i += w
		if i < len(value) {
			followedByWhitespace = isBlankz(value, i+charWidth(value[i]))
		}
	}

	s.multiline = lineBreaks
	s.flowPlainAllowed = true
	s.blockPlainAllowed = true
	s.singleQuotedAllowed = true
	s.blockAllowed = true
	if leadingSpace || leadingBreak || trailingSpace || trailingBreak {
		s.flowPlainAllowed = false
		s.blockPlainAllowed = false
	}
	if trailingSpace {
		s.blockAllowed = false
	}
	if breakSpace {
		s.flowPlainAllowed = false
		s.blockPlainAllowed = false
		s.singleQuotedAllowed = false
	}
	if spaceBreak || specialCharacters {
		s.flowPlainAllowed = false
		s.blockPlainAllowed = false
		s.singleQuotedAllowed = false
		s.blockAllowed = false
	}
	if lineBreaks {
		s.flowPlainAllowed = false
		s.blockPlainAllowed = false
	}
	if flowIndicators {
		s.flowPlainAllowed = false
	}
	if blockIndicators {
		s.blockPlainAllowed = false
	}
}

// ---- writers -----------------------------------------------------------

func (e *emitter) writeIndent() {
	indent := e.indent
	if indent < 0 {
		indent = 0
	}
	if !e.indention || e.column > indent || (e.column == indent && !e.whitespace) {
		e.putBreak()
	}
	for e.column < indent {
		e.put(' ')
	}
	e.whitespace = true
	e.indention = true
}

func (e *emitter) writeIndicator(ind string, needWhitespace, isWhitespace, isIndention bool) {
	if needWhitespace && !e.whitespace {
		e.put(' ')
	}
	e.writeAll(ind)
	e.whitespace = isWhitespace
	e.indention = e.indention && isIndention
}

func (e *emitter) writeTagHandle(v string) {
	if !e.whitespace {
		e.put(' ')
	}
	e.writeAll(v)
	e.whitespace = false
	e.indention = false
}

func (e *emitter) writeTagContent(v string, needWhitespace bool) {
	if needWhitespace && !e.whitespace {
		e.put(' ')
	}
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(v); {
		c := v[i]
		if isAlpha(c) || strings.IndexByte(";/?:@&=+$,_.~*'()[]", c) >= 0 {
			i += e.write(v, i)
			continue
		}
		w := charWidth(c)
		for k := 0; k < w; k++ {
			o := v[i]
			i++
			e.put('%')
			e.put(hex[o>>4])
			e.put(hex[o&0x0f])
		}
	}
	e.whitespace = false
	e.indention = false
}

func (e *emitter) writePlain(v string, allowBreaks bool) {
	// No trailing space for an empty value in block context.
	if !e.whitespace && (v != "" || e.flowLevel > 0) {
		e.put(' ')
	}
	spaces, breaks := false, false
	for i := 0; i < len(v); {
		switch {
		case isSpace(v, i):
			if allowBreaks && !spaces && e.column > e.bestWidth && !isSpace(v, i+1) {
				e.writeIndent()
				i++
			} else {
				i += e.write(v, i)
			}
			spaces = true
		case isBreak(v, i):
			if !breaks && v[i] == '\n' {
				e.putBreak()
			}
			i += e.writeBreak(v, i)
			e.indention = true
			breaks = true
		default:
			if breaks {
				e.writeIndent()
			}
			i += e.write(v, i)
			e.indention = false
			spaces = false
			breaks = false
		}
	}
	e.whitespace = false
	e.indention = false
}

func (e *emitter) writeSingleQuoted(v string, allowBreaks bool) {
	e.writeIndicator("'", true, false, false)
	spaces, breaks := false, false
	for i := 0; i < len(v); {
		switch {
		case isSpace(v, i):
			if allowBreaks && !spaces && e.column > e.bestWidth && i != 0 && i != len(v)-1 && !isSpace(v, i+1) {
				e.writeIndent()
				i++
			} else {
				i += e.write(v, i)
			}
			spaces = true
		case isBreak(v, i):
			if !breaks && v[i] == '\n' {
				e.putBreak()
			}
			i += e.writeBreak(v, i)
			e.indention = true
			breaks = true
		default:
			if breaks {
				e.writeIndent()
			}
			if v[i] == '\'' {
				e.put('\'')
			}
			i += e.write(v, i)
			e.indention = false
			spaces = false
			breaks = false
		}
	}
	if breaks {
		e.writeIndent()
	}
	e.writeIndicator("'", false, false, false)
	e.whitespace = false
	e.indention = false
}

func (e *emitter) writeDoubleQuoted(v string, allowBreaks bool) {
	const hex = "0123456789ABCDEF"
	e.writeIndicator("\"", true, false, false)
	spaces := false
	for i := 0; i < len(v); {
		if !isPrintable(v, i) || (!e.unicode && !isASCII(v, i)) || isBOM(v, i) || isBreak(v, i) ||
			v[i] == '"' || v[i] == '\\' {
			r, w := decodeChar(v, i)
			i += w
			e.put('\\')
			switch r {
			case 0x00:
				e.put('0')
			case 0x07:
				e.put('a')
			case 0x08:
				e.put('b')
			case 0x09:
				e.put('t')
			case 0x0A:
				e.put('n')
			case 0x0B:
				e.put('v')
			case 0x0C:
				e.put('f')
			case 0x0D:
				e.put('r')
			case 0x1B:
				e.put('e')
			case 0x22:
				e.put('"')
			case 0x5C:
				e.put('\\')
			case 0x85:
				e.put('N')
			case 0xA0:
				e.put('_')
			case 0x2028:
				e.put('L')
			case 0x2029:
				e.put('P')
			default:
				digits := 8
				switch {
				case r <= 0xFF:
					e.put('x')
					digits = 2
				case r <= 0xFFFF:
					e.put('u')
					digits = 4
				default:
					e.put('U')
				}
				for k := (digits - 1) * 4; k >= 0; k -= 4 {
					e.put(hex[(r>>uint(k))&0x0F])
				}
			}
			spaces = false
		} else if isSpace(v, i) {
			if allowBreaks && !spaces && e.column > e.bestWidth && i != 0 && i != len(v)-1 {
				e.writeIndent()
				if isSpace(v, i+1) {
					e.put('\\')
				}
				i++
			} else {
				i += e.write(v, i)
			}
			spaces = true
		} else {
			i += e.write(v, i)
			spaces = false
		}
	}
	e.writeIndicator("\"", false, false, false)
	e.whitespace = false
	e.indention = false
}

func (e *emitter) writeBlockScalarHints(v string) {
	if isSpace(v, 0) || isBreak(v, 0) {
		e.writeIndicator(string(rune('0'+e.bestIndent)), false, false, false)
	}
	e.openEnded = 0
	chomp := ""
	if v == "" {
		chomp = "-"
	} else {
		i := len(v) - 1
		for v[i]&0xC0 == 0x80 {
			i--
		}
		if !isBreak(v, i) {
			chomp = "-"
		} else if i == 0 {
			chomp = "+"
			e.openEnded = 2
		} else {
			i--
			for v[i]&0xC0 == 0x80 {
				i--
			}
			if isBreak(v, i) {
				chomp = "+"
				e.openEnded = 2
			}
		}
	}
	if chomp != "" {
		e.writeIndicator(chomp, false, false, false)
	}
}

func (e *emitter) writeLiteral(v string) {
	e.writeIndicator("|", true, false, false)
	e.writeBlockScalarHints(v)
	e.putBreak()
	e.indention = true
	e.whitespace = true
	breaks := true
	for i := 0; i < len(v); {
		if isBreak(v, i) {
			i += e.writeBreak(v, i)
			e.indention = true
			breaks = true
		} else {
			if breaks {
				e.writeIndent()
			}
			i += e.write(v, i)
			e.indention = false
			breaks = false
		}
	}
}

func (e *emitter) writeFolded(v string) {
	e.writeIndicator(">", true, false, false)
	e.writeBlockScalarHints(v)
	e.putBreak()
	e.indention = true
	e.whitespace = true
	breaks, leadingSpaces := true, true
	for i := 0; i < len(v); {
		if isBreak(v, i) {
			if !breaks && !leadingSpaces && v[i] == '\n' {
				k := 0
				for isBreak(v, i+k) {
					k += charWidth(v[i+k])
				}
				if !isBlankz(v, i+k) {
					e.putBreak()
				}
			}
			i += e.writeBreak(v, i)
			e.indention = true
			breaks = true
		} else {
			if breaks {
				e.writeIndent()
				leadingSpaces = isBlank(v, i)
			}
			if !breaks && isSpace(v, i) && !isSpace(v, i+1) && e.column > e.bestWidth {
				e.writeIndent()
				i++
			} else {
				i += e.write(v, i)
			}
			e.indention = false
			breaks = false
		}
	}
}

// ---- character classes (libyaml's yaml_private.h macros) ---------------

func charWidth(c byte) int {
	switch {
	case c&0x80 == 0x00:
		return 1
	case c&0xE0 == 0xC0:
		return 2
	case c&0xF0 == 0xE0:
		return 3
	case c&0xF8 == 0xF0:
		return 4
	}
	return 1
}

func decodeChar(s string, i int) (rune, int) {
	r, w := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && w <= 1 {
		return rune(s[i]), 1
	}
	return r, w
}

func atByte(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func isASCII(s string, i int) bool { return atByte(s, i) <= 0x7F }

func isPrintable(s string, i int) bool {
	c, c1, c2 := atByte(s, i), atByte(s, i+1), atByte(s, i+2)
	return c == 0x0A ||
		(c >= 0x20 && c <= 0x7E) ||
		(c == 0xC2 && c1 >= 0xA0) ||
		(c > 0xC2 && c < 0xED) ||
		(c == 0xED && c1 < 0xA0) ||
		c == 0xEE ||
		(c == 0xEF && !(c1 == 0xBB && c2 == 0xBF) && !(c1 == 0xBF && (c2 == 0xBE || c2 == 0xBF)))
}

func isBOM(s string, i int) bool {
	return atByte(s, i) == 0xEF && atByte(s, i+1) == 0xBB && atByte(s, i+2) == 0xBF
}

func isSpace(s string, i int) bool { return atByte(s, i) == ' ' }

func isBlank(s string, i int) bool { c := atByte(s, i); return c == ' ' || c == '\t' }

func isBreak(s string, i int) bool {
	c, c1, c2 := atByte(s, i), atByte(s, i+1), atByte(s, i+2)
	return c == '\r' || c == '\n' ||
		(c == 0xC2 && c1 == 0x85) ||
		(c == 0xE2 && c1 == 0x80 && c2 == 0xA8) ||
		(c == 0xE2 && c1 == 0x80 && c2 == 0xA9)
}

func isBlankz(s string, i int) bool {
	return isBlank(s, i) || isBreak(s, i) || atByte(s, i) == 0
}
