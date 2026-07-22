package yaml

import (
	"fmt"
	"strings"
)

// Error is a YAML syntax or structure error with source position, formatted
// in the style Ansible users expect.
type Error struct {
	File    string
	Line    int
	Col     int
	Msg     string
	Snippet string // the offending source line, if available
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%d:%d: %s", e.File, e.Line, e.Col, e.Msg)
	if e.Snippet != "" {
		b.WriteString("\n  ")
		b.WriteString(e.Snippet)
		b.WriteString("\n  ")
		for i := 1; i < e.Col; i++ {
			b.WriteByte(' ')
		}
		b.WriteByte('^')
	}
	return b.String()
}

func (s *scanner) errf(line, col int, format string, args ...any) *Error {
	return &Error{
		File:    s.name,
		Line:    line,
		Col:     col,
		Msg:     fmt.Sprintf(format, args...),
		Snippet: s.sourceLine(line),
	}
}

// sourceLine extracts line n (1-based) from the source for error snippets.
func (s *scanner) sourceLine(n int) string {
	line := 1
	start := 0
	for i := 0; i < len(s.src); i++ {
		if line == n {
			start = i
			for i < len(s.src) && s.src[i] != '\n' {
				i++
			}
			return strings.TrimRight(string(s.src[start:i]), "\r")
		}
		if s.src[i] == '\n' {
			line++
		}
	}
	return ""
}
