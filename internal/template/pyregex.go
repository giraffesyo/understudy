package template

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// Python regex compatibility layer. Go's regexp is RE2; Python's re is not.
// Translatable constructs are translated; untranslatable ones (lookaround,
// backreferences in patterns) are rejected loudly — failing clearly beats
// silently matching differently.

// pyRegexCompile compiles a Python-syntax pattern, rejecting
// RE2-unsupported constructs with actionable errors.
func pyRegexCompile(pattern string, ignorecase, multiline bool) (*regexp.Regexp, error) {
	if msg := pyre.SyntaxError(pattern); msg != "" {
		return nil, errors.New(msg) // re.error
	}
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
		count := -1
		if c, ok := kwargs["count"]; ok {
			if n, isInt := asInt(c); isInt && n > 0 {
				count = int(n)
			}
		}
		// re.subn parses the replacement template before matching.
		parts, err := pyre.ParseTemplate(re, repl)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		last := 0
		for _, m := range pyre.FindAllSubmatchIndex(re, s, count) {
			b.WriteString(s[last:m[0]])
			b.WriteString(pyre.ExpandTemplate(parts, s, m))
			last = m[1]
		}
		b.WriteString(s[last:])
		return b.String(), nil
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
		// The group arguments are read first: \g<name> or \N.
		type groupRef struct {
			name string
			num  int
		}
		var groups []groupRef
		for _, g := range args[1:] {
			gs, isStr := asString(g)
			if !isStr {
				return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyClassName(g, false))
			}
			switch {
			case strings.HasPrefix(gs, `\g`):
				m := regexp.MustCompile(`^\\g<(\S+)>`).FindStringSubmatch(gs)
				if m == nil {
					return nil, fmt.Errorf("'NoneType' object has no attribute 'group'")
				}
				groups = append(groups, groupRef{name: m[1], num: -1})
			case strings.HasPrefix(gs, `\`):
				m := regexp.MustCompile(`^\\(\d+)`).FindStringSubmatch(gs)
				if m == nil {
					return nil, fmt.Errorf("'NoneType' object has no attribute 'group'")
				}
				n, _ := strconv.Atoi(m[1])
				groups = append(groups, groupRef{num: n})
			default:
				return nil, fmt.Errorf("Unknown argument")
			}
		}
		re, err := pyRegexCompile(pattern, truthy(kwargs["ignorecase"]), truthy(kwargs["multiline"]))
		if err != nil {
			return nil, err
		}
		m := re.FindStringSubmatchIndex(s)
		if m == nil {
			return nil, nil // Ansible returns None on no match
		}
		if len(groups) == 0 {
			return s[m[0]:m[1]], nil
		}
		out := make([]any, 0, len(groups))
		for _, g := range groups {
			idx := g.num
			if g.num < 0 {
				idx = re.SubexpIndex(g.name)
			}
			if idx < 0 || 2*idx+1 >= len(m) {
				return nil, fmt.Errorf("no such group")
			}
			if m[2*idx] < 0 {
				out = append(out, nil)
				continue
			}
			out = append(out, s[m[2*idx]:m[2*idx+1]])
		}
		return out, nil
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
		out := []any{}
		for _, idx := range pyre.FindAllSubmatchIndex(re, s, -1) {
			m := make([]string, len(idx)/2)
			for i := range m {
				if idx[2*i] >= 0 {
					m[i] = s[idx[2*i]:idx[2*i+1]]
				}
			}
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
			s = toStr(in)
		}
		reType := "python"
		if len(args) > 0 {
			reType = toStr(args[0])
		} else if t, ok := kwargs["re_type"]; ok {
			reType = toStr(t)
		}
		switch reType {
		case "python":
			return pyReEscape(s), nil
		case "posix_basic":
			return regexp.MustCompile(`([].[^$*\\])`).ReplaceAllString(s, `\$1`), nil
		case "posix_extended":
			return nil, fmt.Errorf("Regex type (%s) not yet implemented", reType)
		}
		return nil, fmt.Errorf("Invalid regex type (%s)", reType)
	}
}

// pyReEscape is Python's re.escape: each regex-special character (and
// whitespace, '#', '&', '~', '-') backslash-escaped.
func pyReEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("()[]{}?*+-|^$\\.&~# \t\n\r\v\f", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// PyRegexCompile is pyRegexCompile for other packages (include_vars'
// files_matching / ignore_files patterns).
func PyRegexCompile(pattern string) (*regexp.Regexp, error) {
	return pyRegexCompile(pattern, false, false)
}
