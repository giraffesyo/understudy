package template

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// pyPercentFormat is Python's printf-style str % args (the format
// filter): args is a tuple of values, or with mapping set the dict that
// "%(key)s" conversions read.
func pyPercentFormat(format string, args []any, mapping map[string]any) (string, error) {
	var b strings.Builder
	argi := 0
	usedMapping := false
	next := func() (any, error) {
		if mapping != nil {
			// A mapping that is not a tuple is one argument.
			if usedMapping {
				return nil, fmt.Errorf("not enough arguments for format string")
			}
			usedMapping = true
			m := yaml.NewOMap()
			for _, k := range sortedKeys(mapping) {
				m.Set(k, mapping[k])
			}
			return m, nil
		}
		if argi >= len(args) {
			return nil, fmt.Errorf("not enough arguments for format string")
		}
		v := args[argi]
		argi++
		return v, nil
	}
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '%' {
			b.WriteRune(rs[i])
			continue
		}
		start := i
		i++
		if i >= len(rs) {
			return "", fmt.Errorf("incomplete format")
		}
		var key *string
		if rs[i] == '(' {
			depth, j := 1, i+1
			for j < len(rs) && depth > 0 {
				switch rs[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				j++
			}
			if depth > 0 {
				return "", fmt.Errorf("incomplete format key")
			}
			k := string(rs[i+1 : j-1])
			key = &k
			i = j
		}
		flags := ""
		for i < len(rs) && strings.ContainsRune("-+ #0", rs[i]) {
			flags += string(rs[i])
			i++
		}
		width, prec := -1, -1
		readNum := func() (int, error) {
			if i < len(rs) && rs[i] == '*' {
				i++
				v, err := next()
				if err != nil {
					return 0, err
				}
				n, ok := asInt(v)
				if !ok {
					return 0, fmt.Errorf("* wants int")
				}
				return int(n), nil
			}
			n := -1
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				if n < 0 {
					n = 0
				}
				n = n*10 + int(rs[i]-'0')
				i++
			}
			return n, nil
		}
		var err error
		if width, err = readNum(); err != nil {
			return "", err
		}
		if width < -1 { // a negative * width left-aligns
			flags += "-"
			width = -width
		}
		if i < len(rs) && rs[i] == '.' {
			i++
			if prec, err = readNum(); err != nil {
				return "", err
			}
			if prec < 0 {
				prec = 0
			}
		}
		for i < len(rs) && strings.ContainsRune("hlL", rs[i]) {
			i++
		}
		if i >= len(rs) {
			return "", fmt.Errorf("incomplete format")
		}
		conv := rs[i]
		if conv == '%' {
			b.WriteByte('%')
			continue
		}
		var v any
		if key != nil {
			if mapping == nil {
				return "", fmt.Errorf("format requires a mapping")
			}
			val, ok := mapping[*key]
			if !ok {
				return "", fmt.Errorf("%s", pyStrRepr(*key))
			}
			v = val
		} else if v, err = next(); err != nil {
			return "", err
		}
		s, err := pyConvert(conv, v, flags, prec, start, i)
		if err != nil {
			return "", err
		}
		if width > 0 && utf8.RuneCountInString(s) < width {
			pad := width - utf8.RuneCountInString(s)
			switch {
			case strings.Contains(flags, "-"):
				s += strings.Repeat(" ", pad)
			case strings.Contains(flags, "0") && strings.ContainsRune("diouxXeEfFgG", conv):
				sign := ""
				if s != "" && (s[0] == '-' || s[0] == '+' || s[0] == ' ') {
					sign, s = s[:1], s[1:]
				}
				prefix := ""
				if len(s) > 1 && s[0] == '0' && strings.ContainsRune("xXo", rune(s[1])) {
					prefix, s = s[:2], s[2:]
				}
				s = sign + prefix + strings.Repeat("0", pad) + s
			default:
				s = strings.Repeat(" ", pad) + s
			}
		}
		b.WriteString(s)
	}
	if mapping == nil && argi < len(args) {
		return "", fmt.Errorf("not all arguments converted during string formatting")
	}
	return b.String(), nil
}

