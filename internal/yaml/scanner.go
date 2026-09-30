package yaml

import "strings"

// scanner tokenizes YAML exactly as libyaml 0.2.5 does (ansible-core loads
// YAML through PyYAML's C binding): the same token stream, the same lazy
// token fetching (so the first error hit is the one libyaml reports), the
// same error context/problem text and the same marks.
//
// libyaml owns all indentation logic in the scanner (an indent stack that
// emits BLOCK-*-START/BLOCK-END tokens) and detects simple keys by
// retroactively inserting a KEY token when a ':' follows a possible key.
//
// Marks are 0-based, as in libyaml; Error converts them to 1-based.
type scanner struct {
	src  []byte // the stream, after any byte order mark
	text []byte // the file as read, for the lines errors quote
	name string
	pos  int  // byte offset of the current character
	mark mark // position of the current character

	tokens         []token // queue; tokens[head:] are pending
	head           int
	tokensParsed   int // tokens handed to the parser so far
	tokenAvailable bool

	streamStartProduced bool
	streamEndProduced   bool

	indent           int // current indentation column (-1 at stream level)
	indents          []int
	flowLevel        int
	simpleKeyAllowed bool
	simpleKeys       []simpleKey // one per flow level (plus the block level)

	err *Error
}

type mark struct {
	index, line, column int
}

// simpleKey is a scalar (or anchor/tag/flow collection start) that may turn
// out to be a mapping key once a ':' follows it.
type simpleKey struct {
	possible    bool
	required    bool
	tokenNumber int
	mark        mark
}

// maxDepth caps flow and indentation nesting, as libyaml does.
const maxDepth = 10000

func newScanner(src, text []byte, name string) *scanner {
	return &scanner{src: src, text: text, name: name}
}

// at returns the byte at offset pos+i, or 0 past the end (libyaml's
// end-of-stream NUL).
func (s *scanner) at(i int) byte {
	if s.pos+i < len(s.src) {
		return s.src[s.pos+i]
	}
	return 0
}

func (s *scanner) isZ(i int) bool { return s.pos+i >= len(s.src) }

// isBreakAt reports a line break (CR, LF, NEL, LS, PS) at offset i.
func (s *scanner) isBreakAt(i int) bool {
	switch c := s.at(i); c {
	case '\r', '\n':
		return true
	case 0xC2:
		return s.at(i+1) == 0x85
	case 0xE2:
		return s.at(i+1) == 0x80 && (s.at(i+2) == 0xA8 || s.at(i+2) == 0xA9)
	}
	return false
}

func (s *scanner) isBlankAt(i int) bool       { c := s.at(i); return (c == ' ' || c == '\t') && !s.isZ(i) }
func (s *scanner) isBreakZAt(i int) bool      { return s.isZ(i) || s.isBreakAt(i) }
func (s *scanner) isBlankZAt(i int) bool      { return s.isBlankAt(i) || s.isBreakZAt(i) }
func (s *scanner) isBreak() bool              { return s.isBreakAt(0) }
func (s *scanner) isBlank() bool              { return s.isBlankAt(0) }
func (s *scanner) isBreakZ() bool             { return s.isBreakZAt(0) }
func (s *scanner) isBlankZ() bool             { return s.isBlankZAt(0) }
func (s *scanner) check(c byte) bool          { return !s.isZ(0) && s.src[s.pos] == c }
func (s *scanner) checkAt(c byte, i int) bool { return !s.isZ(i) && s.src[s.pos+i] == c }

