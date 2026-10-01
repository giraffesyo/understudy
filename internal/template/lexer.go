package template

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// lexer is a two-mode state machine: TEXT mode scans literal template text up
// to {{ / {% / {#; TAG mode tokenizes the expression language until the
// matching closer. Whitespace-control markers ({{- and -}}) trim adjacent
// text, and {% raw %} is handled here so its body is never tokenized.
type lexer struct {
	src    string
	pos    int // byte offset
	tokens []token
	opts   Options
	tplPos Position // document position of the template, for errors

	blockStart, blockEnd, varStart, varEnd, commentStart, commentEnd string

	// escapeBackslashes is ansible-core's escape_backslashes: string
	// literals in {{ }} keep their backslashes (it doubles them before
	// Jinja unescapes them); inVar marks the lexer inside {{ }}.
	escapeBackslashes, inVar bool
}

func lex(src string, opts Options, tplPos Position) ([]token, error) {
	return lexEscaping(src, opts, tplPos, false)
}

// lexEscaping is lex with ansible-core's escape_backslashes when set.
func lexEscaping(src string, opts Options, tplPos Position, escapeBackslashes bool) ([]token, error) {
	l := &lexer{src: src, opts: opts, tplPos: tplPos, escapeBackslashes: escapeBackslashes}
	l.blockStart, l.blockEnd, l.varStart, l.varEnd, l.commentStart, l.commentEnd = opts.delims()
	if err := l.run(); err != nil {
		return nil, err
	}
	if nl := opts.NewlineSequence; nl != "" {
		for i, t := range l.tokens {
			if t.kind == tokText {
				l.tokens[i].val = normalizeNewlines(t.val, nl)
			}
		}
	}
	return l.tokens, nil
}

// normalizeNewlines is Jinja's _normalize_newlines: \r\n, \r and \n in
// template data all become the environment's newline_sequence.
func normalizeNewlines(s, nl string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if nl == "\n" {
		return s
	}
	return strings.ReplaceAll(s, "\n", nl)
}

func (l *lexer) errf(format string, args ...any) error {
	return &TemplateError{Pos: l.tplPos, Msg: sprintf(format, args...), Src: l.src, Off: l.pos, Syntax: true}
}

func (l *lexer) run() error {
	for {
		start := l.pos
		text, found := l.scanText()
		if !found {
			if text != "" {
				l.tokens = append(l.tokens, token{kind: tokText, val: text, off: start})
			}
			l.tokens = append(l.tokens, token{kind: tokEOF, off: l.pos})
			return nil
		}

		marker, openLen := l.markerAt(l.pos) // '{', '%', or '#'

		// Whitespace control: a '-' right after the opener trims trailing
		// whitespace from the preceding text.
		trimBefore := l.pos+openLen < len(l.src) && l.src[l.pos+openLen] == '-'
		if trimBefore {
			text = strings.TrimRight(text, " \t\r\n")
		} else if (marker == '%' || marker == '#') && l.opts.LstripBlocks {
			// Strip whitespace from the start of the line the block tag sits on.
			if i := strings.LastIndexByte(text, '\n'); i >= 0 {
				if strings.TrimRight(text[i+1:], " \t") == "" {
					text = text[:i+1]
				}
			} else if strings.TrimRight(text, " \t") == "" && onLineStart(l.src, start) {
				text = ""
			}
		}
		if text != "" {
			l.tokens = append(l.tokens, token{kind: tokText, val: text, off: start})
		}

		switch marker {
		case '#':
			end := strings.Index(l.src[l.pos+openLen:], l.commentEnd)
			if end < 0 {
				return l.errf("unclosed comment (missing '%s')", l.commentEnd)
			}
			end += openLen
			closeEnd := l.pos + end + len(l.commentEnd)
			trimAfter := end >= 1 && l.src[l.pos+end-1] == '-'
			l.pos = closeEnd
			// trim_blocks also eats the newline after a comment.
			l.applyTrimAfter(trimAfter, true)
		case '{':
			l.tokens = append(l.tokens, token{kind: tokVarStart, off: l.pos})
			l.pos += openLen
			if trimBefore {
				l.pos++
			}
			if err := l.lexTag(tokVarEnd); err != nil {
				return err
			}
		case '%':
			openOff := l.pos
			l.pos += openLen
			if trimBefore {
				l.pos++
			}
			// {% raw %} swallows everything up to {% endraw %} as text.
			if name, after := l.peekBlockName(); name == "raw" {
				l.pos = after
				if err := l.finishRawBlock(); err != nil {
					return err
				}
				continue
			}
			l.tokens = append(l.tokens, token{kind: tokBlockStart, off: openOff})
			if err := l.lexTag(tokBlockEnd); err != nil {
				return err
			}
		}
	}
}

