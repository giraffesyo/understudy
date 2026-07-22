package yaml

import "unicode/utf8"

// scanner tokenizes YAML following PyYAML's scanner algorithm: it owns all
// indentation logic (an indent stack emitting synthetic block-start/end
// tokens) and simple-key detection (retroactively inserting tokKey when a
// ':' is found), so the parser is a plain grammar walk.
//
// Positions are 1-based. The indent stack stores the 1-based column at which
// each open block collection started; an empty stack means indent 0.
type scanner struct {
	src            []byte
	name           string
	off            int
	line, col      int
	tokens         []token
	indents        []int
	flowLevel      int
	allowSimpleKey bool
	sk             simpleKey
}

// simpleKey tracks a scalar (or anchor/tag prefix) that may retroactively
// become a mapping key when a ':' follows on the same line.
type simpleKey struct {
	possible  bool
	tokenIdx  int // index in s.tokens where the key's first token sits
	line, col int
}

func newScanner(src []byte, name string) *scanner {
	if len(src) >= 3 && src[0] == 0xEF && src[1] == 0xBB && src[2] == 0xBF {
		src = src[3:] // strip UTF-8 BOM
	}
	return &scanner{src: src, name: name, line: 1, col: 1, allowSimpleKey: true}
}

func (s *scanner) indent() int {
	if len(s.indents) == 0 {
		return 0
	}
	return s.indents[len(s.indents)-1]
}

// ch returns the byte at offset off+i, or 0 at EOF. ASCII comparisons against
// it are safe: UTF-8 continuation/lead bytes never match ASCII values.
func (s *scanner) ch(i int) byte {
	if s.off+i >= len(s.src) {
		return 0
	}
	return s.src[s.off+i]
}

func (s *scanner) eof() bool { return s.off >= len(s.src) }

