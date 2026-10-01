package pyre

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SyntaxError reports the re.error Python's sre_parse raises for
// the common malformed patterns (with its wording and position), or "".
// Patterns it accepts may still be rejected by RE2 later.
func SyntaxError(p string) string {
	type group struct{ pos int }
	var stack []group
	// canRepeat: the previous item can take a quantifier; lastRepeat: it
	// was itself a quantifier.
	canRepeat, lastRepeat, modified := false, false, false
	i := 0
	for i < len(p) {
		c := p[i]
		switch c {
		case '\\':
			if i+1 >= len(p) {
				return fmt.Sprintf("bad escape (end of pattern) at position %d", i)
			}
			n := p[i+1]
			if (n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z') && !strings.ContainsRune("abfnrtvxuUNdDsSwWAZB", rune(n)) {
				return fmt.Sprintf("bad escape \\%c at position %d", n, i)
			}
			i += 2
			// \b \B \A \Z are anchors: nothing to repeat.
			canRepeat, lastRepeat = !strings.ContainsRune("bBAZ", rune(n)), false
		case '[':
			start := i
			i++
			if i < len(p) && p[i] == '^' {
				i++
			}
			first := true
			for {
				if i >= len(p) {
					return fmt.Sprintf("unterminated character set at position %d", start)
				}
				if p[i] == ']' && !first {
					i++
					break
				}
				if p[i] == '\\' {
					if i+1 >= len(p) {
						return fmt.Sprintf("bad escape (end of pattern) at position %d", i)
					}
					i += 2
				} else {
					// A literal range lo-hi must not run backwards.
					if i+2 < len(p) && p[i+1] == '-' && p[i+2] != ']' && p[i+2] != '\\' && p[i+2] < p[i] {
						return fmt.Sprintf("bad character range %c-%c at position %d", p[i], p[i+2], i)
					}
					i++
				}
				first = false
			}
			canRepeat, lastRepeat = true, false
		case '(':
			stack = append(stack, group{i})
			i++
			if i < len(p) && p[i] == '?' {
				i++
				if i < len(p) && p[i] == '#' {
					end := strings.IndexByte(p[i:], ')')
					if end < 0 {
						return fmt.Sprintf("missing ), unterminated comment at position %d", stack[len(stack)-1].pos)
					}
					i += end + 1
					stack = stack[:len(stack)-1]
					continue
				}
				// Skip the extension's header up to where its body starts.
				switch {
				case strings.HasPrefix(p[i:], "P<"):
					end := strings.IndexByte(p[i:], '>')
					if end < 0 {
						return fmt.Sprintf("missing >, unterminated name at position %d", i+2)
					}
					name := p[i+2 : i+end]
					if name == "" {
						return fmt.Sprintf("missing group name at position %d", i+2)
					}
					if !isIdentifier(name) {
						return fmt.Sprintf("bad character in group name %s at position %d", strRepr(name), i+2)
					}
					i += end + 1
				case strings.HasPrefix(p[i:], "P="):
					end := strings.IndexByte(p[i:], ')')
					if end < 0 {
						return fmt.Sprintf("missing ), unterminated name at position %d", i+2)
					}
					i += end + 1
					stack = stack[:len(stack)-1]
					canRepeat, lastRepeat = true, false
					continue
				case strings.HasPrefix(p[i:], "<=") || strings.HasPrefix(p[i:], "<!"):
					i += 2
				case strings.HasPrefix(p[i:], "<"):
					ext := "?<"
					if i+1 < len(p) {
						ext += string(p[i+1])
					}
					return fmt.Sprintf("unknown extension %s at position %d", ext, i-1)
				case i < len(p) && strings.IndexByte("aiLmsux-:=!>P", p[i]) < 0:
					return fmt.Sprintf("unknown extension ?%c at position %d", p[i], i-1)
				case i < len(p) && (p[i] == ':' || p[i] == '=' || p[i] == '!' || p[i] == '>'):
					i++
				default:
					for i < len(p) && strings.IndexByte("aiLmsux-", p[i]) >= 0 {
						i++
					}
					if i < len(p) && p[i] == ')' {
						// (?flags): a global flag group, no body.
						i++
						stack = stack[:len(stack)-1]
						canRepeat, lastRepeat = false, false
						continue
					}
					if i < len(p) && p[i] == ':' {
						i++
					}
				}
			}
			canRepeat, lastRepeat = false, false
		case ')':
			if len(stack) == 0 {
				return fmt.Sprintf("unbalanced parenthesis at position %d", i)
			}
			stack = stack[:len(stack)-1]
			i++
			canRepeat, lastRepeat = true, false
		case '|':
			i++
			canRepeat, lastRepeat = false, false
		case '*', '+', '?', '{':
			if c == '{' {
				end := strings.IndexByte(p[i:], '}')
				if end < 0 || !isRepeatBody(p[i+1:i+end]) {
					i++ // a literal brace
					canRepeat, lastRepeat = true, false
					continue
				}
			}
			if lastRepeat {
				if (c == '?' || c == '+') && !modified {
					// lazy / possessive modifier
					i++
					modified = true
					continue
				}
				return fmt.Sprintf("multiple repeat at position %d", i)
			}
			if !canRepeat {
				return fmt.Sprintf("nothing to repeat at position %d", i)
			}
			if c == '{' {
				end := strings.IndexByte(p[i:], '}')
				if lo, hi, ok := strings.Cut(p[i+1:i+end], ","); ok && lo != "" && hi != "" {
					l, _ := strconv.Atoi(lo)
					h, _ := strconv.Atoi(hi)
					if l > h {
						return fmt.Sprintf("min repeat greater than max repeat at position %d", i+1)
					}
				}
				i += end + 1
			} else {
				i++
			}
			lastRepeat, modified = true, false
		case '^', '$':
			i++
			canRepeat, lastRepeat = false, false // anchors take no quantifier
		default:
			i++
			canRepeat, lastRepeat = true, false
		}
	}
	if len(stack) > 0 {
		return fmt.Sprintf("missing ), unterminated subpattern at position %d", stack[len(stack)-1].pos)
	}
	return ""
}

