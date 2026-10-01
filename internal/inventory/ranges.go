package inventory

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ExpandRange expands Ansible host ranges: web[01:20].example.com,
// db[a:f], node[1:20:2]. Zero-padding of the start token is preserved.
// A name with no range expands to itself.
func ExpandRange(name string) ([]string, error) {
	if !strings.Contains(name, "[") {
		return []string{name}, nil
	}
	return expandHostnameRange(name)
}

const asciiLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// expandHostnameRange is ansible-core's expand_hostname_range, errors
// included (Python's own, for malformed input, as ansible-core lets them
// surface).
func expandHostnameRange(line string) ([]string, error) {
	parts := strings.Split(strings.Replace(strings.Replace(line, "[", "|", 1), "]", "|", 1), "|")
	if len(parts) != 3 {
		if len(parts) < 3 {
			return nil, fmt.Errorf("not enough values to unpack (expected 3, got %d)", len(parts))
		}
		return nil, fmt.Errorf("too many values to unpack (expected 3, got %d)", len(parts))
	}
	head, nrange, tail := parts[0], parts[1], parts[2]
	bounds := strings.Split(nrange, ":")
	if len(bounds) != 2 && len(bounds) != 3 {
		return nil, fmt.Errorf("host range must be begin:end or begin:end:step")
	}
	beg, end := bounds[0], bounds[1]
	step := "1"
	if len(bounds) == 3 {
		step = bounds[2]
	}
	if beg == "" {
		beg = "0"
	}
	if end == "" {
		return nil, fmt.Errorf("host range must specify end value")
	}
	width := 0
	if beg[0] == '0' && len(beg) > 1 {
		width = len(beg)
		if width != len(end) {
			return nil, fmt.Errorf("host range must specify equal-length begin and end formats")
		}
	}
	stepN, err := pyInt(step)
	if err != nil {
		return nil, err
	}
	var seq []string
	iBeg, iEnd := strings.Index(asciiLetters, beg), strings.Index(asciiLetters, end)
	if len(beg) == 1 && len(end) == 1 && iBeg >= 0 && iEnd >= 0 {
		if iBeg > iEnd {
			return nil, fmt.Errorf("host range must have begin <= end")
		}
		if stepN == 0 {
			return nil, fmt.Errorf("slice step cannot be zero")
		}
		letters := asciiLetters[iBeg : iEnd+1]
		if stepN > 0 {
			for i := 0; i < len(letters); i += int(stepN) {
				seq = append(seq, letters[i:i+1])
			}
		}
	} else {
		b, err := pyInt(beg)
		if err != nil {
			return nil, err
		}
		e, err := pyInt(end)
		if err != nil {
			return nil, err
		}
		if stepN == 0 {
			return nil, fmt.Errorf("range() arg 3 must not be zero")
		}
		for i := b; (stepN > 0 && i < e+1) || (stepN < 0 && i > e+1); i += stepN {
			s := strconv.FormatInt(i, 10)
			if width > 0 {
				// str.zfill keeps the sign in front.
				neg := strings.HasPrefix(s, "-")
				digits := strings.TrimPrefix(s, "-")
				for len(digits)+boolInt(neg) < width {
					digits = "0" + digits
				}
				if neg {
					digits = "-" + digits
				}
				s = digits
			}
			seq = append(seq, s)
		}
	}
	var out []string
	for _, r := range seq {
		name := head + r + tail
		if strings.Contains(name, "[") {
			more, err := expandHostnameRange(name)
			if err != nil {
				return nil, err
			}
			out = append(out, more...)
		} else {
			out = append(out, name)
		}
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pyInt is Python's int(str): surrounding whitespace, a sign and
// underscores between digits allowed.
func pyInt(s string) (int64, error) {
	t := strings.TrimSpace(s)
	body := strings.TrimLeft(t, "+-")
	if len(t)-len(body) > 1 || body == "" || strings.HasPrefix(body, "_") || strings.HasSuffix(body, "_") || strings.Contains(body, "__") {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyQuote(s))
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(t, "_", ""), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyQuote(s))
	}
	return n, nil
}

