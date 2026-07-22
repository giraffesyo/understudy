package yaml

import (
	"strings"
	"unicode/utf8"
)

// This file ports PyYAML's scalar scanning: scan_plain, scan_flow_scalar,
// and scan_block_scalar, including their exact line-folding behavior
// (single break -> space; break followed by N more breaks -> N newlines).

func (s *scanner) fetchPlain() error {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	tok, err := s.scanPlain()
	if err != nil {
		return err
	}
	s.emit(tok)
	return nil
}

func (s *scanner) fetchFlowStyleScalar(double bool) error {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	tok, err := s.scanFlowScalar(double)
	if err != nil {
		return err
	}
	s.emit(tok)
	return nil
}

func (s *scanner) fetchBlockScalar(folded bool) error {
	s.allowSimpleKey = true
	s.sk.possible = false
	tok, err := s.scanBlockScalar(folded)
	if err != nil {
		return err
	}
	s.emit(tok)
	return nil
}

// scanPlain scans a plain (unquoted) scalar, possibly spanning lines.
func (s *scanner) scanPlain() (token, error) {
	startLine, startCol := s.line, s.col
	var chunks strings.Builder
	var spaces string        // pending fold whitespace between content runs
	minCol := s.indent() + 1 // continuation lines must start at col > indent

	for {
		if s.ch(0) == '#' {
			break
		}
		// Consume one run of non-terminating characters.
		length := 0
		for {
			c := s.ch(length)
			if isBlankOrBreakOrEOF(c) {
				break
			}
			if c == ':' {
				n := s.ch(length + 1)
				if isBlankOrBreakOrEOF(n) || (s.flowLevel > 0 && isFlowIndicator(n)) {
					break
				}
			}
			if s.flowLevel > 0 && (isFlowIndicator(c) || c == '?') {
				break
			}
			length++
		}
		if length == 0 {
			break
		}
		s.allowSimpleKey = false
		chunks.WriteString(spaces)
		chunks.Write(s.src[s.off : s.off+length])
		s.advanceBytes(length)

		var ended bool
		spaces, ended = s.scanPlainSpaces()
		if ended || spaces == "" || s.ch(0) == '#' ||
			(s.flowLevel == 0 && s.col < minCol) {
			break
		}
	}
	return token{kind: tokScalar, val: chunks.String(), style: Plain, line: startLine, col: startCol}, nil
}

// advanceBytes consumes exactly n bytes known to contain no line breaks,
// counting columns in runes.
func (s *scanner) advanceBytes(n int) {
	end := s.off + n
	for s.off < end {
		if s.src[s.off] < utf8.RuneSelf {
			s.off++
		} else {
			_, size := utf8.DecodeRune(s.src[s.off:])
			s.off += size
		}
		s.col++
	}
}

// scanPlainSpaces consumes blanks/breaks after a plain-scalar run and returns
// the folded whitespace to insert before the next run. ended=true means the
// scalar is terminated (a document marker starts the next line).
func (s *scanner) scanPlainSpaces() (string, bool) {
	length := 0
	for s.ch(length) == ' ' {
		length++
	}
	whitespace := string(s.src[s.off : s.off+length])
	s.advanceBytes(length)

	if !isBreak(s.ch(0)) {
		return whitespace, false
	}
	s.scanLineBreak()
	s.allowSimpleKey = true
	if s.atDocumentMarker() {
		return "", true
	}
	var breaks strings.Builder
	for s.ch(0) == ' ' || isBreak(s.ch(0)) {
		if s.ch(0) == ' ' {
			s.advance()
		} else {
			s.scanLineBreak()
			breaks.WriteString("\n")
			if s.atDocumentMarker() {
				return "", true
			}
		}
	}
	if breaks.Len() == 0 {
		return " ", false // single break folds to one space
	}
	return breaks.String(), false
}

func (s *scanner) atDocumentMarker() bool {
	if s.col != 1 {
		return false
	}
	c := s.ch(0)
	return (c == '-' && s.ch(1) == '-' && s.ch(2) == '-' && isBlankOrBreakOrEOF(s.ch(3))) ||
		(c == '.' && s.ch(1) == '.' && s.ch(2) == '.' && isBlankOrBreakOrEOF(s.ch(3)))
}

