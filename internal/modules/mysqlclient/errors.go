package mysqlclient

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error is a server or client error. Error() renders it as str() of the
// PyMySQL exception: "(1045, \"Access denied for user ...\")", or the bare
// message for single-argument exceptions.
type Error struct {
	Code   int
	Msg    string
	Single bool // str(exc) is just the message
}

func (e *Error) Error() string {
	if e.Single {
		return e.Msg
	}
	return fmt.Sprintf("(%d, %s)", e.Code, PyRepr(e.Msg))
}

// parseErrPacket decodes an ERR packet (0xff, code, [#sqlstate], message).
func parseErrPacket(data []byte) error {
	if len(data) < 3 {
		return malformed()
	}
	code := int(int16(binary.LittleEndian.Uint16(data[1:])))
	msg := data[3:]
	if len(msg) > 0 && msg[0] == '#' && len(msg) >= 6 {
		msg = msg[6:]
	}
	return &Error{Code: code, Msg: strings.ToValidUTF8(string(msg), "�")}
}

// PyRepr is Python's repr() of a str.
func PyRepr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == utf8.RuneError:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyBytesRepr is repr() of an ASCII bytes value (b'...').
func pyBytesRepr(s string) string { return "b" + PyRepr(s) }
