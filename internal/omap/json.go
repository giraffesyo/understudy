package omap

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSONError is Python's json.JSONDecodeError: the message and where in
// the document (a character offset, with its line and column) decoding
// stopped.
type JSONError struct {
	Msg            string
	Pos, Line, Col int
}

func (e *JSONError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.Msg, e.Line, e.Col, e.Pos)
}

// UnmarshalJSON decodes JSON as Python's json.loads does: objects become
// *OMap in document order (a repeated key keeps its first position and
// last value), integer literals int64 (*big.Int beyond it), other numbers
// float64, NaN and Infinity accepted; malformed input fails with
// json.loads' JSONDecodeError.
func UnmarshalJSON(data []byte) (any, error) {
	d := &jsonDecoder{s: []rune(string(data))}
	return d.decode()
}

type jsonDecoder struct {
	s []rune
}

// errAt is a JSONDecodeError at character pos.
func (d *jsonDecoder) errAt(msg string, pos int) error {
	line, col := 1, pos+1
	for i := 0; i < pos && i < len(d.s); i++ {
		if d.s[i] == '\n' {
			line++
			col = pos - i
		}
	}
	return &JSONError{Msg: msg, Pos: pos, Line: line, Col: col}
}

// stopIteration is the scanner finding no value at pos.
type stopIteration struct{ pos int }

func (e *stopIteration) Error() string { return "StopIteration" }

func (d *jsonDecoder) ws(i int) int {
	for i < len(d.s) {
		switch d.s[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		}
		break
	}
	return i
}

