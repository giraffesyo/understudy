package callback

import (
	"io"
	"sync"
	"unicode/utf8"
)

// utf8Display is ansible-core's Display stream encoding: text holding
// undecodable bytes (Python's surrogate escapes, which a Go string keeps
// as the bytes themselves) is written with each run of them replaced by
// one "?", after a warning on stderr (once, as Display de-duplicates it).
type utf8Display struct {
	w    io.Writer
	warn io.Writer
	once *sync.Once
}

const nonUTF8Warning = "[WARNING]: Non UTF-8 encoded data replaced with \"?\" while displaying text to stdout/stderr.\n"

func (u *utf8Display) Write(p []byte) (int, error) {
	if utf8.Valid(p) {
		return u.w.Write(p)
	}
	out := make([]byte, 0, len(p))
	inRun := false
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError && size == 1 {
			if !inRun {
				out = append(out, '?')
				inRun = true
			}
			i++
			continue
		}
		inRun = false
		out = append(out, p[i:i+size]...)
		i += size
	}
	if u.warn != nil {
		u.once.Do(func() { io.WriteString(u.warn, nonUTF8Warning) })
	}
	if _, err := u.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

var nonUTF8Once sync.Once