// pyQuote is Python's repr() of a string.
func pyQuote(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		q = "\""
	}
	var b strings.Builder
	b.WriteString(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == q:
			b.WriteString(`\` + q)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(q)
	return b.String()
}

// Address patterns from ansible-core's parsing/utils/addresses.py.
var (
	bracketedHostPort = regexp.MustCompile(`^\[(.+)\]:([0-9]+)$`)
	hostPort          = regexp.MustCompile(`^((?:[^:\[\]]|\[[^\]]*\])*):([0-9]+)$`)
	ipv4Address       = regexp.MustCompile(`(?i)^(?:` + ipv4Component + `\.){3}` + ipv4Component + `$`)
	ipv6Address       = regexp.MustCompile(`(?i)^(` +
		`(?:C:){7}C|(?:C:){1,6}:|(?:C:)(?::C){1,6}|(?:C:){2}(?::C){1,5}|(?:C:){3}(?::C){1,4}|` +
		`(?:C:){4}(?::C){1,3}|(?:C:){5}(?::C){1,2}|(?:C:){6}(?::C)|:(?::C){1,6}|C?::|` +
		`(?:0:){6}(?:C\.){3}C|::(?:ffff:)?(?:C\.){3}C|(?:0:){5}ffff:(?:C\.){3}C)$`)
)

const (
	numericRange  = `\[(?:[0-9]+:[0-9]+)(?::[0-9]+)?\]`
	hexRange      = `\[(?:[0-9a-f]+:[0-9a-f]+)(?::[0-9]+)?\]`
	ipv4Component = `(?:[01]?[0-9]{1,2}|2[0-4][0-9]|25[0-5]|` + numericRange + `)`
	ipv6Component = `(?:[0-9a-f]{1,4}|` + hexRange + `)`
)

func init() {
	ipv6Address = regexp.MustCompile(strings.ReplaceAll(ipv6Address.String(), "C", ipv6Component))
}

// alnumRange matches an alphanumeric [x:y(:z)] range at the start of s.
var alnumRange = regexp.MustCompile(`(?i)^\[(?:[a-z]:[a-z]|[0-9]+:[0-9]+)(?::[0-9]+)?\]`)

// isHostname is the addresses.py 'hostname' pattern: dot-separated labels
// of word characters, '-' and ranges, each starting with a word character
// or range and not ending with '-' or '_'.
func isHostname(s string) bool {
	for _, label := range splitLabels(s) {
		if label == "" {
			return false
		}
		first := true
		var last rune
		for i := 0; i < len(label); {
			if m := alnumRange.FindString(label[i:]); m != "" {
				i += len(m)
				last = ']'
				first = false
				continue
			}
			r := []rune(label[i:])[0]
			word := r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
			if !word && (first || r != '-') {
				return false
			}
			i += len(string(r))
			last = r
			first = false
		}
		if last == '-' || last == '_' {
			return false
		}
	}
	return true
}

// splitLabels splits on the dots outside [ranges].
func splitLabels(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// ParseAddress is ansible-core's parse_address: the host and port of
// "host", "host:port" or "[host]:port" (port -1 when absent).
func ParseAddress(address string, allowRanges bool) (string, int, error) {
	return parseAddress(address, allowRanges)
}

// parseAddress is ansible-core's parse_address: the host and port of
// "host", "host:port" or "[host]:port" (port -1 when absent).
func parseAddress(address string, allowRanges bool) (string, int, error) {
	port := -1
	if m := bracketedHostPort.FindStringSubmatch(address); m != nil {
		address = m[1]
		port, _ = strconv.Atoi(m[2])
	}
	if m := hostPort.FindStringSubmatch(address); m != nil {
		address = m[1]
		port, _ = strconv.Atoi(m[2])
	}
	if !ipv4Address.MatchString(address) && !ipv6Address.MatchString(address) && !isHostname(address) {
		return "", -1, fmt.Errorf("Not a valid network hostname: %s", address)
	}
	if !allowRanges && strings.Contains(address, "[") {
		return "", -1, fmt.Errorf("Detected range in host but was asked to ignore ranges")
	}
	return address, port, nil
}

// expandHostPattern is BaseInventoryPlugin._expand_hostpattern: the host
// names a pattern expands to and its port (-1 when none).
func expandHostPattern(pattern string) ([]string, int, error) {
	host, port, err := parseAddress(pattern, true)
	if err != nil {
		host, port = pattern, -1
	}
	if strings.Contains(host, "[") {
		names, err := expandHostnameRange(host)
		return names, port, err
	}
	return []string{host}, port, nil
}
