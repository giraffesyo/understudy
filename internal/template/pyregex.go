package template

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// The regex filters and tests run Python's re (the pyre port): its syntax
// (lookaround, backreferences, named groups, conditionals, possessive
// quantifiers, atomic groups), its matching, its replacement templates
// and its re.error messages.

// pyRegexCompile is re.compile(pattern, flags) with the filters' ignorecase
// and multiline switches; a pattern that is not a str fails as re.compile
// does.
func pyRegexCompile(pattern any, ignorecase, multiline bool) (*pyre.Pattern, error) {
	p, ok := asString(Undeprecate(pattern))
	if !ok {
		return nil, &pyTypeError{"first argument must be string or compiled pattern"}
	}
	var flags pyre.Flag
	if ignorecase {
		flags |= pyre.IGNORECASE
	}
	if multiline {
		flags |= pyre.MULTILINE
	}
	return pyre.Compile(p, flags)
}

// regexSubject is to_text(value, nonstring='simplerepr'): a str as it is,
// anything else as its str().
func regexSubject(in any) string {
	if s, ok := asString(in); ok {
		return s
	}
	return toStr(in)
}

// registerRegexFilters installs regex_replace/search/findall/escape.
func registerRegexFilters(e *Engine) {
	f := e.Filters

	// regex_replace(value='', pattern='', replacement='', ignorecase=False,
	// multiline=False, count=0, mandatory_count=0): re.subn.
	f["regex_replace"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s := regexSubject(in)
		var pattern any = ""
		if v, ok := filterArg(args, 0, kwargs, "pattern"); ok {
			pattern = v
		}
		repl := ""
		if v, ok := filterArg(args, 1, kwargs, "replacement"); ok {
			r, isStr := asString(Undeprecate(v))
			if !isStr {
				return nil, &pyTypeError{fmt.Sprintf("expected str instance, %s found", pyClassName(v, false))}
			}
			repl = r
		}
		ic, _ := filterArg(args, 2, kwargs, "ignorecase")
		ml, _ := filterArg(args, 3, kwargs, "multiline")
		re, err := pyRegexCompile(pattern, truthy(ic), truthy(ml))
		if err != nil {
			return nil, err
		}
		count := int64(0)
		if c, ok := filterArg(args, 4, kwargs, "count"); ok {
			n, isInt := asInt(Undeprecate(c))
			if !isInt {
				return nil, &pyTypeError{fmt.Sprintf("'%s' object cannot be interpreted as an integer", pyClassName(c, false))}
			}
			count = n
		}
		var out string
		var subs int
		if count < 0 {
			// re.subn with a negative count replaces nothing (the
			// template is still parsed).
			if _, _, err := re.Sub(repl, "", 1); err != nil {
				return nil, err
			}
			out = s
		} else if out, subs, err = re.Sub(repl, s, int(count)); err != nil {
			return nil, err
		}
		if mc, ok := filterArg(args, 5, kwargs, "mandatory_count"); ok && truthy(mc) {
			n, _ := asInt(Undeprecate(mc))
			if n != int64(subs) {
				return nil, fmt.Errorf("'%s' should match %d times, but matches %d times in '%s'", re.Pattern(), n, count, s)
			}
		}
		return out, nil
	}

	// regex_search(value, regex, *args, **kwargs): re.search, the whole
	// match or the groups named by \g<name> and \N arguments.
	f["regex_search"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s := regexSubject(in)
		if len(args) < 1 {
			return nil, fmt.Errorf("regex_search requires a pattern")
		}
		// The group arguments are read first: \g<name> or \N.
		var groups []any
		for _, g := range args[1:] {
			gs, isStr := asString(Undeprecate(g))
			if !isStr {
				return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyClassName(g, false))
			}
			switch {
			case strings.HasPrefix(gs, `\g`):
				m := groupNameArg.Match(gs, 0, -1)
				if m == nil {
					return nil, fmt.Errorf("'NoneType' object has no attribute 'group'")
				}
				groups = append(groups, gs[m[2]:m[3]])
			case strings.HasPrefix(gs, `\`):
				m := groupNumArg.Match(gs, 0, -1)
				if m == nil {
					return nil, fmt.Errorf("'NoneType' object has no attribute 'group'")
				}
				n, err := pyIntLiteral(gs[m[2]:m[3]])
				if err != nil {
					return nil, err
				}
				groups = append(groups, n)
			default:
				return nil, fmt.Errorf("Unknown argument")
			}
		}
		re, err := pyRegexCompile(args[0], truthy(kwargs["ignorecase"]), truthy(kwargs["multiline"]))
		if err != nil {
			return nil, err
		}
		m := re.Search(s, 0, -1)
		if m == nil {
			return nil, nil // None on no match
		}
		if len(groups) == 0 {
			return s[m[0]:m[1]], nil
		}
		out := make([]any, 0, len(groups))
		for _, g := range groups {
			idx := -1
			switch t := g.(type) {
			case string:
				idx = re.SubexpIndex(t)
			case int64:
				if t >= 0 && t <= int64(re.Groups()) {
					idx = int(t)
				}
			}
			if idx < 0 {
				return nil, errors.New("no such group") // IndexError
			}
			if m[2*idx] < 0 {
				out = append(out, nil)
				continue
			}
			out = append(out, s[m[2*idx]:m[2*idx+1]])
		}
		return out, nil
	}

	// regex_findall(value, regex, multiline=False, ignorecase=False):
	// re.findall.
	f["regex_findall"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s := regexSubject(in)
		pattern, ok := filterArg(args, 0, kwargs, "regex")
		if !ok {
			return nil, fmt.Errorf("regex_findall requires a pattern")
		}
		ml, _ := filterArg(args, 1, kwargs, "multiline")
		ic, _ := filterArg(args, 2, kwargs, "ignorecase")
		re, err := pyRegexCompile(pattern, truthy(ic), truthy(ml))
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, item := range re.FindAll(s) {
			if t, isTuple := item.([]string); isTuple {
				groups := make([]any, len(t))
				for i, g := range t {
					groups[i] = g
				}
				item = groups
			}
			out = append(out, item)
		}
		return out, nil
	}

	f["regex_escape"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s := regexSubject(in)
		reType := "python"
		if v, ok := filterArg(args, 0, kwargs, "re_type"); ok {
			reType = toStr(v)
		}
		switch reType {
		case "python":
			return pyre.Escape(s), nil
		case "posix_basic":
			// re.sub(r'([].[^$*\\])', r'\\\1', string)
			out, err := posixBasicSpecial.ReplaceAllString(s, `\\\1`)
			return out, err
		case "posix_extended":
			return nil, fmt.Errorf("Regex type (%s) not yet implemented", reType)
		}
		return nil, fmt.Errorf("Invalid regex type (%s)", reType)
	}
}

var (
	groupNameArg      = pyre.MustCompile(`\\g<(\S+)>`, 0)
	groupNumArg       = pyre.MustCompile(`\\(\d+)`, 0)
	posixBasicSpecial = pyre.MustCompile(`([].[^$*\\])`, 0)
)

// pyIntLiteral is int(s) of a decimal digit string (\d matches every
// Unicode decimal digit, which int() reads too).
func pyIntLiteral(s string) (int64, error) {
	var n int64
	for _, r := range s {
		d := unicodeDecimal(r)
		if d < 0 {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyStrRepr(s))
		}
		n = n*10 + int64(d)
	}
	return n, nil
}

// PyRegexCompile is re.compile(pattern) for other packages (include_vars'
// files_matching / ignore_files patterns, the varnames lookup).
func PyRegexCompile(pattern string) (*pyre.Pattern, error) {
	return pyre.Compile(pattern, 0)
}

// unicodeDecimal is a decimal digit's value (unicodedata.decimal), or -1:
// Unicode's decimal digits come in runs of ten, zero first.
func unicodeDecimal(r rune) int {
	if !unicode.Is(unicode.Nd, r) {
		return -1
	}
	start := r
	for start > 0 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}