// scanText advances to the next tag opener ({{, {%, or {# by default),
// returning the literal text before it. found=false means the rest of
// the source is text.
func (l *lexer) scanText() (string, bool) {
	start := l.pos
	for p := l.pos; p < len(l.src); p++ {
		if m, _ := l.markerAt(p); m != 0 {
			l.pos = p
			return l.src[start:p], true
		}
	}
	l.pos = len(l.src)
	return l.src[start:], false
}

// markerAt reports which opener starts at p ('{' variable, '%' block,
// '#' comment, 0 none) and its length; the longest opener wins, as in
// Jinja's lexer.
func (l *lexer) markerAt(p int) (byte, int) {
	best, bestLen := byte(0), 0
	for _, o := range []struct {
		s string
		m byte
	}{{l.varStart, '{'}, {l.blockStart, '%'}, {l.commentStart, '#'}} {
		if len(o.s) > bestLen && strings.HasPrefix(l.src[p:], o.s) {
			best, bestLen = o.m, len(o.s)
		}
	}
	return best, bestLen
}

func onLineStart(src string, off int) bool {
	return off == 0 || src[off-1] == '\n'
}

// applyTrimAfter handles '-' before a closer (trim all following whitespace)
// and TrimBlocks (a block tag's closer eats one following newline).
func (l *lexer) applyTrimAfter(trimAfter, blockTag bool) {
	if trimAfter {
		for l.pos < len(l.src) {
			switch l.src[l.pos] {
			case ' ', '\t', '\r', '\n':
				l.pos++
				continue
			}
			break
		}
		return
	}
	if blockTag && l.opts.TrimBlocks {
		if l.pos < len(l.src) && l.src[l.pos] == '\n' {
			l.pos++
		} else if l.pos+1 < len(l.src) && l.src[l.pos] == '\r' && l.src[l.pos+1] == '\n' {
			l.pos += 2
		}
	}
}

// peekBlockName reads the identifier after '{%' (and optional '-')
// without consuming, returning the name and the offset just past it.
func (l *lexer) peekBlockName() (string, int) {
	p := l.pos
	for p < len(l.src) && (l.src[p] == ' ' || l.src[p] == '\t') {
		p++
	}
	start := p
	for p < len(l.src) && (isNameByte(l.src[p])) {
		p++
	}
	return l.src[start:p], p
}

func (l *lexer) finishRawBlock() error {
	bs, be := l.blockStart, l.blockEnd
	// Consume the rest of the {% raw %} tag.
	end := l.findTagEnd(be)
	if end < 0 {
		return l.errf("unclosed '%s raw %s' tag", bs, be)
	}
	trimAfterOpen := l.src[end-1] == '-'
	l.pos = end + len(be)
	l.applyTrimAfter(trimAfterOpen, true)
	bodyStart := l.pos
	// Find {% endraw %}.
	rest := l.src[l.pos:]
	for {
		i := strings.Index(rest, bs)
		if i < 0 {
			return l.errf("missing '%s endraw %s'", bs, be)
		}
		save := l.pos
		l.pos = save + i
		p := l.pos + len(bs)
		if p < len(l.src) && l.src[p] == '-' {
			p++
		}
		l.pos = p
		name, after := l.peekBlockName()
		if name == "endraw" {
			body := l.src[bodyStart : save+i]
			trimBefore := save+i+len(bs) < len(l.src) && l.src[save+i+len(bs)] == '-'
			if trimBefore {
				body = strings.TrimRight(body, " \t\r\n")
			}
			if body != "" {
				l.tokens = append(l.tokens, token{kind: tokText, val: body, off: bodyStart})
			}
			l.pos = after
			end := l.findTagEnd(be)
			if end < 0 {
				return l.errf("unclosed '%s endraw %s' tag", bs, be)
			}
			trimAfter := l.src[end-1] == '-'
			l.pos = end + len(be)
			l.applyTrimAfter(trimAfter, true)
			return nil
		}
		l.pos = save
		rest = l.src[save+i+len(bs):]
		if len(rest) == 0 {
			return l.errf("missing '%s endraw %s'", bs, be)
		}
		l.pos = save + i + len(bs)
		rest = l.src[l.pos:]
	}
}