func isRepeatBody(s string) bool {
	if s == "" {
		return false
	}
	lo, hi, found := strings.Cut(s, ",")
	digits := func(x string) bool {
		for _, r := range x {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if !found {
		return digits(lo)
	}
	return digits(lo) && digits(hi)
}

// pyCompile is re.compile as a module uses it: a Python syntax error

// TemplateError is an error from Python's replacement-template parser:
// re.error, or IndexError for an unknown group name.
type TemplateError struct {
	Msg        string
	IndexError bool
}

func (e *TemplateError) Error() string { return e.Msg }

// TemplatePart is a literal (group < 0) or a group reference.
type TemplatePart struct {
	Lit   string
	Group int
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// ParseTemplate is sre_parse.parse_template: \1..\99, \g<n>, \g<name>,
// octal and character escapes, with Python's errors and positions.
func ParseTemplate(re *regexp.Regexp, repl string) ([]TemplatePart, error) {
	var parts []TemplatePart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, TemplatePart{Lit: lit.String(), Group: -1})
			lit.Reset()
		}
	}
	errAt := func(msg string, pos int) error {
		return &TemplateError{Msg: fmt.Sprintf("%s at position %d", msg, pos)}
	}
	groups := re.NumSubexp()
	addGroup := func(idx, pos int) error {
		if idx > groups {
			return errAt(fmt.Sprintf("invalid group reference %d", idx), pos)
		}
		flush()
		parts = append(parts, TemplatePart{Group: idx})
		return nil
	}
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	isOct := func(c byte) bool { return c >= '0' && c <= '7' }
	i := 0
	for i < len(repl) {
		c := repl[i]
		if c != '\\' {
			lit.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(repl) {
			return nil, errAt("bad escape (end of pattern)", len(repl)-1)
		}
		start := i
		e := repl[i+1]
		i += 2
		switch {
		case e == 'g':
			if i >= len(repl) || repl[i] != '<' {
				return nil, errAt("missing <", i)
			}
			i++
			end := strings.IndexByte(repl[i:], '>')
			if end < 0 {
				return nil, errAt("missing >, unterminated name", i)
			}
			name := repl[i : i+end]
			i += end + 1
			if name == "" {
				return nil, errAt("missing group name", i-1)
			}
			var idx int
			if n, err := strconv.Atoi(name); err == nil && !strings.ContainsAny(name, "+-") {
				idx = n
			} else if !isIdentifier(name) {
				return nil, errAt(fmt.Sprintf("bad character in group name %s", strRepr(name)), i-len(name)-1)
			} else {
				idx = re.SubexpIndex(name)
				if idx < 0 {
					return nil, &TemplateError{Msg: fmt.Sprintf("unknown group name %s", strRepr(name)), IndexError: true}
				}
			}
			if err := addGroup(idx, i-len(name)-1); err != nil {
				return nil, err
			}
		case e == '0':
			j := i
			for j < len(repl) && j < i+2 && isOct(repl[j]) {
				j++
			}
			n, _ := strconv.ParseInt("0"+repl[i:j], 8, 32)
			lit.WriteRune(rune(n & 0xff))
			i = j
		case isDigit(e):
			this := repl[start:i]
			if i < len(repl) && isDigit(repl[i]) {
				this += string(repl[i])
				i++
				if isOct(e) && isOct(this[2]) && i < len(repl) && isOct(repl[i]) {
					this += string(repl[i])
					i++
					n, _ := strconv.ParseInt(this[1:], 8, 32)
					if n > 0o377 {
						return nil, errAt(fmt.Sprintf("octal escape value %s outside of range 0-0o377", this), start)
					}
					lit.WriteRune(rune(n))
					continue
				}
			}
			n, _ := strconv.Atoi(this[1:])
			if err := addGroup(n, start+1); err != nil {
				return nil, err
			}
		default:
			if esc, ok := map[byte]string{'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v", '\\': "\\"}[e]; ok {
				lit.WriteString(esc)
			} else if e >= 'a' && e <= 'z' || e >= 'A' && e <= 'Z' {
				return nil, errAt("bad escape "+repl[start:i], start)
			} else {
				lit.WriteString(repl[start:i])
			}
		}
	}
	flush()
	return parts, nil
}

// ExpandTemplate renders a parsed template for one match (unmatched
// groups expand to "").
func ExpandTemplate(parts []TemplatePart, s string, m []int) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Group < 0 {
			b.WriteString(p.Lit)
			continue
		}
		if 2*p.Group+1 < len(m) && m[2*p.Group] >= 0 {
			b.WriteString(s[m[2*p.Group]:m[2*p.Group+1]])
		}
	}
	return b.String()
}

// strRepr is repr() of a simple Python str.
func strRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
