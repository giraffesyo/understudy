package modules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// pyRegexSyntaxError reports the re.error Python's sre_parse raises for
// the common malformed patterns (with its wording and position), or "".
// Patterns it accepts may still be rejected by RE2 later.
func pyRegexSyntaxError(p string) string {
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
					if !isPyIdentifier(name) {
						return fmt.Sprintf("bad character in group name %s at position %d", pyStrRepr(name), i+2)
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
// escapes as an unhandled exception (module crash); a pattern RE2 cannot
// run fails with a clear message.
func pyCompile(pattern string) (*regexp.Regexp, *agentproto.Result) {
	if msg := pyRegexSyntaxError(pattern); msg != "" {
		return nil, &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + msg}
	}
	re, err := compilePyPattern(pattern)
	if err != nil {
		return nil, agentproto.Fail("%v", err)
	}
	return re, nil
}