func isBreak(c byte) bool { return c == '\n' || c == '\r' }
func isBlank(c byte) bool { return c == ' ' || c == '\t' }
func isBlankOrBreakOrEOF(c byte) bool {
	return c == 0 || c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
func isFlowIndicator(c byte) bool {
	return c == ',' || c == '[' || c == ']' || c == '{' || c == '}'
}

// advance consumes one rune, or one line break ('\n', '\r', or '\r\n' as a
// single break), updating line/col.
func (s *scanner) advance() {
	c := s.src[s.off]
	switch {
	case c == '\r':
		s.off++
		if !s.eof() && s.src[s.off] == '\n' {
			s.off++
		}
		s.line++
		s.col = 1
	case c == '\n':
		s.off++
		s.line++
		s.col = 1
	case c < utf8.RuneSelf:
		s.off++
		s.col++
	default:
		_, size := utf8.DecodeRune(s.src[s.off:])
		s.off += size
		s.col++
	}
}

// advanceN consumes n runes known not to contain line breaks.
func (s *scanner) advanceN(n int) {
	for i := 0; i < n; i++ {
		s.advance()
	}
}

// scanLineBreak consumes one line break and returns its normalized form.
func (s *scanner) scanLineBreak() string {
	if isBreak(s.ch(0)) {
		s.advance()
		return "\n"
	}
	return ""
}

func (s *scanner) emit(t token) { s.tokens = append(s.tokens, t) }

func (s *scanner) insertToken(idx int, t token) {
	s.tokens = append(s.tokens, token{})
	copy(s.tokens[idx+1:], s.tokens[idx:])
	s.tokens[idx] = t
}

// scan tokenizes the whole stream.
func (s *scanner) scan() ([]token, error) {
	for {
		if err := s.scanNext(); err != nil {
			return nil, err
		}
		if len(s.tokens) > 0 && s.tokens[len(s.tokens)-1].kind == tokStreamEnd {
			return s.tokens, nil
		}
	}
}

// scanToNextToken skips spaces, comments, and line breaks. Like PyYAML, tabs
// are NOT skipped between tokens: a tab at a token position is an error.
func (s *scanner) scanToNextToken() {
	for {
		c := s.ch(0)
		switch {
		case c == ' ':
			s.advance()
		case c == '#':
			for !s.eof() && !isBreak(s.ch(0)) {
				s.advance()
			}
		case isBreak(c):
			s.advance()
			if s.flowLevel == 0 {
				s.allowSimpleKey = true
			}
		default:
			return
		}
	}
}

func (s *scanner) scanNext() error {
	s.scanToNextToken()

	// A pending simple key goes stale once we leave its line (block context:
	// a key and its ':' must share a line).
	if s.sk.possible && s.flowLevel == 0 && s.sk.line != s.line {
		s.sk.possible = false
	}

	if s.eof() {
		s.unrollIndent(0)
		s.sk.possible = false
		s.allowSimpleKey = false
		s.emit(token{kind: tokStreamEnd, line: s.line, col: s.col})
		return nil
	}

	s.unrollIndent(s.col)

	c := s.ch(0)

	if s.col == 1 {
		if c == '%' {
			return s.scanDirective()
		}
		if (c == '-' && s.ch(1) == '-' && s.ch(2) == '-' && isBlankOrBreakOrEOF(s.ch(3))) ||
			(c == '.' && s.ch(1) == '.' && s.ch(2) == '.' && isBlankOrBreakOrEOF(s.ch(3))) {
			kind := tokDocStart
			if c == '.' {
				kind = tokDocEnd
			}
			s.unrollIndent(0)
			s.sk.possible = false
			s.allowSimpleKey = false
			tok := token{kind: kind, line: s.line, col: s.col}
			s.advanceN(3)
			s.emit(tok)
			return nil
		}
	}

	switch {
	case c == '[':
		return s.fetchFlowStart(tokFlowSeqStart)
	case c == '{':
		return s.fetchFlowStart(tokFlowMapStart)
	case c == ']':
		return s.fetchFlowEnd(tokFlowSeqEnd)
	case c == '}':
		return s.fetchFlowEnd(tokFlowMapEnd)
	case c == ',':
		return s.fetchFlowEntry()
	case c == '-' && isBlankOrBreakOrEOF(s.ch(1)):
		return s.fetchBlockEntry()
	case c == '?' && (s.flowLevel > 0 || isBlankOrBreakOrEOF(s.ch(1))):
		return s.errf(s.line, s.col, "complex mapping keys ('? ') are not supported")
	case c == ':' && (s.flowLevel > 0 || isBlankOrBreakOrEOF(s.ch(1))):
		return s.fetchValue()
	case c == '&':
		return s.fetchAnchorOrAlias(tokAnchor)
	case c == '*':
		return s.fetchAnchorOrAlias(tokAlias)
	case c == '!':
		return s.fetchTag()
	case (c == '|' || c == '>') && s.flowLevel == 0:
		return s.fetchBlockScalar(c == '>')
	case c == '\'':
		return s.fetchFlowStyleScalar(false)
	case c == '"':
		return s.fetchFlowStyleScalar(true)
	case c == '\t':
		return s.errf(s.line, s.col, "found a tab character where a token is expected (tabs cannot be used for indentation)")
	case canStartPlain(c):
		return s.fetchPlain()
	}
	return s.errf(s.line, s.col, "found character %q that cannot start any token", string(rune(c)))
}

// canStartPlain reports whether c can begin a plain scalar. '-', '?', ':'
// reach here only when not followed by a blank (checked in the dispatcher).
func canStartPlain(c byte) bool {
	switch c {
	case 0, ' ', '\t', '\n', '\r':
		return false
	case '-', '?', ':':
		return true
	case ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
		return false
	}
	return true
}

// scanDirective handles %YAML (accepted and ignored) and rejects %TAG.
func (s *scanner) scanDirective() error {
	line, col := s.line, s.col
	s.advance() // '%'
	start := s.off
	for !s.eof() && !isBlankOrBreakOrEOF(s.ch(0)) {
		s.advance()
	}
	name := string(s.src[start:s.off])
	if name == "TAG" {
		return s.errf(line, col, "%%TAG directives are not supported")
	}
	// %YAML and unknown directives: skip to end of line.
	for !s.eof() && !isBreak(s.ch(0)) {
		s.advance()
	}
	s.unrollIndent(0)
	s.sk.possible = false
	s.allowSimpleKey = false
	return nil
}

// rollIndent opens a block collection at column col if it is deeper than the
// current indent, inserting the start token at index idx in the queue.
func (s *scanner) rollIndent(col int, kind tokKind, idx, line, tcol int) {
	if s.flowLevel > 0 || s.indent() >= col {
		return
	}
	s.indents = append(s.indents, col)
	s.insertToken(idx, token{kind: kind, line: line, col: tcol})
}

// unrollIndent closes block collections deeper than col.
func (s *scanner) unrollIndent(col int) {
	if s.flowLevel > 0 {
		return
	}
	for len(s.indents) > 0 && s.indents[len(s.indents)-1] > col {
		s.indents = s.indents[:len(s.indents)-1]
		s.emit(token{kind: tokBlockEnd, line: s.line, col: s.col})
	}
}

// saveSimpleKey records that the token about to be scanned could be a key.
func (s *scanner) saveSimpleKey() {
	if s.allowSimpleKey {
		s.sk = simpleKey{possible: true, tokenIdx: len(s.tokens), line: s.line, col: s.col}
	}
}

func (s *scanner) fetchFlowStart(kind tokKind) error {
	s.saveSimpleKey()
	s.flowLevel++
	s.allowSimpleKey = true
	s.emit(token{kind: kind, line: s.line, col: s.col})
	s.advance()
	return nil
}

func (s *scanner) fetchFlowEnd(kind tokKind) error {
	s.sk.possible = false
	if s.flowLevel > 0 {
		s.flowLevel--
	}
	s.allowSimpleKey = false
	s.emit(token{kind: kind, line: s.line, col: s.col})
	s.advance()
	return nil
}

func (s *scanner) fetchFlowEntry() error {
	s.sk.possible = false
	s.allowSimpleKey = true
	s.emit(token{kind: tokFlowEntry, line: s.line, col: s.col})
	s.advance()
	return nil
}

func (s *scanner) fetchBlockEntry() error {
	if s.flowLevel > 0 {
		return s.errf(s.line, s.col, "block sequence entries are not allowed inside flow collections")
	}
	if !s.allowSimpleKey {
		return s.errf(s.line, s.col, "block sequence entries are not allowed here")
	}
	s.rollIndent(s.col, tokBlockSeqStart, len(s.tokens), s.line, s.col)
	s.sk.possible = false
	s.allowSimpleKey = true
	s.emit(token{kind: tokBlockEntry, line: s.line, col: s.col})
	s.advance()
	return nil
}

func (s *scanner) fetchValue() error {
	if s.sk.possible {
		// The saved token(s) become the mapping key.
		s.insertToken(s.sk.tokenIdx, token{kind: tokKey, line: s.sk.line, col: s.sk.col})
		s.rollIndent(s.sk.col, tokBlockMapStart, s.sk.tokenIdx, s.sk.line, s.sk.col)
		s.sk.possible = false
		s.allowSimpleKey = false
	} else {
		if s.flowLevel == 0 {
			if !s.allowSimpleKey {
				return s.errf(s.line, s.col, "mapping values are not allowed here (is a previous value missing quotes or spanning lines?)")
			}
			s.rollIndent(s.col, tokBlockMapStart, len(s.tokens), s.line, s.col)
		}
		s.allowSimpleKey = s.flowLevel == 0
	}
	s.emit(token{kind: tokValue, line: s.line, col: s.col})
	s.advance()
	return nil
}

func (s *scanner) fetchAnchorOrAlias(kind tokKind) error {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	line, col := s.line, s.col
	s.advance() // '&' or '*'
	start := s.off
	for {
		c := s.ch(0)
		if c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			s.advance()
			continue
		}
		break
	}
	name := string(s.src[start:s.off])
	if name == "" {
		return s.errf(line, col, "expected an anchor name after %q", string(rune(s.src[s.off-1])))
	}
	if c := s.ch(0); !isBlankOrBreakOrEOF(c) && !isFlowIndicator(c) && c != ':' {
		return s.errf(s.line, s.col, "unexpected character %q in anchor name", string(rune(c)))
	}
	s.emit(token{kind: kind, val: name, line: line, col: col})
	return nil
}

func (s *scanner) fetchTag() error {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	line, col := s.line, s.col
	s.advance() // '!'
	if s.ch(0) == '<' {
		return s.errf(line, col, "verbatim tags (!<...>) are not supported")
	}
	prefix := "!"
	if s.ch(0) == '!' {
		prefix = "!!"
		s.advance()
	}
	start := s.off
	for {
		c := s.ch(0)
		if c == '-' || c == '_' || c == '.' || c == '/' ||
			c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			s.advance()
			continue
		}
		break
	}
	suffix := string(s.src[start:s.off])
	if !isBlankOrBreakOrEOF(s.ch(0)) && !isFlowIndicator(s.ch(0)) {
		return s.errf(s.line, s.col, "unexpected character %q in tag", string(rune(s.ch(0))))
	}
	if prefix == "!!" && suffix == "" {
		return s.errf(line, col, "expected a tag name after '!!'")
	}
	s.emit(token{kind: tokTag, val: prefix + suffix, line: line, col: col})
	return nil
}