func (d *jsonDecoder) decode() (any, error) {
	if len(d.s) > 0 && d.s[0] == 0xfeff {
		return nil, d.errAt("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	idx := d.ws(0)
	v, end, err := d.scanOnce(idx)
	if err != nil {
		if si, ok := err.(*stopIteration); ok {
			return nil, d.errAt("Expecting value", si.pos)
		}
		return nil, err
	}
	end = d.ws(end)
	if end != len(d.s) {
		return nil, d.errAt("Extra data", end)
	}
	return v, nil
}

func (d *jsonDecoder) hasAt(i int, lit string) bool {
	r := []rune(lit)
	if i+len(r) > len(d.s) {
		return false
	}
	for j, c := range r {
		if d.s[i+j] != c {
			return false
		}
	}
	return true
}

func (d *jsonDecoder) scanOnce(idx int) (any, int, error) {
	if idx >= len(d.s) {
		return nil, idx, &stopIteration{idx}
	}
	switch c := d.s[idx]; {
	case c == '"':
		return d.scanString(idx + 1)
	case c == '{':
		return d.parseObject(idx + 1)
	case c == '[':
		return d.parseArray(idx + 1)
	case c == 'n' && d.hasAt(idx, "null"):
		return nil, idx + 4, nil
	case c == 't' && d.hasAt(idx, "true"):
		return true, idx + 4, nil
	case c == 'f' && d.hasAt(idx, "false"):
		return false, idx + 5, nil
	}
	if v, end, ok := d.matchNumber(idx); ok {
		return v, end, nil
	}
	switch {
	case d.hasAt(idx, "NaN"):
		return math.NaN(), idx + 3, nil
	case d.hasAt(idx, "Infinity"):
		return math.Inf(1), idx + 8, nil
	case d.hasAt(idx, "-Infinity"):
		return math.Inf(-1), idx + 9, nil
	}
	return nil, idx, &stopIteration{idx}
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// matchNumber is NUMBER_RE: (-?(?:0|[1-9]\d*))(\.\d+)?([eE][-+]?\d+)?
func (d *jsonDecoder) matchNumber(start int) (any, int, bool) {
	s := d.s
	i := start
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isASCIIDigit(s[i]) {
			i++
		}
	case i < len(s) && s[i] == '0':
		i++
	default:
		return nil, start, false
	}
	isFloat := false
	if i+1 < len(s) && s[i] == '.' && isASCIIDigit(s[i+1]) {
		i += 2
		for i < len(s) && isASCIIDigit(s[i]) {
			i++
		}
		isFloat = true
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isASCIIDigit(s[j]) {
			for j < len(s) && isASCIIDigit(s[j]) {
				j++
			}
			i = j
			isFloat = true
		}
	}
	text := string(s[start:i])
	if isFloat {
		f, _ := strconv.ParseFloat(text, 64) // out of range: ±Inf, as float()
		return f, i, true
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, i, true
	}
	b, _ := new(big.Int).SetString(text, 10)
	return b, i, true
}

// scanString is the C scanner's scanstring: end is just past the opening
// quote.
func (d *jsonDecoder) scanString(end int) (any, int, error) {
	s := d.s
	begin := end - 1
	var b strings.Builder
	for {
		if end >= len(s) {
			return nil, end, d.errAt("Unterminated string starting at", begin)
		}
		c := s[end]
		if c == '"' {
			return b.String(), end + 1, nil
		}
		if c != '\\' {
			if c < 0x20 {
				return nil, end, d.errAt("Invalid control character at", end)
			}
			b.WriteRune(c)
			end++
			continue
		}
		next := end + 1
		if next >= len(s) {
			return nil, next, d.errAt("Unterminated string starting at", begin)
		}
		c = s[next]
		if c != 'u' {
			esc, ok := map[rune]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[c]
			if !ok {
				return nil, next, d.errAt("Invalid \\escape", next-1)
			}
			b.WriteRune(esc)
			end = next + 1
			continue
		}
		next++
		hexEnd := next + 4
		if hexEnd >= len(s) {
			return nil, next, d.errAt("Invalid \\uXXXX escape", next-1)
		}
		u, ok := hex4(s[next:hexEnd])
		if !ok {
			return nil, next, d.errAt("Invalid \\uXXXX escape", hexEnd-5)
		}
		end = hexEnd
		if u >= 0xd800 && u <= 0xdbff && end+6 < len(s) && s[end] == '\\' && s[end+1] == 'u' {
			if lo, ok := hex4(s[end+2 : end+6]); ok && lo >= 0xdc00 && lo <= 0xdfff {
				u = 0x10000 + ((u - 0xd800) << 10) + (lo - 0xdc00)
				end += 6
			}
		}
		if u >= 0xd800 && u <= 0xdfff {
			u = utf8.RuneError // a lone surrogate has no UTF-8 form
		}
		b.WriteRune(u)
	}
}

func hex4(r []rune) (rune, bool) {
	var v rune
	for _, c := range r {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v += c - '0'
		case c >= 'a' && c <= 'f':
			v += c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v += c - 'A' + 10
		default:
			return 0, false
		}
	}
	return v, true
}

func (d *jsonDecoder) parseObject(end int) (any, int, error) {
	s := d.s
	m := NewOMap()
	end = d.ws(end)
	if end < len(s) && s[end] == '}' {
		return m, end + 1, nil
	}
	if end >= len(s) || s[end] != '"' {
		return nil, end, d.errAt("Expecting property name enclosed in double quotes", end)
	}
	end++
	for {
		k, e, err := d.scanString(end)
		if err != nil {
			return nil, e, err
		}
		key := k.(string)
		end = d.ws(e)
		if end >= len(s) || s[end] != ':' {
			return nil, end, d.errAt("Expecting ':' delimiter", end)
		}
		end = d.ws(end + 1)
		v, e, err := d.scanOnce(end)
		if err != nil {
			if si, ok := err.(*stopIteration); ok {
				return nil, e, d.errAt("Expecting value", si.pos)
			}
			return nil, e, err
		}
		m.Set(key, v)
		end = d.ws(e)
		if end < len(s) && s[end] == '}' {
			return m, end + 1, nil
		}
		if end >= len(s) || s[end] != ',' {
			return nil, end, d.errAt("Expecting ',' delimiter", end)
		}
		comma := end
		end = d.ws(end + 1)
		if end >= len(s) || s[end] != '"' {
			if end < len(s) && s[end] == '}' {
				return nil, end, d.errAt("Illegal trailing comma before end of object", comma)
			}
			return nil, end, d.errAt("Expecting property name enclosed in double quotes", end)
		}
		end++
	}
}

func (d *jsonDecoder) parseArray(end int) (any, int, error) {
	s := d.s
	out := []any{}
	end = d.ws(end)
	if end < len(s) && s[end] == ']' {
		return out, end + 1, nil
	}
	for {
		v, e, err := d.scanOnce(end)
		if err != nil {
			if si, ok := err.(*stopIteration); ok {
				return nil, e, d.errAt("Expecting value", si.pos)
			}
			return nil, e, err
		}
		out = append(out, v)
		end = d.ws(e)
		if end < len(s) && s[end] == ']' {
			return out, end + 1, nil
		}
		if end >= len(s) || s[end] != ',' {
			return nil, end, d.errAt("Expecting ',' delimiter", end)
		}
		comma := end
		end = d.ws(end + 1)
		if end < len(s) && s[end] == ']' {
			return nil, end, d.errAt("Illegal trailing comma before end of array", comma)
		}
	}
}