var doubleEscapes = map[byte]string{
	'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", '\t': "\t", 'n': "\n",
	'v': "\v", 'f': "\f", 'r': "\r", 'e': "\x1b", ' ': " ", '"': "\"",
	'\\': "\\", '/': "/", 'N': "\u0085", '_': "\u00a0", 'L': "\u2028", 'P': "\u2029",
}

var escapeLengths = map[byte]int{'x': 2, 'u': 4, 'U': 8}

// scanFlowScalar scans a single- or double-quoted scalar.
func (s *scanner) scanFlowScalar(double bool) (token, error) {
	startLine, startCol := s.line, s.col
	quote := s.ch(0)
	s.advance()
	var chunks strings.Builder
	for {
		// Non-space chunk.
		for {
			c := s.ch(0)
			if c == 0 {
				return token{}, s.errf(startLine, startCol, "unexpected end of stream within a quoted scalar")
			}
			if isBlank(c) || isBreak(c) {
				break
			}
			switch {
			case !double && c == '\'' && s.ch(1) == '\'':
				chunks.WriteByte('\'')
				s.advanceN(2)
			case c == quote:
				s.advance()
				return token{kind: tokScalar, val: chunks.String(),
					style: quotedStyle(double), line: startLine, col: startCol}, nil
			case double && c == '\\':
				s.advance()
				e := s.ch(0)
				if rep, ok := doubleEscapes[e]; ok {
					chunks.WriteString(rep)
					s.advance()
				} else if n, ok := escapeLengths[e]; ok {
					s.advance()
					var code rune
					for i := 0; i < n; i++ {
						h := s.ch(0)
						var d rune
						switch {
						case h >= '0' && h <= '9':
							d = rune(h - '0')
						case h >= 'a' && h <= 'f':
							d = rune(h-'a') + 10
						case h >= 'A' && h <= 'F':
							d = rune(h-'A') + 10
						default:
							return token{}, s.errf(s.line, s.col, "expected %d hexadecimal digits in escape sequence", n)
						}
						code = code*16 + d
						s.advance()
					}
					chunks.WriteRune(code)
				} else if isBreak(e) {
					// Escaped line break: join lines with no space.
					s.scanLineBreak()
					chunks.WriteString(s.scanFlowScalarBreaks())
				} else {
					return token{}, s.errf(s.line, s.col, "unknown escape character %q", string(rune(e)))
				}
			default:
				start := s.off
				for {
					c := s.ch(0)
					if c == 0 || isBlank(c) || isBreak(c) || c == quote ||
						(double && c == '\\') || (!double && c == '\'') {
						break
					}
					s.advance()
				}
				chunks.Write(s.src[start:s.off])
			}
		}
		// Whitespace / folding chunk.
		length := 0
		for isBlank(s.ch(length)) {
			length++
		}
		whitespace := string(s.src[s.off : s.off+length])
		s.advanceBytes(length)
		if isBreak(s.ch(0)) {
			s.scanLineBreak()
			breaks := s.scanFlowScalarBreaks()
			if breaks == "" {
				chunks.WriteString(" ")
			} else {
				chunks.WriteString(breaks)
			}
		} else {
			chunks.WriteString(whitespace)
		}
	}
}

func quotedStyle(double bool) Style {
	if double {
		return DoubleQuoted
	}
	return SingleQuoted
}

// scanFlowScalarBreaks consumes blank lines inside a quoted scalar and
// returns the newlines they fold to.
func (s *scanner) scanFlowScalarBreaks() string {
	var breaks strings.Builder
	for {
		for isBlank(s.ch(0)) {
			s.advance()
		}
		if isBreak(s.ch(0)) {
			s.scanLineBreak()
			breaks.WriteString("\n")
		} else {
			return breaks.String()
		}
	}
}

