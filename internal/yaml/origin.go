package yaml

import (
	"sync"
	"unsafe"
)

// Where a decoded string came from. ansible-core tags every value it loads
// with its Origin, so an error or deprecation raised while templating a
// variable's value points at the variable's definition, not at the task
// that used it. Decoded values here are plain Go strings, so the parser
// records each scalar's position by the string's backing array: the
// string a Decode returns (and every copy of it) is found again, while a
// string built from it (a substring, a concatenation) is not.
var origins sync.Map // *byte -> origin

type origin struct {
	s         string // keeps the backing array alive, so it is not reused
	file      string
	line, col int
}

// recordOrigin remembers where the scalar s was parsed.
func recordOrigin(s, file string, line, col int) {
	if s == "" || file == "" {
		return
	}
	origins.Store(unsafe.StringData(s), origin{s: s, file: file, line: line, col: col})
}

// Origin reports where s was parsed: the file and the 1-based line and
// column of its scalar node, when s is (a copy of) a decoded scalar.
func Origin(s string) (file string, line, col int, ok bool) {
	if s == "" {
		return "", 0, 0, false
	}
	v, found := origins.Load(unsafe.StringData(s))
	if !found {
		return "", 0, 0, false
	}
	o := v.(origin)
	if len(o.s) != len(s) {
		return "", 0, 0, false
	}
	return o.file, o.line, o.col, true
}
