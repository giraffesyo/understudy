package pyre

import "strings"

// Python's bytes patterns, run on the str engine: each byte is the code
// point of the same value (latin-1), as re's parser decodes them. They
// parse as re parses bytes (no \u, \U or \N escapes, no (?u), (?L)
// allowed, ASCII-only messages), and without the UNICODE flag a str
// pattern defaults to, so \w, \d, \s, \b and IGNORECASE have their bytes
// (ASCII) meaning; LOCALE is taken to be a locale with the same.
// Match positions count bytes.

// Latin1 is data as a str of its bytes.
func Latin1(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		b.WriteRune(rune(c))
	}
	return b.String()
}

// Bytes is the bytes a Latin1 str holds (a span of a bytes match).
func Bytes(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	return b
}

// CompileBytes is re.compile(pattern, flags) of a bytes pattern; match
// it against Latin1 subjects.
func CompileBytes(pattern []byte, flags Flag) (*Pattern, error) {
	return cachedCompile(Latin1(pattern), flags, true)
}

// IsBytes reports whether the pattern is a bytes pattern.
func (p *Pattern) IsBytes() bool { return p.bytes }