// findTagEnd locates the closer within the current tag, skipping strings.
func (l *lexer) findTagEnd(closer string) int {
	inStr := byte(0)
	for i := l.pos; i < len(l.src); i++ {
		c := l.src[i]
		if inStr != 0 {
			if c == '\\' && inStr == '"' {
				i++
			} else if c == inStr {
				inStr = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			inStr = c
		case strings.HasPrefix(l.src[i:], closer):
			return i
		case c == '-' && strings.HasPrefix(l.src[i+1:], closer):
			return i + 1
		}
	}
	return -1
}

// lexTag tokenizes expression content until the matching closer token.
func (l *lexer) lexTag(closer tokKind) error {
	closeStr, openStr := l.varEnd, l.varStart
	if closer == tokBlockEnd {
		closeStr, openStr = l.blockEnd, l.blockStart
	}
	depth := 0 // bracket depth: a '}' at depth 0 may be part of '}}'
	l.inVar = closer == tokVarEnd
	defer func() { l.inVar = false }()
	for {
		l.skipTagWhitespace()
		if l.pos >= len(l.src) {
			return l.errf("unclosed '%s' (missing '%s')", openStr, closeStr)
		}
		c := l.src[l.pos]

		// Closing marker (with optional whitespace-control '-')?
		if depth == 0 {
			if c == '-' && strings.HasPrefix(l.src[l.pos+1:], closeStr) {
				l.tokens = append(l.tokens, token{kind: closer, off: l.pos})
				l.pos += 1 + len(closeStr)
				l.applyTrimAfter(true, closer == tokBlockEnd)
				return nil
			}
			if strings.HasPrefix(l.src[l.pos:], closeStr) {
				l.tokens = append(l.tokens, token{kind: closer, off: l.pos})
				l.pos += len(closeStr)
				l.applyTrimAfter(false, closer == tokBlockEnd)
				return nil
			}
		}

		start := l.pos
		switch {
		case c == '\'' || c == '"':
			s, err := l.lexString(c)
			if err != nil {
				return err
			}
			l.tokens = append(l.tokens, token{kind: tokString, val: s, off: start})
		case c >= '0' && c <= '9' ||
			c == '.' && !l.afterPostfixable() && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]):
			l.lexNumber()
		case isNameStartByte(c) || c >= utf8.RuneSelf:
			if !l.lexName() {
				return l.errf("unexpected character in template expression")
			}
		default:
			kind, size := l.lexOperator()
			if size == 0 {
				return l.errf("unexpected character %q in template expression", string(rune(c)))
			}
			switch kind {
			case tokLBracket, tokLParen, tokLBrace:
				depth++
			case tokRBracket, tokRParen, tokRBrace:
				depth--
			}
			l.tokens = append(l.tokens, token{kind: kind, off: start})
			l.pos += size
		}
	}
}

func (l *lexer) skipTagWhitespace() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', '\r', '\n':
			l.pos++
		default:
			return
		}
	}
}

// afterPostfixable reports whether the previous token can take a '.attr'
// postfix, so `l.0` lexes as attribute access rather than the float '.0'.
func (l *lexer) afterPostfixable() bool {
	if len(l.tokens) == 0 {
		return false
	}
	switch l.tokens[len(l.tokens)-1].kind {
	case tokName, tokString, tokRParen, tokRBracket, tokRBrace:
		return true
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isNameStartByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isNameByte(c byte) bool { return isNameStartByte(c) || isDigit(c) }

// lexName scans an identifier; it reports false if no valid name characters
// were consumed (e.g. an invalid UTF-8 byte).
func (l *lexer) lexName() bool {
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isNameByte(c) {
			l.pos++
			continue
		}
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			if r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
				l.pos += size
				continue
			}
		}
		break
	}
	if l.pos == start {
		return false
	}
	l.tokens = append(l.tokens, token{kind: tokName, val: l.src[start:l.pos], off: start})
	return true
}

