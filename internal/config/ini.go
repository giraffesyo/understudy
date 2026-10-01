package config

import (
	"fmt"
	"regexp"
	"strings"
)

// iniFile is an ansible.cfg as Python's configparser.ConfigParser
// (inline_comment_prefixes=(';',)) reads it: sections of lowercased
// option names, the DEFAULT section's options inherited by every section,
// multi-line values joined with newlines.
type iniFile struct {
	defaults map[string]string
	sections map[string]map[string]string
}

// get is ConfigParser.get(section, key, raw=True): the section's value,
// else DEFAULT's; ok is false for a missing section or option.
func (f *iniFile) get(section, key string) (string, bool) {
	if f == nil {
		return "", false
	}
	key = strings.ToLower(key)
	sec, ok := f.sections[section]
	if !ok {
		return "", false
	}
	if v, ok := sec[key]; ok {
		return v, true
	}
	v, ok := f.defaults[key]
	return v, ok
}

var (
	// configparser's SECTCRE and OPTCRE (delimiters "=" and ":").
	iniSectRe = regexp.MustCompile(`^\[(.+)\]`)
	iniOptRe  = regexp.MustCompile(`^(.*?)\s*(=|:)\s*(.*)$`)
	// _CommentSpec: "#" or ";" starting a line, ";" after whitespace.
	iniCommentRe = regexp.MustCompile(`^#.*|^;.*|(^|\s);.*`)
)

// parseINI is ConfigParser.read_string. Its errors are configparser's:
// a missing section header, a duplicate section or option (raised where
// met), and the lines that are neither (collected, raised at the end).
func parseINI(text string) (*iniFile, error) {
	f := &iniFile{defaults: map[string]string{}, sections: map[string]map[string]string{}}
	raw := map[string]map[string][]string{} // section -> option -> lines
	var order []string
	defaults := map[string][]string{}
	var cursect map[string][]string
	sectname, optname := "", ""
	hasOpt := false
	indentLevel := 0
	added := map[string]bool{}
	var errLines []string
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lineno := i + 1
		trimmed := strings.TrimSpace(line)
		clean := strings.TrimRightFunc(iniCommentRe.ReplaceAllString(trimmed, ""), isPySpace)
		if clean == "" {
			// empty_lines_in_values: an empty line (not a comment) joins
			// the value being read.
			if trimmed == clean && cursect != nil && hasOpt && cursect[optname] != nil {
				cursect[optname] = append(cursect[optname], "")
			}
			continue
		}
		curIndent := strings.IndexFunc(line, func(r rune) bool { return !isPySpace(r) })
		if curIndent < 0 {
			curIndent = 0
		}
		if cursect != nil && hasOpt && curIndent > indentLevel {
			cursect[optname] = append(cursect[optname], clean)
			continue
		}
		indentLevel = curIndent
		if m := iniSectRe.FindStringSubmatch(clean); m != nil {
			sectname = m[1]
			switch {
			case raw[sectname] != nil:
				if added[sectname] {
					return nil, fmt.Errorf("While reading from '<string>' [line %2d]: section %s already exists", lineno, pyRepr(sectname))
				}
				cursect = raw[sectname]
				added[sectname] = true
			case sectname == "DEFAULT":
				cursect = defaults
			default:
				cursect = map[string][]string{}
				raw[sectname] = cursect
				order = append(order, sectname)
				added[sectname] = true
			}
			hasOpt, optname = false, ""
			continue
		}
		if cursect == nil {
			return nil, fmt.Errorf("File contains no section headers.\nfile: '<string>', line: %d\n%s", lineno, pyRepr(line))
		}
		m := iniOptRe.FindStringSubmatch(clean)
		if m == nil {
			errLines = append(errLines, fmt.Sprintf("\n\t[line %2d]: %s", lineno, pyRepr(line)))
			continue
		}
		if m[1] == "" {
			errLines = append(errLines, fmt.Sprintf("\n\t[line %2d]: %s", lineno, pyRepr(line)))
		}
		optname = strings.ToLower(strings.TrimRightFunc(m[1], isPySpace))
		hasOpt = true
		key := sectname + "\x00" + optname
		if added[key] {
			return nil, fmt.Errorf("While reading from '<string>' [line %2d]: option %s in section %s already exists",
				lineno, pyRepr(optname), pyRepr(sectname))
		}
		added[key] = true
		cursect[optname] = []string{strings.TrimSpace(m[3])}
	}
	if len(errLines) > 0 {
		return nil, fmt.Errorf("Source contains parsing errors: '<string>'%s", strings.Join(errLines, ""))
	}
	join := func(opts map[string][]string) map[string]string {
		out := make(map[string]string, len(opts))
		for k, v := range opts {
			out[k] = strings.TrimRightFunc(strings.Join(v, "\n"), isPySpace)
		}
		return out
	}
	f.defaults = join(defaults)
	for _, name := range order {
		f.sections[name] = join(raw[name])
	}
	return f, nil
}

// isPySpace is str.isspace for one character.
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85:
		return true
	}
	return r > 0x7f && strings.ContainsRune("                 　", r)
}

// pyRepr is Python's repr() of a str.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r < 0xa0 || r == 0xad:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
