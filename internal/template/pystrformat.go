package template

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pyStrFormat is Python's str.format(*args, **kwargs): "{}" fields
// numbered automatically or by position, named by keyword, with
// attribute and item lookups, a !r/!s/!a conversion and a format spec.
// markup escapes each field's text (markupsafe's EscapeFormatter).
func pyStrFormat(format string, args []any, kwargs map[string]any, markup bool) (string, error) {
	var b strings.Builder
	auto, manual := 0, false
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '}' {
			if i+1 < len(rs) && rs[i+1] == '}' {
				b.WriteRune('}')
				i++
				continue
			}
			return "", fmt.Errorf("Single '}' encountered in format string")
		}
		if c != '{' {
			b.WriteRune(c)
			continue
		}
		if i+1 < len(rs) && rs[i+1] == '{' {
			b.WriteRune('{')
			i++
			continue
		}
		// The field, up to its matching brace.
		depth, j := 1, i+1
		for j < len(rs) && depth > 0 {
			switch rs[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			j++
		}
		if depth > 0 {
			if j >= len(rs) && strings.ContainsRune(string(rs[i+1:]), '{') {
				return "", fmt.Errorf("expected '}' before end of string")
			}
			return "", fmt.Errorf("Single '{' encountered in format string")
		}
		field := string(rs[i+1 : j-1])
		i = j - 1
		name, conv, spec := field, "", ""
		if k := strings.IndexAny(name, "!:"); k >= 0 {
			rest := name[k:]
			name = name[:k]
			if rest[0] == '!' {
				if len(rest) < 2 {
					return "", fmt.Errorf("end of string while looking for conversion specifier")
				}
				conv = rest[1:2]
				rest = rest[2:]
				if rest != "" && rest[0] != ':' {
					return "", fmt.Errorf("expected ':' after conversion specifier")
				}
			}
			if rest != "" {
				spec = rest[1:]
			}
		}
		// The first part names the argument; .attr and [key] follow.
		first, lookups := name, ""
		if k := strings.IndexAny(name, ".["); k >= 0 {
			first, lookups = name[:k], name[k:]
		}
		var v any
		if first == "" {
			if manual {
				return "", fmt.Errorf("cannot switch from manual field specification to automatic field numbering")
			}
			if auto >= len(args) {
				return "", fmt.Errorf("Replacement index %d out of range for positional args tuple", auto)
			}
			v = args[auto]
			auto++
		} else if n, err := strconv.Atoi(first); err == nil {
			if auto > 0 {
				return "", fmt.Errorf("cannot switch from automatic field numbering to manual field specification")
			}
			manual = true
			if n >= len(args) {
				return "", fmt.Errorf("Replacement index %d out of range for positional args tuple", n)
			}
			v = args[n]
		} else {
			kv, ok := kwargs[first]
			if !ok {
				return "", &pyKeyError{key: first}
			}
			v = kv
		}
		for lookups != "" {
			if lookups[0] == '.' {
				k := strings.IndexAny(lookups[1:], ".[")
				attr := lookups[1:]
				if k >= 0 {
					attr = lookups[1 : k+1]
				}
				return "", fmt.Errorf("%s object has no attribute %s", pyStrRepr(pyClassName(v, false)), pyStrRepr(attr))
			}
			k := strings.IndexByte(lookups, ']')
			if k < 0 {
				return "", fmt.Errorf("Missing ']' in format string")
			}
			key := lookups[1:k]
			lookups = lookups[k+1:]
			switch t := Undeprecate(v).(type) {
			case []any:
				n, err := strconv.Atoi(key)
				if err != nil {
					return "", fmt.Errorf("list indices must be integers or slices, not str")
				}
				if n < 0 || n >= len(t) {
					return "", fmt.Errorf("list index out of range")
				}
				v = t[n]
			default:
				m, ok := anyToMap(t)
				if !ok {
					return "", fmt.Errorf("%s object is not subscriptable", pyStrRepr(pyClassName(v, false)))
				}
				item, found := m[key]
				if !found {
					return "", &pyKeyError{key: key}
				}
				v = item
			}
		}
		// A spec's own fields are replaced first.
		if strings.ContainsRune(spec, '{') {
			s, err := pyStrFormat(spec, args[min(auto, len(args)):], kwargs, false)
			if err != nil {
				return "", err
			}
			spec = s
		}
		switch conv {
		case "":
		case "s":
			v = toStr(v)
		case "r", "a":
			v = pyRepr(v)
		default:
			return "", fmt.Errorf("Unknown conversion specifier %s", conv)
		}
		if markup {
			v = markupArg(v)
			if m, ok := v.(Markup); ok {
				v = string(m)
			}
		}
		s, err := pyFormatValue(v, spec)
		if err != nil {
			return "", err
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// pyFormatValue is format(v, spec): the format-spec mini-language for
// text and numbers; other values take only an empty spec.
func pyFormatValue(v any, spec string) (string, error) {
	v = Undeprecate(v)
	if spec == "" {
		return toStr(v), nil
	}
	rs := []rune(spec)
	i := 0
	fill, align := ' ', rune(0)
	if len(rs) >= 2 && strings.ContainsRune("<>=^", rs[1]) {
		fill, align = rs[0], rs[1]
		i = 2
	} else if len(rs) >= 1 && strings.ContainsRune("<>=^", rs[0]) {
		align = rs[0]
		i = 1
	}
	sign := ""
	if i < len(rs) && strings.ContainsRune("+- ", rs[i]) {
		sign = string(rs[i])
		i++
	}
	alt := false
	if i < len(rs) && rs[i] == '#' {
		alt = true
		i++
	}
	if i < len(rs) && rs[i] == '0' && align == 0 {
		fill, align = '0', '='
		i++
	}
	width := 0
	for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
		width = width*10 + int(rs[i]-'0')
		i++
	}
	group := ""
	if i < len(rs) && (rs[i] == ',' || rs[i] == '_') {
		group = string(rs[i])
		i++
	}
	prec := -1
	if i < len(rs) && rs[i] == '.' {
		i++
		prec = 0
		start := i
		for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
			prec = prec*10 + int(rs[i]-'0')
			i++
		}
		if i == start {
			return "", fmt.Errorf("Format specifier missing precision")
		}
	}
	typ := rune(0)
	if i < len(rs) {
		typ = rs[i]
		i++
	}
	if i < len(rs) {
		return "", fmt.Errorf("Invalid format specifier '%s' for object of type '%s'", spec, pyClassName(v, false))
	}
	var body string
	numeric := false
	switch t := v.(type) {
	case string, Markup:
		s, _ := asString(t)
		if typ != 0 && typ != 's' {
			return "", fmt.Errorf("Unknown format code '%c' for object of type 'str'", typ)
		}
		if sign != "" {
			return "", fmt.Errorf("Sign not allowed in string format specifier")
		}
		if align == '=' {
			return "", fmt.Errorf("'=' alignment not allowed in string format specifier")
		}
		if prec >= 0 && utf8.RuneCountInString(s) > prec {
			s = string([]rune(s)[:prec])
		}
		body = s
		if align == 0 {
			align = '<'
		}
	default:
		if !isNumber(v) {
			return "", fmt.Errorf("unsupported format string passed to %s.__format__", pyClassName(v, false))
		}
		numeric = true
		flags := ""
		if sign == "+" || sign == " " {
			flags = sign
		}
		if alt {
			flags += "#"
		}
		_, isFloat := v.(float64)
		conv := typ
		switch {
		case conv == 0 && isFloat:
			if prec < 0 {
				body = toStr(math.Abs(v.(float64)))
				if math.Signbit(v.(float64)) {
					body = "-" + body
				} else if sign == "+" || sign == " " {
					body = sign + body
				}
				conv = -1
			} else {
				conv = 'g'
			}
		case conv == 0, conv == 'n':
			conv = 'd'
		case conv == '%':
			f, _ := asFloat(v)
			if prec < 0 {
				prec = 6
			}
			s, err := pyConvert('f', f*100, flags, prec, 0, 0)
			if err != nil {
				return "", err
			}
			body = s + "%"
			conv = -1
		case conv == 'b':
			n, ok := asBigInt(v)
			if !ok || isFloat {
				return "", fmt.Errorf("Unknown format code 'b' for object of type '%s'", pyClassName(v, false))
			}
			digits := new(big.Int).Abs(n).Text(2)
			prefix := ""
			if alt {
				prefix = "0b"
			}
			pre := ""
			if n.Sign() < 0 {
				pre = "-"
			} else if sign == "+" || sign == " " {
				pre = sign
			}
			body = pre + prefix + digits
			conv = -1
		}
		if conv > 0 {
			if isFloat && strings.ContainsRune("dxXoc", conv) {
				return "", fmt.Errorf("Unknown format code '%c' for object of type 'float'", conv)
			}
			s, err := pyConvert(conv, v, flags, prec, 0, 0)
			if err != nil {
				return "", err
			}
			body = s
		}
		if group != "" {
			body = groupDigits(body, group)
		}
		if align == 0 {
			align = '>'
		}
	}
	n := utf8.RuneCountInString(body)
	if width <= n {
		return body, nil
	}
	pad := width - n
	fs := string(fill)
	switch align {
	case '<':
		return body + strings.Repeat(fs, pad), nil
	case '^':
		return strings.Repeat(fs, pad/2) + body + strings.Repeat(fs, pad-pad/2), nil
	case '=':
		if numeric {
			k := 0
			if body != "" && strings.ContainsRune("+- ", rune(body[0])) {
				k = 1
			}
			if len(body) > k+1 && body[k] == '0' && strings.ContainsRune("xXob", rune(body[k+1])) {
				k += 2
			}
			return body[:k] + strings.Repeat(fs, pad) + body[k:], nil
		}
	}
	return strings.Repeat(fs, pad) + body, nil
}

// groupDigits inserts sep between thousands of a number's integer part.
func groupDigits(s, sep string) string {
	start := 0
	for start < len(s) && !(s[start] >= '0' && s[start] <= '9') {
		start++
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	digits := s[start:end]
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(d)
	}
	return s[:start] + b.String() + s[end:]
}