func (l *lexer) lexNumber() {
	start := l.pos
	isFloat := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isDigit(c) || c == '_' {
			l.pos++
		} else if c == '.' && !isFloat && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]) {
			isFloat = true
			l.pos++
		} else if (c == 'e' || c == 'E') && l.pos+1 < len(l.src) &&
			(isDigit(l.src[l.pos+1]) || (l.src[l.pos+1] == '-' || l.src[l.pos+1] == '+') && l.pos+2 < len(l.src) && isDigit(l.src[l.pos+2])) {
			isFloat = true
			l.pos += 2
		} else {
			break
		}
	}
	kind := tokInt
	if isFloat {
		kind = tokFloat
	}
	l.tokens = append(l.tokens, token{kind: kind, val: strings.ReplaceAll(l.src[start:l.pos], "_", ""), off: start})
}

func (l *lexer) lexString(quote byte) (string, error) {
	l.pos++ // opening quote
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == quote {
			s := l.src[start:l.pos]
			l.pos++
			if l.escapeBackslashes && l.inVar {
				// Doubled, every backslash unescapes to itself: the
				// literal's text is its value.
				return s, nil
			}
			return pyUnicodeEscape(s), nil
		}
		if c == '\\' && l.pos+1 < len(l.src) {
			l.pos += 2 // an escaped character never ends the literal
			continue
		}
		l.pos++
	}
	return "", l.errf("unclosed string literal")
}

// pySimpleEscapes are unicode-escape's one-character escapes.
var pySimpleEscapes = map[byte]string{'\\': "\\", '\'': "'", '"': "\"", 'a': "\a", 'b': "\b", 'f': "\f",
	'n': "\n", 'r': "\r", 't': "\t", 'v': "\v", '\n': ""}

// pyUnicodeEscape is a Jinja string literal's value: its text decoded
// with Python's unicode-escape (an escape it does not know kept as is).
func pyUnicodeEscape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		e := s[i+1]
		if r, ok := pySimpleEscapes[e]; ok {
			b.WriteString(r)
			i++
			continue
		}
		if n, ok := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]; ok {
			if i+2+n <= len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32); err == nil && v <= unicode.MaxRune {
					b.WriteRune(rune(v))
					i += 1 + n
					continue
				}
			}
			b.WriteByte(c)
			continue
		}
		if e >= '0' && e <= '7' {
			j := i + 1
			v := 0
			for j < len(s) && j < i+4 && s[j] >= '0' && s[j] <= '7' {
				v = v*8 + int(s[j]-'0')
				j++
			}
			b.WriteRune(rune(v))
			i = j - 1
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// lexOperator matches the longest operator at the cursor.
func (l *lexer) lexOperator() (tokKind, int) {
	src := l.src[l.pos:]
	two := ""
	if len(src) >= 2 {
		two = src[:2]
	}
	switch two {
	case "**":
		return tokPow, 2
	case "//":
		return tokFloorDiv, 2
	case "==":
		return tokEq, 2
	case "!=":
		return tokNe, 2
	case "<=":
		return tokLe, 2
	case ">=":
		return tokGe, 2
	}
	switch src[0] {
	case '+':
		return tokAdd, 1
	case '-':
		return tokSub, 1
	case '*':
		return tokMul, 1
	case '/':
		return tokDiv, 1
	case '%':
		return tokMod, 1
	case '~':
		return tokTilde, 1
	case '<':
		return tokLt, 1
	case '>':
		return tokGt, 1
	case '=':
		return tokAssign, 1
	case '|':
		return tokPipe, 1
	case '.':
		return tokDot, 1
	case ',':
		return tokComma, 1
	case ':':
		return tokColon, 1
	case '(':
		return tokLParen, 1
	case ')':
		return tokRParen, 1
	case '[':
		return tokLBracket, 1
	case ']':
		return tokRBracket, 1
	case '{':
		return tokLBrace, 1
	case '}':
		return tokRBrace, 1
	}
	return tokEOF, 0
}