// scanBlockScalar scans a literal (|) or folded (>) block scalar.
// Chomping: 0 = clip (default), -1 = strip, +1 = keep.
func (s *scanner) scanBlockScalar(folded bool) (token, error) {
	startLine, startCol := s.line, s.col
	s.advance() // '|' or '>'

	chomping := 0
	increment := -1
	for {
		c := s.ch(0)
		switch {
		case c == '+' && chomping == 0:
			chomping = 1
			s.advance()
			continue
		case c == '-' && chomping == 0:
			chomping = -1
			s.advance()
			continue
		case c >= '1' && c <= '9' && increment < 0:
			increment = int(c - '0')
			s.advance()
			continue
		}
		break
	}
	// Rest of the header line: optional spaces + comment, then a break.
	for isBlank(s.ch(0)) {
		s.advance()
	}
	if s.ch(0) == '#' {
		for !s.eof() && !isBreak(s.ch(0)) {
			s.advance()
		}
	}
	if !s.eof() && !isBreak(s.ch(0)) {
		return token{}, s.errf(s.line, s.col, "unexpected character %q in block scalar header", string(rune(s.ch(0))))
	}
	s.scanLineBreak()

	// Content indent, in PyYAML's 0-based terms: minIndent is one deeper than
	// the enclosing block level; our 1-based indent() equals that directly.
	minIndent := s.indent()
	if minIndent < 1 {
		minIndent = 1
	}
	var chunks strings.Builder
	var breaks string
	var indent int
	if increment >= 0 {
		indent = minIndent + increment - 1
		breaks = s.scanBlockScalarBreaks(indent)
	} else {
		var maxIndent int
		breaks, maxIndent = s.scanBlockScalarIndentation()
		indent = minIndent
		if maxIndent > indent {
			indent = maxIndent
		}
	}

	lineBreak := ""
	for s.col-1 == indent && !s.eof() {
		chunks.WriteString(breaks)
		leadingNonSpace := !isBlank(s.ch(0))
		start := s.off
		for !s.eof() && !isBreak(s.ch(0)) {
			s.advance()
		}
		chunks.Write(s.src[start:s.off])
		lineBreak = s.scanLineBreak()
		breaks = s.scanBlockScalarBreaks(indent)
		if s.col-1 == indent && !s.eof() {
			// Folded: a single break between two non-more-indented content
			// lines becomes a space; otherwise breaks are kept literally.
			if folded && lineBreak == "\n" && leadingNonSpace && !isBlank(s.ch(0)) {
				if breaks == "" {
					chunks.WriteString(" ")
				}
			} else {
				chunks.WriteString(lineBreak)
			}
		} else {
			break
		}
	}
	if chomping != -1 {
		chunks.WriteString(lineBreak) // clip: single trailing newline
	}
	if chomping == 1 {
		chunks.WriteString(breaks) // keep: all trailing newlines
	}
	style := Literal
	if folded {
		style = Folded
	}
	return token{kind: tokScalar, val: chunks.String(), style: style, line: startLine, col: startCol}, nil
}

// scanBlockScalarIndentation consumes leading blank lines and returns their
// newlines plus the deepest 0-based indentation seen.
func (s *scanner) scanBlockScalarIndentation() (string, int) {
	var breaks strings.Builder
	maxIndent := 0
	for {
		c := s.ch(0)
		if c == ' ' {
			s.advance()
			if s.col-1 > maxIndent {
				maxIndent = s.col - 1
			}
		} else if isBreak(c) {
			s.scanLineBreak()
			breaks.WriteString("\n")
		} else {
			return breaks.String(), maxIndent
		}
	}
}

// scanBlockScalarBreaks consumes trailing breaks and up to `indent` (0-based)
// leading spaces on each new line, returning the newlines seen.
func (s *scanner) scanBlockScalarBreaks(indent int) string {
	var breaks strings.Builder
	for s.col-1 < indent && s.ch(0) == ' ' {
		s.advance()
	}
	for isBreak(s.ch(0)) {
		s.scanLineBreak()
		breaks.WriteString("\n")
		for s.col-1 < indent && s.ch(0) == ' ' {
			s.advance()
		}
	}
	return breaks.String()
}