// pyConvert renders one conversion of Python's % formatting.
func pyConvert(conv rune, v any, flags string, prec, start, at int) (string, error) {
	v = Undeprecate(v)
	sign := func(neg bool) string {
		switch {
		case neg:
			return "-"
		case strings.Contains(flags, "+"):
			return "+"
		case strings.Contains(flags, " "):
			return " "
		}
		return ""
	}
	switch conv {
	case 's':
		s := toStr(v)
		if prec >= 0 && utf8.RuneCountInString(s) > prec {
			s = string([]rune(s)[:prec])
		}
		return s, nil
	case 'r', 'a':
		s := pyRepr(v)
		if prec >= 0 && utf8.RuneCountInString(s) > prec {
			s = string([]rune(s)[:prec])
		}
		return s, nil
	case 'd', 'i', 'u', 'o', 'x', 'X':
		var n *big.Int
		if f, ok := v.(float64); ok && strings.ContainsRune("diu", conv) {
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return "", fmt.Errorf("cannot convert float %s to integer", pyFloatStr(f))
			}
			iv, _ := floatToInt(f)
			n, _ = asBigInt(iv)
		} else if bi, ok := asBigInt(v); ok {
			n = bi
		} else {
			if strings.ContainsRune("diu", conv) {
				return "", fmt.Errorf("%%%c format: a real number is required, not %s", conv, pyClassName(v, false))
			}
			return "", fmt.Errorf("%%%c format: an integer is required, not %s", conv, pyClassName(v, false))
		}
		base, prefix := 10, ""
		switch conv {
		case 'o':
			base, prefix = 8, "0o"
		case 'x':
			base, prefix = 16, "0x"
		case 'X':
			base, prefix = 16, "0X"
		}
		digits := new(big.Int).Abs(n).Text(base)
		if conv == 'X' {
			digits = strings.ToUpper(digits)
		}
		if prec > len(digits) {
			digits = strings.Repeat("0", prec-len(digits)) + digits
		}
		if !strings.Contains(flags, "#") {
			prefix = ""
		}
		return sign(n.Sign() < 0) + prefix + digits, nil
	case 'e', 'E', 'f', 'F', 'g', 'G':
		f, ok := asFloat(v)
		if bi, isBig := v.(*big.Int); isBig {
			var err error
			if f, err = bigToFloat(bi); err != nil {
				return "", err
			}
		} else if !ok {
			return "", fmt.Errorf("must be real number, not %s", pyClassName(v, false))
		}
		if prec < 0 {
			prec = 6
		}
		verb := string(conv)
		if conv == 'F' {
			verb = "f"
		}
		goFlags := ""
		if strings.Contains(flags, "#") {
			goFlags = "#"
		}
		var s string
		switch {
		case math.IsInf(f, 0):
			s = "inf"
		case math.IsNaN(f):
			s = "nan"
		default:
			s = fmt.Sprintf("%"+goFlags+"."+strconv.Itoa(prec)+verb, math.Abs(f))
		}
		if strings.ContainsRune("EFG", conv) {
			s = strings.ToUpper(s)
		}
		return sign(math.Signbit(f) && !math.IsNaN(f)) + s, nil
	case 'c':
		if s, ok := asString(v); ok {
			if utf8.RuneCountInString(s) != 1 {
				return "", fmt.Errorf("%%c requires an int or a unicode character, not a string of length %d", utf8.RuneCountInString(s))
			}
			return s, nil
		}
		if n, ok := asInt(v); ok {
			if n < 0 || n > 0x10ffff {
				return "", fmt.Errorf("%%c arg not in range(0x110000)")
			}
			return string(rune(n)), nil
		}
		return "", fmt.Errorf("%%c requires an int or a unicode character, not %s", pyClassName(v, false))
	}
	return "", fmt.Errorf("unsupported format character %s (0x%x) at index %d", pyStrRepr(string(conv)), conv, at)
}
