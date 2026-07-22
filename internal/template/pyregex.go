package template

import (
	"fmt"
	"regexp"
	"strings"
)

// Python regex compatibility layer. Go's regexp is RE2; Python's re is not.
// Translatable constructs are translated; untranslatable ones (lookaround,
// backreferences in patterns) are rejected loudly — failing clearly beats
// silently matching differently.

// pyRegexCompile compiles a Python-syntax pattern, rejecting
// RE2-unsupported constructs with actionable errors.
func pyRegexCompile(pattern string, ignorecase, multiline bool) (*regexp.Regexp, error) {
	if err := checkUnsupported(pattern); err != nil {
		return nil, err
	}
	flags := ""
	if ignorecase {
		flags += "i"
	}
	if multiline {
		flags += "m"
	}
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression %q: %v", pattern, err)
	}
	return re, nil
}

func checkUnsupported(pattern string) error {
	// Scan outside character classes for (?= (?! (?<= (?<! and \1..\9 and (?P=name).
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\' && i+1 < len(pattern):
			next := pattern[i+1]
			if next >= '1' && next <= '9' && !inClass {
				return fmt.Errorf("pattern uses backreference \\%c, which is not supported by this regex engine", next)
			}
			i++ // skip escaped char
		case c == '[' && !inClass:
			inClass = true
		case c == ']' && inClass:
			inClass = false
		case c == '(' && !inClass && i+2 < len(pattern) && pattern[i+1] == '?':
			rest := pattern[i+2:]
			switch {
			case strings.HasPrefix(rest, "="), strings.HasPrefix(rest, "!"):
				return fmt.Errorf("pattern uses lookahead ('(?=' or '(?!'), which is not supported by this regex engine")
			case strings.HasPrefix(rest, "<=") || strings.HasPrefix(rest, "<!"):
				return fmt.Errorf("pattern uses lookbehind, which is not supported by this regex engine")
			case strings.HasPrefix(rest, "P="):
				return fmt.Errorf("pattern uses a named backreference '(?P=...)', which is not supported by this regex engine")
			}
		}
	}
	return nil
}

// pyReplTranslate converts Python replacement syntax to Go's:
// \1 -> ${1}, \g<name> -> ${name}, literal $ -> $$.
func pyReplTranslate(repl string) string {
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		switch {
		case c == '$':
			b.WriteString("$$")
		case c == '\\' && i+1 < len(repl):
			next := repl[i+1]
			switch {
			case next >= '0' && next <= '9':
				b.WriteString("${")
				b.WriteByte(next)
				i++
				// Multi-digit group references: \10 etc.
				for i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9' {
					i++
					b.WriteByte(repl[i])
				}
				b.WriteString("}")
			case next == 'g' && i+2 < len(repl) && repl[i+2] == '<':
				end := strings.IndexByte(repl[i+3:], '>')
				if end < 0 {
					b.WriteByte(c)
					continue
				}
				b.WriteString("${")
				b.WriteString(repl[i+3 : i+3+end])
				b.WriteString("}")
				i += 3 + end
			case next == '\\':
				b.WriteByte('\\')
				i++
			case next == 'n':
				b.WriteByte('\n')
				i++
			case next == 't':
				b.WriteByte('\t')
				i++
			default:
				b.WriteByte(next)
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// registerRegexFilters installs regex_replace/search/findall/escape.
func registerRegexFilters(e *Engine) {
	f := e.Filters

	f["regex_replace"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("regex_replace requires a pattern")
		}
		pattern, _ := asString(args[0])
		repl := ""
		if len(args) > 1 {
			repl, _ = asString(args[1])
		}
		re, err := pyRegexCompile(pattern, truthy(kwargs["ignorecase"]), truthy(kwargs["multiline"]))
		if err != nil {
			return nil, err
		}
		return re.ReplaceAllString(s, pyReplTranslate(repl)), nil
	}

	f["regex_search"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("regex_search requires a pattern")
		}
		pattern, _ := asString(args[0])
		re, err := pyRegexCompile(pattern, truthy(kwargs["ignorecase"]), truthy(kwargs["multiline"]))
		if err != nil {
			return nil, err
		}
		m := re.FindStringSubmatch(s)
		if m == nil {
			return nil, nil // Ansible returns None on no match
		}
		// With group references as extra args, return those groups.
		if len(args) > 1 {
			var out []any
			for _, g := range args[1:] {
				gs, _ := asString(g)
				switch {
				case strings.HasPrefix(gs, "\\g<") && strings.HasSuffix(gs, ">"):
					name := gs[3 : len(gs)-1]
					idx := re.SubexpIndex(name)
					if idx < 0 || idx >= len(m) {
						return nil, fmt.Errorf("no capture group named %q", name)
					}
					out = append(out, m[idx])
				case strings.HasPrefix(gs, "\\"):
					var idx int
					if _, err := fmt.Sscanf(gs, "\\%d", &idx); err != nil || idx >= len(m) {
						return nil, fmt.Errorf("invalid group reference %q", gs)
					}
					out = append(out, m[idx])
				default:
					return nil, fmt.Errorf("group references must look like '\\1' or '\\g<name>', got %q", gs)
				}
			}
			return out, nil
		}
		return m[0], nil
	}

	f["regex_findall"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("regex_findall requires a pattern")
		}
		pattern, _ := asString(args[0])
		re, err := pyRegexCompile(pattern, truthy(kwargs["ignorecase"]), truthy(kwargs["multiline"]))
		if err != nil {
			return nil, err
		}
		matches := re.FindAllStringSubmatch(s, -1)
		out := []any{}
		for _, m := range matches {
			switch {
			case len(m) == 1:
				out = append(out, m[0])
			case len(m) == 2:
				// One capture group: Python findall returns just the group.
				out = append(out, m[1])
			default:
				groups := make([]any, len(m)-1)
				for i, g := range m[1:] {
					groups[i] = g
				}
				out = append(out, groups)
			}
		}
		return out, nil
	}

	f["regex_escape"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("regex_escape requires a string")
		}
		return regexp.QuoteMeta(s), nil
	}
}