func isAlpha(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '-'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

func asHex(c byte) int {
	switch {
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c - '0')
}

// width is the byte length of the UTF-8 sequence starting with c.
func width(c byte) int {
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
	return 0
}

// skip consumes one non-break character.
func (s *scanner) skip() {
	s.mark.index++
	s.mark.column++
	if w := width(s.src[s.pos]); w > 0 {
		s.pos += w
	} else {
		s.pos++
	}
}

// skipLine consumes one line break (CR LF counts as one).
func (s *scanner) skipLine() {
	if s.check('\r') && s.checkAt('\n', 1) {
		s.mark.index += 2
		s.mark.column = 0
		s.mark.line++
		s.pos += 2
	} else if s.isBreak() {
		s.mark.index++
		s.mark.column = 0
		s.mark.line++
		s.pos += width(s.src[s.pos])
	}
}

// read copies the current character to b and advances.
func (s *scanner) read(b *strings.Builder) {
	w := width(s.src[s.pos])
	if w == 0 || s.pos+w > len(s.src) {
		w = 1
	}
	b.Write(s.src[s.pos : s.pos+w])
	s.pos += w
	s.mark.index++
	s.mark.column++
}

// readLine copies a line break to b, normalizing CR, LF, CR LF and NEL to
// LF (LS and PS are kept), and advances.
func (s *scanner) readLine(b *strings.Builder) {
	switch {
	case s.check('\r') && s.checkAt('\n', 1):
		b.WriteByte('\n')
		s.pos += 2
		s.mark.index++
	case s.check('\r') || s.check('\n'):
		b.WriteByte('\n')
		s.pos++
	case s.check(0xC2) && s.checkAt(0x85, 1):
		b.WriteByte('\n')
		s.pos += 2
	case s.check(0xE2) && s.checkAt(0x80, 1) && (s.checkAt(0xA8, 2) || s.checkAt(0xA9, 2)):
		b.Write(s.src[s.pos : s.pos+3])
		s.pos += 3
	default:
		return
	}
	s.mark.index++
	s.mark.column = 0
	s.mark.line++
}

// scannerError records a scanner error at the current position.
func (s *scanner) scannerError(context string, contextMark mark, problem string) bool {
	s.err = &Error{File: s.name, Context: context, Problem: problem,
		Line: s.mark.line + 1, Col: s.mark.column + 1, src: s.text}
	_ = contextMark // libyaml records it; ansible shows only the problem mark
	return false
}

// peek returns the head token, fetching more as needed; nil on error.
func (s *scanner) peek() *token {
	if !s.tokenAvailable && !s.fetchMoreTokens() {
		return nil
	}
	return &s.tokens[s.head]
}

// skipToken removes the head token (after peek).
func (s *scanner) skipToken() {
	s.tokenAvailable = false
	s.tokensParsed++
	s.streamEndProduced = s.tokens[s.head].kind == tokStreamEnd
	s.head++
	if s.head == len(s.tokens) {
		s.tokens, s.head = s.tokens[:0], 0
	}
}

func (s *scanner) insertToken(pos int, t token) {
	if pos < 0 {
		s.tokens = append(s.tokens, t)
		return
	}
	i := s.head + pos
	s.tokens = append(s.tokens, token{})
	copy(s.tokens[i+1:], s.tokens[i:])
	s.tokens[i] = t
}

// fetchMoreTokens ensures the queue holds a token the parser may take: one
// that no pending simple key could still precede.
func (s *scanner) fetchMoreTokens() bool {
	for {
		need := false
		if s.head == len(s.tokens) {
			need = true
		} else {
			if !s.staleSimpleKeys() {
				return false
			}
			for i := range s.simpleKeys {
				if k := &s.simpleKeys[i]; k.possible && k.tokenNumber == s.tokensParsed {
					need = true
					break
				}
			}
		}
		if !need {
			break
		}
		if !s.fetchNextToken() {
			return false
		}
	}
	s.tokenAvailable = true
	return true
}

func (s *scanner) fetchNextToken() bool {
	if !s.streamStartProduced {
		return s.fetchStreamStart()
	}
	if !s.scanToNextToken() {
		return false
	}
	if !s.staleSimpleKeys() {
		return false
	}
	s.unrollIndent(s.mark.column)

	if s.isZ(0) {
		return s.fetchStreamEnd()
	}
	if s.mark.column == 0 && s.check('%') {
		return s.fetchDirective()
	}
	if s.mark.column == 0 && s.isDocumentIndicator() {
		if s.check('-') {
			return s.fetchDocumentIndicator(tokDocStart)
		}
		return s.fetchDocumentIndicator(tokDocEnd)
	}
	c := s.at(0)
	switch {
	case c == '[':
		return s.fetchFlowCollectionStart(tokFlowSeqStart)
	case c == '{':
		return s.fetchFlowCollectionStart(tokFlowMapStart)
	case c == ']':
		return s.fetchFlowCollectionEnd(tokFlowSeqEnd)
	case c == '}':
		return s.fetchFlowCollectionEnd(tokFlowMapEnd)
	case c == ',':
		return s.fetchFlowEntry()
	case c == '-' && s.isBlankZAt(1):
		return s.fetchBlockEntry()
	case c == '?' && (s.flowLevel > 0 || s.isBlankZAt(1)):
		return s.fetchKey()
	case c == ':' && (s.flowLevel > 0 || s.isBlankZAt(1)):
		return s.fetchValue()
	case c == '*':
		return s.fetchAnchor(tokAlias)
	case c == '&':
		return s.fetchAnchor(tokAnchor)
	case c == '!':
		return s.fetchTag()
	case c == '|' && s.flowLevel == 0:
		return s.fetchBlockScalar(true)
	case c == '>' && s.flowLevel == 0:
		return s.fetchBlockScalar(false)
	case c == '\'':
		return s.fetchFlowScalar(true)
	case c == '"':
		return s.fetchFlowScalar(false)
	}
	// A plain scalar may start with any non-blank character except the
	// indicators; '-', '?' and ':' also start one when followed by a
	// non-blank ('-' in any context, '?' and ':' in the block context).
	if !(s.isBlankZ() || strings.IndexByte("-?:,[]{}#&*!|>'\"%@`", c) >= 0) ||
		(c == '-' && !s.isBlankAt(1)) ||
		(s.flowLevel == 0 && (c == '?' || c == ':') && !s.isBlankZAt(1)) {
		return s.fetchPlainScalar()
	}
	return s.scannerError("while scanning for the next token", s.mark,
		"found character that cannot start any token")
}

// isDocumentIndicator reports '---' or '...' followed by a blank or break.
func (s *scanner) isDocumentIndicator() bool {
	c := s.at(0)
	return (c == '-' || c == '.') && s.checkAt(c, 1) && s.checkAt(c, 2) && s.isBlankZAt(3)
}

// staleSimpleKeys drops possible simple keys that can no longer be keys (a
// key must fit on one line and within 1024 characters); a required one is an
// error.
func (s *scanner) staleSimpleKeys() bool {
	for i := range s.simpleKeys {
		k := &s.simpleKeys[i]
		if k.possible && (k.mark.line < s.mark.line || k.mark.index+1024 < s.mark.index) {
			if k.required {
				return s.scannerError("while scanning a simple key", k.mark, "could not find expected ':'")
			}
			k.possible = false
		}
	}
	return true
}

// saveSimpleKey records that the next token may start a simple key. A key
// is required when it starts a block-context line at the indentation level.
func (s *scanner) saveSimpleKey() bool {
	required := s.flowLevel == 0 && s.indent == s.mark.column
	if s.simpleKeyAllowed {
		k := simpleKey{
			possible:    true,
			required:    required,
			tokenNumber: s.tokensParsed + len(s.tokens) - s.head,
			mark:        s.mark,
		}
		if !s.removeSimpleKey() {
			return false
		}
		s.simpleKeys[len(s.simpleKeys)-1] = k
	}
	return true
}

func (s *scanner) removeSimpleKey() bool {
	k := &s.simpleKeys[len(s.simpleKeys)-1]
	if k.possible && k.required {
		return s.scannerError("while scanning a simple key", k.mark, "could not find expected ':'")
	}
	k.possible = false
	return true
}

func (s *scanner) increaseFlowLevel() bool {
	s.simpleKeys = append(s.simpleKeys, simpleKey{})
	s.flowLevel++
	if s.flowLevel > maxDepth {
		return s.scannerError("while increasing flow level", s.mark, "exceeded max depth of 10000")
	}
	return true
}

func (s *scanner) decreaseFlowLevel() {
	if s.flowLevel > 0 {
		s.flowLevel--
		s.simpleKeys = s.simpleKeys[:len(s.simpleKeys)-1]
	}
}

// rollIndent opens a block collection at column when it is deeper than the
// current indentation, inserting its start token at queue position number
// (a token number; -1 appends).
func (s *scanner) rollIndent(column, number int, kind tokKind, m mark) bool {
	if s.flowLevel > 0 {
		return true
	}
	if s.indent < column {
		s.indents = append(s.indents, s.indent)
		s.indent = column
		if len(s.indents) > maxDepth {
			return s.scannerError("while increasing indent level", s.mark, "exceeded max depth of 10000")
		}
		t := token{kind: kind, start: m, end: m}
		if number > -1 {
			number -= s.tokensParsed
		}
		s.insertToken(number, t)
	}
	return true
}

// unrollIndent closes block collections deeper than column.
func (s *scanner) unrollIndent(column int) {
	if s.flowLevel > 0 {
		return
	}
	for s.indent > column {
		s.insertToken(-1, token{kind: tokBlockEnd, start: s.mark, end: s.mark})
		s.indent = s.indents[len(s.indents)-1]
		s.indents = s.indents[:len(s.indents)-1]
	}
}

func (s *scanner) fetchStreamStart() bool {
	s.indent = -1
	s.simpleKeys = append(s.simpleKeys, simpleKey{})
	s.simpleKeyAllowed = true
	s.streamStartProduced = true
	s.insertToken(-1, token{kind: tokStreamStart, start: s.mark, end: s.mark})
	return true
}

func (s *scanner) fetchStreamEnd() bool {
	// Force a new line.
	if s.mark.column != 0 {
		s.mark.column = 0
		s.mark.line++
	}
	s.unrollIndent(-1)
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	s.insertToken(-1, token{kind: tokStreamEnd, start: s.mark, end: s.mark})
	return true
}

func (s *scanner) fetchDirective() bool {
	s.unrollIndent(-1)
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	t, ok := s.scanDirective()
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

func (s *scanner) fetchDocumentIndicator(kind tokKind) bool {
	s.unrollIndent(-1)
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	start := s.mark
	s.skip()
	s.skip()
	s.skip()
	s.insertToken(-1, token{kind: kind, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchFlowCollectionStart(kind tokKind) bool {
	if !s.saveSimpleKey() {
		return false
	}
	if !s.increaseFlowLevel() {
		return false
	}
	s.simpleKeyAllowed = true
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: kind, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchFlowCollectionEnd(kind tokKind) bool {
	if !s.removeSimpleKey() {
		return false
	}
	s.decreaseFlowLevel()
	s.simpleKeyAllowed = false
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: kind, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchFlowEntry() bool {
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = true
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: tokFlowEntry, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchBlockEntry() bool {
	if s.flowLevel == 0 {
		if !s.simpleKeyAllowed {
			return s.scannerError("", s.mark, "block sequence entries are not allowed in this context")
		}
		if !s.rollIndent(s.mark.column, -1, tokBlockSeqStart, s.mark) {
			return false
		}
	}
	// In the flow context the parser reports the stray '-'.
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = true
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: tokBlockEntry, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchKey() bool {
	if s.flowLevel == 0 {
		if !s.simpleKeyAllowed {
			return s.scannerError("", s.mark, "mapping keys are not allowed in this context")
		}
		if !s.rollIndent(s.mark.column, -1, tokBlockMapStart, s.mark) {
			return false
		}
	}
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = s.flowLevel == 0
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: tokKey, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchValue() bool {
	k := &s.simpleKeys[len(s.simpleKeys)-1]
	if k.possible {
		// The saved token(s) become the mapping key.
		s.insertToken(k.tokenNumber-s.tokensParsed, token{kind: tokKey, start: k.mark, end: k.mark})
		if !s.rollIndent(k.mark.column, k.tokenNumber, tokBlockMapStart, k.mark) {
			return false
		}
		k.possible = false
		s.simpleKeyAllowed = false
	} else {
		// The ':' follows a complex key (or nothing).
		if s.flowLevel == 0 {
			if !s.simpleKeyAllowed {
				return s.scannerError("", s.mark, "mapping values are not allowed in this context")
			}
			if !s.rollIndent(s.mark.column, -1, tokBlockMapStart, s.mark) {
				return false
			}
		}
		s.simpleKeyAllowed = s.flowLevel == 0
	}
	start := s.mark
	s.skip()
	s.insertToken(-1, token{kind: tokValue, start: start, end: s.mark})
	return true
}

func (s *scanner) fetchAnchor(kind tokKind) bool {
	if !s.saveSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	t, ok := s.scanAnchor(kind)
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

func (s *scanner) fetchTag() bool {
	if !s.saveSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	t, ok := s.scanTag()
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

func (s *scanner) fetchBlockScalar(literal bool) bool {
	if !s.removeSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = true
	t, ok := s.scanBlockScalar(literal)
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

func (s *scanner) fetchFlowScalar(single bool) bool {
	if !s.saveSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	t, ok := s.scanFlowScalar(single)
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

func (s *scanner) fetchPlainScalar() bool {
	if !s.saveSimpleKey() {
		return false
	}
	s.simpleKeyAllowed = false
	t, ok := s.scanPlainScalar()
	if !ok {
		return false
	}
	s.insertToken(-1, t)
	return true
}

// scanToNextToken skips blanks, comments and line breaks. Tabs are skipped
// only in the flow context or where no simple key may start (so a tab never
// indents a block line or follows '-', '?' or ':').
func (s *scanner) scanToNextToken() bool {
	for {
		if s.mark.column == 0 && s.check(0xEF) && s.checkAt(0xBB, 1) && s.checkAt(0xBF, 2) {
			s.skip() // BOM
		}
		for s.check(' ') || ((s.flowLevel > 0 || !s.simpleKeyAllowed) && s.check('\t')) {
			s.skip()
		}
		if s.check('#') {
			for !s.isBreakZ() {
				s.skip()
			}
		}
		if !s.isBreak() {
			return true
		}
		s.skipLine()
		if s.flowLevel == 0 {
			s.simpleKeyAllowed = true
		}
	}
}

// scanDirective scans a %YAML or %TAG directive line.
func (s *scanner) scanDirective() (token, bool) {
	start := s.mark
	s.skip() // '%'
	name, ok := s.scanDirectiveName(start)
	if !ok {
		return token{}, false
	}
	var t token
	switch name {
	case "YAML":
		major, minor, ok := s.scanVersionDirectiveValue(start)
		if !ok {
			return token{}, false
		}
		t = token{kind: tokVersionDirective, start: start, end: s.mark, major: major, minor: minor}
	case "TAG":
		handle, prefix, ok := s.scanTagDirectiveValue(start)
		if !ok {
			return token{}, false
		}
		t = token{kind: tokTagDirective, start: start, end: s.mark, val: handle, suffix: prefix}
	default:
		return token{}, s.scannerError("while scanning a directive", start, "found unknown directive name")
	}
	// Eat the rest of the line including any comments.
	for s.isBlank() {
		s.skip()
	}
	if s.check('#') {
		for !s.isBreakZ() {
			s.skip()
		}
	}
	if !s.isBreakZ() {
		return token{}, s.scannerError("while scanning a directive", start, "did not find expected comment or line break")
	}
	if s.isBreak() {
		s.skipLine()
	}
	return t, true
}

func (s *scanner) scanDirectiveName(start mark) (string, bool) {
	var b strings.Builder
	for isAlpha(s.at(0)) && !s.isZ(0) {
		s.read(&b)
	}
	if b.Len() == 0 {
		return "", s.scannerError("while scanning a directive", start, "could not find expected directive name")
	}
	if !s.isBlankZ() {
		return "", s.scannerError("while scanning a directive", start, "found unexpected non-alphabetical character")
	}
	return b.String(), true
}

func (s *scanner) scanVersionDirectiveValue(start mark) (int, int, bool) {
	for s.isBlank() {
		s.skip()
	}
	major, ok := s.scanVersionDirectiveNumber(start)
	if !ok {
		return 0, 0, false
	}
	if !s.check('.') {
		return 0, 0, s.scannerError("while scanning a %YAML directive", start, "did not find expected digit or '.' character")
	}
	s.skip()
	minor, ok := s.scanVersionDirectiveNumber(start)
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

func (s *scanner) scanVersionDirectiveNumber(start mark) (int, bool) {
	value, length := 0, 0
	for isDigit(s.at(0)) {
		length++
		if length > 9 {
			return 0, s.scannerError("while scanning a %YAML directive", start, "found extremely long version number")
		}
		value = value*10 + int(s.at(0)-'0')
		s.skip()
	}
	if length == 0 {
		return 0, s.scannerError("while scanning a %YAML directive", start, "did not find expected version number")
	}
	return value, true
}

func (s *scanner) scanTagDirectiveValue(start mark) (string, string, bool) {
	for s.isBlank() {
		s.skip()
	}
	handle, ok := s.scanTagHandle(true, start)
	if !ok {
		return "", "", false
	}
	if !s.isBlank() {
		return "", "", s.scannerError("while scanning a %TAG directive", start, "did not find expected whitespace")
	}
	for s.isBlank() {
		s.skip()
	}
	prefix, ok := s.scanTagURI(true, true, "", start)
	if !ok {
		return "", "", false
	}
	if !s.isBlankZ() {
		return "", "", s.scannerError("while scanning a %TAG directive", start, "did not find expected whitespace or line break")
	}
	return handle, prefix, true
}

func (s *scanner) scanAnchor(kind tokKind) (token, bool) {
	start := s.mark
	s.skip() // '&' or '*'
	var b strings.Builder
	for isAlpha(s.at(0)) && !s.isZ(0) {
		s.read(&b)
	}
	end := s.mark
	// The name must be non-empty and end at a blank or an indicator.
	if b.Len() == 0 || !(s.isBlankZ() || strings.IndexByte("?:,]}%@`", s.at(0)) >= 0) {
		context := "while scanning an alias"
		if kind == tokAnchor {
			context = "while scanning an anchor"
		}
		return token{}, s.scannerError(context, start, "did not find expected alphabetic or numeric character")
	}
	return token{kind: kind, start: start, end: end, val: b.String()}, true
}

// scanTag scans '!<verbatim>', '!handle!suffix', '!suffix' or '!'. The
// token's val is the handle ("" for verbatim) and suffix the rest.
func (s *scanner) scanTag() (token, bool) {
	start := s.mark
	var handle, suffix string
	if s.checkAt('<', 1) {
		s.skip()
		s.skip()
		var ok bool
		if suffix, ok = s.scanTagURI(true, false, "", start); !ok {
			return token{}, false
		}
		if !s.check('>') {
			return token{}, s.scannerError("while scanning a tag", start, "did not find the expected '>'")
		}
		s.skip()
	} else {
		var ok bool
		if handle, ok = s.scanTagHandle(false, start); !ok {
			return token{}, false
		}
		if len(handle) > 1 && handle[0] == '!' && handle[len(handle)-1] == '!' {
			if suffix, ok = s.scanTagURI(false, false, "", start); !ok {
				return token{}, false
			}
		} else {
			// Not a handle after all: it is part of the suffix.
			if suffix, ok = s.scanTagURI(false, false, handle, start); !ok {
				return token{}, false
			}
			handle = "!"
			// The '!' non-specific tag: handle "" and suffix "!".
			if suffix == "" {
				handle, suffix = "", "!"
			}
		}
	}
	if !s.isBlankZ() {
		if s.flowLevel == 0 || !s.check(',') {
			return token{}, s.scannerError("while scanning a tag", start, "did not find expected whitespace or line break")
		}
	}
	return token{kind: tokTag, start: start, end: s.mark, val: handle, suffix: suffix}, true
}

func (s *scanner) tagError(directive bool, start mark, problem string) bool {
	context := "while parsing a tag"
	if directive {
		context = "while parsing a %TAG directive"
	}
	return s.scannerError(context, start, problem)
}

func (s *scanner) scanTagHandle(directive bool, start mark) (string, bool) {
	if !s.check('!') {
		context := "while scanning a tag"
		if directive {
			context = "while scanning a tag directive"
		}
		return "", s.scannerError(context, start, "did not find expected '!'")
	}
	var b strings.Builder
	s.read(&b)
	for isAlpha(s.at(0)) && !s.isZ(0) {
		s.read(&b)
	}
	if s.check('!') {
		s.read(&b)
	} else if directive && b.String() != "!" {
		// A %TAG handle must be '!', '!!' or '!name!'.
		return "", s.scannerError("while parsing a tag directive", start, "did not find expected '!'")
	}
	return b.String(), true
}

// scanTagURI scans a tag suffix or prefix. uriChar admits the flow
// indicators ',', '[' and ']' (verbatim tags and %TAG prefixes only); head
// is a mis-scanned handle to prepend (without its leading '!').
func (s *scanner) scanTagURI(uriChar, directive bool, head string, start mark) (string, bool) {
	var b strings.Builder
	length := len(head) // a lone '!' head counts: it is the '!' tag
	if length > 1 {
		b.WriteString(head[1:])
	}
	for !s.isZ(0) {
		c := s.at(0)
		if !(isAlpha(c) || strings.IndexByte(";/?:@&=+$.%!~*'()", c) >= 0 ||
			(uriChar && (c == ',' || c == '[' || c == ']'))) {
			break
		}
		if c == '%' {
			if !s.scanURIEscapes(directive, start, &b) {
				return "", false
			}
		} else {
			s.read(&b)
		}
		length++
	}
	if length == 0 {
		return "", s.tagError(directive, start, "did not find expected tag URI")
	}
	return b.String(), true
}

// scanURIEscapes decodes %XX escapes making up one UTF-8 character.
func (s *scanner) scanURIEscapes(directive bool, start mark, b *strings.Builder) bool {
	w := 1024
	for w > 0 {
		if !(s.check('%') && isHex(s.at(1)) && isHex(s.at(2)) && !s.isZ(2)) {
			return s.tagError(directive, start, "did not find URI escaped octet")
		}
		octet := byte(asHex(s.at(1))<<4 + asHex(s.at(2)))
		if w == 1024 {
			w = width(octet)
			if w == 0 {
				return s.tagError(directive, start, "found an incorrect leading UTF-8 octet")
			}
		} else if octet&0xC0 != 0x80 {
			return s.tagError(directive, start, "found an incorrect trailing UTF-8 octet")
		}
		b.WriteByte(octet)
		s.skip()
		s.skip()
		s.skip()
		w--
	}
	return true
}

// scanBlockScalar scans a literal (|) or folded (>) scalar.
func (s *scanner) scanBlockScalar(literal bool) (token, bool) {
	start := s.mark
	s.skip() // '|' or '>'

	chomping, increment := 0, 0
	if s.check('+') || s.check('-') {
		chomping = 1
		if s.check('-') {
			chomping = -1
		}
		s.skip()
		if isDigit(s.at(0)) && !s.isZ(0) {
			if s.check('0') {
				return token{}, s.scannerError("while scanning a block scalar", start, "found an indentation indicator equal to 0")
			}
			increment = int(s.at(0) - '0')
			s.skip()
		}
	} else if isDigit(s.at(0)) && !s.isZ(0) {
		if s.check('0') {
			return token{}, s.scannerError("while scanning a block scalar", start, "found an indentation indicator equal to 0")
		}
		increment = int(s.at(0) - '0')
		s.skip()
		if s.check('+') || s.check('-') {
			chomping = 1
			if s.check('-') {
				chomping = -1
			}
			s.skip()
		}
	}

	// Eat whitespaces and comments to the end of the line.
	for s.isBlank() {
		s.skip()
	}
	if s.check('#') {
		for !s.isBreakZ() {
			s.skip()
		}
	}
	if !s.isBreakZ() {
		return token{}, s.scannerError("while scanning a block scalar", start, "did not find expected comment or line break")
	}
	if s.isBreak() {
		s.skipLine()
	}
	end := s.mark

	indent := 0
	if increment > 0 {
		if s.indent >= 0 {
			indent = s.indent + increment
		} else {
			indent = increment
		}
	}

	var out, leadingBreak, trailingBreaks strings.Builder
	if !s.scanBlockScalarBreaks(&indent, &trailingBreaks, start, &end) {
		return token{}, false
	}

	leadingBlank, trailingBlank := false, false
	for s.mark.column == indent && !s.isZ(0) {
		// At the beginning of a non-empty line.
		trailingBlank = s.isBlank()
		if !literal && !leadingBlank && !trailingBlank && strings.HasPrefix(leadingBreak.String(), "\n") {
			// Fold: join the lines by a space unless blank lines follow.
			if trailingBreaks.Len() == 0 {
				out.WriteByte(' ')
			}
		} else {
			out.WriteString(leadingBreak.String())
		}
		leadingBreak.Reset()
		out.WriteString(trailingBreaks.String())
		trailingBreaks.Reset()

		leadingBlank = s.isBlank()
		for !s.isBreakZ() {
			s.read(&out)
		}
		s.readLine(&leadingBreak)
		if !s.scanBlockScalarBreaks(&indent, &trailingBreaks, start, &end) {
			return token{}, false
		}
	}

	// Chomp the tail.
	if chomping != -1 {
		out.WriteString(leadingBreak.String())
	}
	if chomping == 1 {
		out.WriteString(trailingBreaks.String())
	}
	style := Literal
	if !literal {
		style = Folded
	}
	return token{kind: tokScalar, start: start, end: end, val: out.String(), style: style}, true
}

// scanBlockScalarBreaks eats indentation spaces and blank lines, and fixes
// the content indentation (from the most indented leading blank line) when
// it is not yet known.
func (s *scanner) scanBlockScalarBreaks(indent *int, breaks *strings.Builder, start mark, end *mark) bool {
	*end = s.mark
	maxIndent := 0
	for {
		for (*indent == 0 || s.mark.column < *indent) && s.check(' ') {
			s.skip()
		}
		if s.mark.column > maxIndent {
			maxIndent = s.mark.column
		}
		if (*indent == 0 || s.mark.column < *indent) && s.check('\t') {
			return s.scannerError("while scanning a block scalar", start, "found a tab character where an indentation space is expected")
		}
		if !s.isBreak() {
			break
		}
		s.readLine(breaks)
		*end = s.mark
	}
	if *indent == 0 {
		*indent = maxIndent
		if *indent < s.indent+1 {
			*indent = s.indent + 1
		}
		if *indent < 1 {
			*indent = 1
		}
	}
	return true
}

// scanFlowScalar scans a single- or double-quoted scalar.
func (s *scanner) scanFlowScalar(single bool) (token, bool) {
	start := s.mark
	s.skip() // the left quote
	var out, leadingBreak, trailingBreaks, whitespaces strings.Builder
	for {
		if s.mark.column == 0 && s.isDocumentIndicator() {
			return token{}, s.scannerError("while scanning a quoted scalar", start, "found unexpected document indicator")
		}
		if s.isZ(0) {
			return token{}, s.scannerError("while scanning a quoted scalar", start, "found unexpected end of stream")
		}

		// Consume non-blank characters.
		leadingBlanks := false
		for !s.isBlankZ() {
			c := s.at(0)
			switch {
			case single && c == '\'' && s.checkAt('\'', 1):
				out.WriteByte('\'')
				s.skip()
				s.skip()
				continue
			case single && c == '\'', !single && c == '"':
			case !single && c == '\\' && s.isBreakAt(1):
				// An escaped line break.
				s.skip()
				s.skipLine()
				leadingBlanks = true
			case !single && c == '\\':
				codeLength := 0
				switch s.at(1) {
				case '0':
					out.WriteByte(0)
				case 'a':
					out.WriteByte('\a')
				case 'b':
					out.WriteByte('\b')
				case 't', '\t':
					out.WriteByte('\t')
				case 'n':
					out.WriteByte('\n')
				case 'v':
					out.WriteByte('\v')
				case 'f':
					out.WriteByte('\f')
				case 'r':
					out.WriteByte('\r')
				case 'e':
					out.WriteByte('\x1b')
				case ' ':
					out.WriteByte(' ')
				case '"':
					out.WriteByte('"')
				case '/':
					out.WriteByte('/')
				case '\\':
					out.WriteByte('\\')
				case 'N':
					out.WriteString("\u0085")
				case '_':
					out.WriteString(" ")
				case 'L':
					out.WriteString(" ")
				case 'P':
					out.WriteString(" ")
				case 'x':
					codeLength = 2
				case 'u':
					codeLength = 4
				case 'U':
					codeLength = 8
				default:
					return token{}, s.scannerError("while parsing a quoted scalar", start, "found unknown escape character")
				}
				s.skip()
				s.skip()
				if codeLength > 0 {
					value := 0
					for k := 0; k < codeLength; k++ {
						if !isHex(s.at(k)) || s.isZ(k) {
							return token{}, s.scannerError("while parsing a quoted scalar", start, "did not find expected hexdecimal number")
						}
						value = value<<4 + asHex(s.at(k))
					}
					if (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
						return token{}, s.scannerError("while parsing a quoted scalar", start, "found invalid Unicode character escape code")
					}
					out.WriteRune(rune(value))
					for k := 0; k < codeLength; k++ {
						s.skip()
					}
				}
				continue
			default:
				s.read(&out)
				continue
			}
			break
		}

		// At the closing quote?
		if (single && s.check('\'')) || (!single && s.check('"')) {
			break
		}

		// Consume blank characters.
		for s.isBlank() || s.isBreak() {
			if s.isBlank() {
				if !leadingBlanks {
					s.read(&whitespaces)
				} else {
					s.skip()
				}
			} else if !leadingBlanks {
				whitespaces.Reset()
				s.readLine(&leadingBreak)
				leadingBlanks = true
			} else {
				s.readLine(&trailingBreaks)
			}
		}

		// Join the whitespaces or fold line breaks.
		if leadingBlanks {
			if strings.HasPrefix(leadingBreak.String(), "\n") {
				if trailingBreaks.Len() == 0 {
					out.WriteByte(' ')
				} else {
					out.WriteString(trailingBreaks.String())
				}
			} else {
				out.WriteString(leadingBreak.String())
				out.WriteString(trailingBreaks.String())
			}
			leadingBreak.Reset()
			trailingBreaks.Reset()
		} else {
			out.WriteString(whitespaces.String())
			whitespaces.Reset()
		}
	}
	s.skip() // the right quote
	style := DoubleQuoted
	if single {
		style = SingleQuoted
	}
	return token{kind: tokScalar, start: start, end: s.mark, val: out.String(), style: style}, true
}

// scanPlainScalar scans a plain scalar, possibly spanning lines.
func (s *scanner) scanPlainScalar() (token, bool) {
	var out, leadingBreak, trailingBreaks, whitespaces strings.Builder
	leadingBlanks := false
	indent := s.indent + 1
	start := s.mark
	end := s.mark
	for {
		if s.mark.column == 0 && s.isDocumentIndicator() {
			break
		}
		if s.check('#') {
			break
		}
		for !s.isBlankZ() {
			// "x:" followed by a flow indicator in the flow context.
			if s.flowLevel > 0 && s.check(':') && strings.IndexByte(",?[]{}", s.at(1)) >= 0 && !s.isZ(1) {
				return token{}, s.scannerError("while scanning a plain scalar", start, "found unexpected ':'")
			}
			// Indicators that may end a plain scalar.
			if (s.check(':') && s.isBlankZAt(1)) ||
				(s.flowLevel > 0 && strings.IndexByte(",[]{}", s.at(0)) >= 0) {
				break
			}
			// Join pending whitespace or folded line breaks.
			if leadingBlanks || whitespaces.Len() > 0 {
				if leadingBlanks {
					if strings.HasPrefix(leadingBreak.String(), "\n") {
						if trailingBreaks.Len() == 0 {
							out.WriteByte(' ')
						} else {
							out.WriteString(trailingBreaks.String())
						}
					} else {
						out.WriteString(leadingBreak.String())
						out.WriteString(trailingBreaks.String())
					}
					leadingBreak.Reset()
					trailingBreaks.Reset()
					leadingBlanks = false
				} else {
					out.WriteString(whitespaces.String())
					whitespaces.Reset()
				}
			}
			s.read(&out)
			end = s.mark
		}

		// Is it the end?
		if !(s.isBlank() || s.isBreak()) {
			break
		}

		// Consume blank characters.
		for s.isBlank() || s.isBreak() {
			if s.isBlank() {
				// A tab may not indent a continuation line.
				if leadingBlanks && s.mark.column < indent && s.check('\t') {
					return token{}, s.scannerError("while scanning a plain scalar", start, "found a tab character that violates indentation")
				}
				if !leadingBlanks {
					s.read(&whitespaces)
				} else {
					s.skip()
				}
			} else if !leadingBlanks {
				whitespaces.Reset()
				s.readLine(&leadingBreak)
				leadingBlanks = true
			} else {
				s.readLine(&trailingBreaks)
			}
		}

		// Check the indentation level.
		if s.flowLevel == 0 && s.mark.column < indent {
			break
		}
	}
	// A simple key may follow a plain scalar that ended at a line break.
	if leadingBlanks {
		s.simpleKeyAllowed = true
	}
	return token{kind: tokScalar, start: start, end: end, val: out.String(), style: Plain}, true
}
