package mysqlclient

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Mogrify is PyMySQL's client-side parameter binding: every argument is
// escaped to an SQL literal, then query % args is applied with Python's
// %-formatting (args a []any for %s placeholders, or a map[string]any for
// %(name)s ones). Errors carry Python's exception text.
func Mogrify(query string, args any, noBackslash bool) (string, error) {
	switch a := args.(type) {
	case map[string]any:
		esc := make(map[string]string, len(a))
		for k, v := range a {
			lit, err := Literal(v, noBackslash)
			if err != nil {
				return "", err
			}
			esc[k] = lit
		}
		return pyFormat(query, nil, esc, true)
	case []any:
		esc := make([]string, len(a))
		for i, v := range a {
			lit, err := Literal(v, noBackslash)
			if err != nil {
				return "", err
			}
			esc[i] = lit
		}
		return pyFormat(query, esc, nil, false)
	default:
		lit, err := Literal(args, noBackslash)
		if err != nil {
			return "", err
		}
		return pyFormat(query, []string{lit}, nil, false)
	}
}

// Literal renders a value as an SQL literal (PyMySQL's escape_item).
func Literal(v any, noBackslash bool) (string, error) {
	switch t := v.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if t {
			return "1", nil
		}
		return "0", nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case uint64:
		return strconv.FormatUint(t, 10), nil
	case float64:
		s := PyFloatRepr(t)
		if s == "inf" || s == "-inf" || s == "nan" {
			return "", &Error{Code: -1, Msg: s + " can not be used with MySQL", Single: true}
		}
		if !strings.Contains(s, "e") {
			s += "e0"
		}
		return s, nil
	case string:
		return QuoteString(t, noBackslash), nil
	case Bytes:
		return "_binary X'" + hex.EncodeToString(t) + "'", nil
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			lit, err := Literal(e, noBackslash)
			if err != nil {
				return "", err
			}
			parts[i] = lit
		}
		return "(" + strings.Join(parts, ",") + ")", nil
	case map[string]any:
		return "", &Error{Code: -1, Msg: "dict can not be used as parameter", Single: true}
	}
	return QuoteString(fmt.Sprint(v), noBackslash), nil
}

// QuoteString is escape_str: a single-quoted literal with backslash
// escapes (or doubled quotes under NO_BACKSLASH_ESCAPES).
func QuoteString(s string, noBackslash bool) string {
	if noBackslash {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case 0:
			b.WriteString(`\0`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\032':
			b.WriteString(`\Z`)
		case '"':
			b.WriteString(`\"`)
		case '\'':
			b.WriteString(`\'`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// PyFloatRepr is Python's repr(float).
func PyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// pyFormat applies Python's str % args to already-rendered string
// arguments.
func pyFormat(format string, pos []string, named map[string]string, isMap bool) (string, error) {
	var b strings.Builder
	used := 0
	usedMapping := false
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		start := i
		i++
		if i >= len(format) {
			return "", &Error{Code: -1, Msg: "incomplete format", Single: true}
		}
		var arg string
		haveKey := false
		if format[i] == '(' {
			depth := 1
			j := i + 1
			for ; j < len(format) && depth > 0; j++ {
				switch format[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			if depth > 0 {
				return "", &Error{Code: -1, Msg: "incomplete format key", Single: true}
			}
			key := format[i+1 : j-1]
			if !isMap {
				return "", &Error{Code: -1, Msg: "format requires a mapping", Single: true}
			}
			v, ok := named[key]
			if !ok {
				return "", &Error{Code: -1, Msg: PyRepr(key), Single: true}
			}
			arg, haveKey, usedMapping = v, true, true
			i = j
		}
		// flags, width, precision
		leftAlign := false
		for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
			if format[i] == '-' {
				leftAlign = true
			}
			i++
		}
		width := 0
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			width = width*10 + int(format[i]-'0')
			i++
		}
		prec := -1
		if i < len(format) && format[i] == '.' {
			i++
			prec = 0
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				prec = prec*10 + int(format[i]-'0')
				i++
			}
		}
		if i >= len(format) {
			return "", &Error{Code: -1, Msg: "incomplete format", Single: true}
		}
		conv := format[i]
		if conv == '%' && !haveKey {
			b.WriteByte('%')
			continue
		}
		if !haveKey {
			if isMap {
				// A mapping used positionally formats as its repr.
				arg = mapRepr(named)
				usedMapping = true
			} else {
				if used >= len(pos) {
					return "", &Error{Code: -1, Msg: "not enough arguments for format string", Single: true}
				}
				arg = pos[used]
				used++
			}
		}
		switch conv {
		case 's':
		case 'r', 'a':
			arg = PyRepr(arg)
		case 'd', 'i', 'u', 'o', 'x', 'X', 'e', 'E', 'f', 'F', 'g', 'G':
			return "", &Error{Code: -1, Msg: fmt.Sprintf("%%%c format: a real number is required, not str", conv), Single: true}
		case 'c':
			if len([]rune(arg)) != 1 {
				return "", &Error{Code: -1, Msg: fmt.Sprintf("%%c requires an int or a unicode character, not a string of length %d", len([]rune(arg))), Single: true}
			}
		default:
			return "", &Error{Code: -1, Msg: fmt.Sprintf("unsupported format character '%c' (0x%x) at index %d", conv, conv, i), Single: true}
		}
		_ = start
		if prec >= 0 && len([]rune(arg)) > prec {
			arg = string([]rune(arg)[:prec])
		}
		if pad := width - len([]rune(arg)); pad > 0 {
			if leftAlign {
				arg += strings.Repeat(" ", pad)
			} else {
				arg = strings.Repeat(" ", pad) + arg
			}
		}
		b.WriteString(arg)
	}
	if !isMap && used < len(pos) {
		return "", &Error{Code: -1, Msg: "not all arguments converted during string formatting", Single: true}
	}
	_ = usedMapping
	return b.String(), nil
}

func mapRepr(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = PyRepr(k) + ": " + PyRepr(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
