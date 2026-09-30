package mysqlclient

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// OptionFile is a parsed MySQL option file (~/.my.cnf) — Python
// configparser semantics, which both PyMySQL (read_default_file) and the
// modules (config_overrides_defaults) use.
type OptionFile struct {
	sections map[string]map[string]*string
	defaults map[string]*string
}

// ParseOptions selects between PyMySQL's parser (RawConfigParser with
// allow_no_value and '_' -> '-' option names) and the modules' own
// ConfigParser(comment_prefixes=('#', ';', '!')).
type ParseOptions struct {
	AllowNoValue   bool
	DashNames      bool // optionxform: lower().replace('_', '-')
	CommentPrefix  string
	StripQuotesGet bool
}

// PyMySQLParser is pymysql.optionfile.Parser.
var PyMySQLParser = ParseOptions{AllowNoValue: true, DashNames: true, CommentPrefix: "#;", StripQuotesGet: true}

// ModuleParser is the collection's parse_from_mysql_config_file parser.
var ModuleParser = ParseOptions{CommentPrefix: "#;!"}

var (
	sectRe  = regexp.MustCompile(`^\[(.+)\]`)
	optRe   = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)
	optNVRe = regexp.MustCompile(`^(.*?)\s*(?:([=:])\s*(.*))?$`)
)

// ReadOptionFile parses path; a missing file yields an empty result (as
// configparser.read ignores unreadable files).
func ReadOptionFile(path string, po ParseOptions) (*OptionFile, error) {
	of := &OptionFile{sections: map[string]map[string]*string{}, defaults: map[string]*string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return of, nil
		}
		return of, nil
	}
	var (
		cur         map[string]*string
		curName     string
		optName     string
		indentLevel int
		parseErrs   []string
	)
	seenSect := map[string]bool{}
	lines := strings.SplitAfter(string(data), "\n")
	for n, raw := range lines {
		if raw == "" {
			continue
		}
		lineno := n + 1
		line := strings.TrimRight(raw, "\n")
		stripped := strings.TrimSpace(line)
		if stripped != "" && strings.ContainsRune(po.CommentPrefix, rune(stripped[0])) {
			continue
		}
		if stripped == "" {
			// empty_lines_in_values: a blank line inside a multi-line
			// value is kept; it ends nothing here.
			if cur != nil && optName != "" && cur[optName] != nil {
				v := *cur[optName] + "\n"
				cur[optName] = &v
			}
			continue
		}
		curIndent := len(line) - len(strings.TrimLeft(line, " \t\f\v\r"))
		if cur != nil && optName != "" && curIndent > indentLevel {
			if cur[optName] != nil {
				v := *cur[optName] + "\n" + stripped
				cur[optName] = &v
			}
			continue
		}
		indentLevel = curIndent
		if m := sectRe.FindStringSubmatch(stripped); m != nil {
			curName = m[1]
			if seenSect[curName] {
				return nil, fmt.Errorf("While reading from %s [line %2d]: section %s already exists", PyRepr(path), lineno, PyRepr(curName))
			}
			seenSect[curName] = true
			if curName == "DEFAULT" {
				cur = of.defaults
			} else {
				cur = map[string]*string{}
				of.sections[curName] = cur
			}
			optName = ""
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("File contains no section headers.\nfile: %s, line: %d\n%s", PyRepr(path), lineno, PyRepr(raw))
		}
		re := optRe
		if po.AllowNoValue {
			re = optNVRe
		}
		m := re.FindStringSubmatch(stripped)
		if m == nil {
			parseErrs = append(parseErrs, fmt.Sprintf("\t[line %2d]: %s", lineno, PyRepr(raw)))
			continue
		}
		name := strings.ToLower(strings.TrimRight(m[1], " \t"))
		if po.DashNames {
			name = strings.ReplaceAll(name, "_", "-")
		}
		if name == "" {
			parseErrs = append(parseErrs, fmt.Sprintf("\t[line %2d]: %s", lineno, PyRepr(raw)))
			continue
		}
		if _, dup := cur[name]; dup {
			return nil, fmt.Errorf("While reading from %s [line %2d]: option %s in section %s already exists",
				PyRepr(path), lineno, PyRepr(name), PyRepr(curName))
		}
		optName = name
		if m[2] == "" {
			cur[name] = nil
		} else {
			v := strings.TrimSpace(m[3])
			cur[name] = &v
		}
	}
	// Trailing blank lines appended to values are stripped.
	for _, sect := range append([]map[string]*string{of.defaults}, mapsOf(of.sections)...) {
		for k, v := range sect {
			if v != nil {
				t := strings.TrimRight(*v, "\n")
				sect[k] = &t
			}
		}
	}
	if len(parseErrs) > 0 {
		return nil, fmt.Errorf("Source contains parsing errors: %s\n%s", PyRepr(path), strings.Join(parseErrs, "\n"))
	}
	return of, nil
}

func mapsOf(m map[string]map[string]*string) []map[string]*string {
	out := make([]map[string]*string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// HasSection reports whether a section exists.
func (o *OptionFile) HasSection(name string) bool {
	_, ok := o.sections[name]
	return ok
}

// Get returns an option's value (DEFAULT section as fallback). ok is false
// when the section or option is missing or the option has no value.
func (o *OptionFile) Get(section, key string, po ParseOptions) (string, bool) {
	sect, ok := o.sections[section]
	if !ok {
		return "", false
	}
	v, ok := sect[key]
	if !ok {
		v, ok = o.defaults[key]
	}
	if !ok || v == nil {
		return "", false
	}
	s := *v
	if po.StripQuotesGet && len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s, true
}
