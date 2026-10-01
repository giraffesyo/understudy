package yaml

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error is a YAML load failure. Marked errors (scanner, parser, composer
// and constructor errors) carry libyaml's context and problem text and the
// problem mark; ansible-core reports them from that, rewriting some common
// mistakes into friendlier messages (see Message).
type Error struct {
	File    string
	Line    int // problem mark, 1-based; 0 for an unmarked error
	Col     int
	Context string
	Problem string
	// Msg is the text of an unmarked error (a Python exception raised while
	// constructing a value, e.g. int() of a bad !!int).
	Msg string
	// Ansible marks errors raised by ansible-core's own constructor (they skip
	// the friendly-message analysis).
	Ansible bool

	src     []byte
	analyze analysis // Message, Origin and HelpText, computed once
}

type analysis struct {
	done     bool
	msg      string
	col      int
	helpText string
}

func (e *Error) Error() string {
	file, line, col := e.Origin()
	if line == 0 {
		return fmt.Sprintf("%s: %s", file, e.Message())
	}
	return fmt.Sprintf("%s:%d:%d: %s", file, line, col, e.Message())
}

// PyYAMLString is str() of the PyYAML exception (MarkedYAMLError) for a
// stream named name ("<unicode string>" for a str): the context, the
// problem and its mark, one per line; an unmarked error is its message.
func (e *Error) PyYAMLString(name string) string {
	if e.Line == 0 {
		return e.Msg
	}
	var lines []string
	if e.Context != "" {
		lines = append(lines, e.Context)
	}
	if e.Problem != "" {
		lines = append(lines, e.Problem)
	}
	lines = append(lines, fmt.Sprintf("  in \"%s\", line %d, column %d", name, e.Line, e.Col))
	return strings.Join(lines, "\n")
}

// Message is ansible-core's wording for the failure.
func (e *Error) Message() string {
	e.analyzeOnce()
	return "YAML parsing failed: " + e.analyze.msg
}

// UnanalyzedMessage is Message without the friendly-message analysis,
// which ansible-core skips when it cannot read the source (YAML text from
// the command line); the error then points at e.Line and e.Col.
func (e *Error) UnanalyzedMessage() string {
	c := *e
	c.Ansible, c.analyze = true, analysis{}
	return c.Message()
}

// Origin is where the error points (line 0: the file as a whole).
func (e *Error) Origin() (file string, line, col int) {
	e.analyzeOnce()
	return e.File, e.Line, e.analyze.col
}

// HasOrigin reports whether the error names its file even without a line
// (ansible-core shows "Origin: <path>" for unmarked YAML errors).
func (e *Error) HasOrigin() bool { return e.File != "" }

// HelpText is the extra advice ansible-core prints after the source excerpt.
func (e *Error) HelpText() string {
	e.analyzeOnce()
	return e.analyze.helpText
}

// targetLine is line n (1-based) as Python's text-mode readline sees it,
// and whether the file has that many lines.
func targetLine(src []byte, n int) (string, bool) {
	line := 1
	start := 0
	for i := 0; i <= len(src); i++ {
		if i == len(src) || src[i] == '\n' || src[i] == '\r' {
			if line == n {
				if i == len(src) && i == start {
					return "", false // past the last line
				}
				return string(src[start:i]), true
			}
			if i == len(src) {
				break
			}
			if src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			line++
			start = i + 1
		}
	}
	return "", false
}

// Python's \s and \w on str patterns are Unicode-aware; RE2's are ASCII.
const (
	pySpace = `[\s\v\x{1c}-\x{1f}\x{85}\p{Z}]`
	pyWord  = `[\p{L}\p{N}_]`
)

var (
	templateRE = regexp.MustCompile(`^` + pySpace + `*(?:-` + pySpace + `+)*(?:(?:` + pyWord + `|` + pySpace + `)+:` + pySpace + `+)?(\{\{.*\}\})`)
	valueRE    = regexp.MustCompile(`^` + pySpace + `*(?:-` + pySpace + `+)*(?:(?:` + pyWord + `|` + pySpace + `|[\[\]{}])+:` + pySpace + `+)?(.*)$`)
	quotedRE   = regexp.MustCompile(`^` + pySpace + `*('[^']*'|"[^"]*")` + pySpace + `*$`)
	colonRE    = regexp.MustCompile(`:($| )`)
	quoteRE    = regexp.MustCompile(`^` + pySpace + `*(?:-` + pySpace + `+)*(?:(?:` + pyWord + `|` + pySpace + `)+:` + pySpace + `+)?(["'].*?` + pySpace + `*)$`)
)

// runeCol converts a byte offset in s to a 1-based character column.
func runeCol(s string, byteOff int) int { return utf8.RuneCountInString(s[:byteOff]) + 1 }

const colonHelp = `
For example:

    raw: echo 'name: ansible'

Should be:

    raw: "echo 'name: ansible'"
`

const templateHelp = `
For example:

    raw: {{ some_var }}

Should be:

    raw: "{{ some_var }}"
`

const quoteHelp = `
For example:

    raw: "foo" in bar

Should be:

    raw: '"foo" in bar'
`

const quoteTwiceHelp = `
For example:

    raw: "foo" in "bar"

Should be:

    raw: '"foo" in "bar"'
`

// analyzeOnce ports AnsibleYAMLParserError.handle_exception: the message is
// libyaml's context, problem and note, unless the offending line looks like
// a common mistake (a tab, an unquoted template, an unquoted colon, a
// half-quoted value), which gets its own message, column and help text.
func (e *Error) analyzeOnce() {
	a := &e.analyze
	if a.done {
		return
	}
	a.done = true
	a.col = e.Col
	if e.Line == 0 {
		a.msg = strings.Join(strings.Fields(e.Msg), " ")
		return
	}
	line, _ := targetLine(e.src, e.Line)
	if !e.Ansible {
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			a.col, a.msg = runeCol(line, i), "Tabs are usually invalid in YAML."
			return
		}
		if m := templateRE.FindStringSubmatchIndex(line); m != nil {
			a.col, a.msg, a.helpText = runeCol(line, m[2]), "This may be an issue with missing quotes around a template block.", templateHelp
			return
		}
		if !strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), ":") {
			if m := valueRE.FindStringSubmatchIndex(line); m != nil {
				value := line[m[2]:m[3]]
				fragment := value
				if q := quotedRE.FindStringIndex(value); q != nil {
					fragment = strings.Repeat(".", utf8.RuneCountInString(value))
				}
				if c := colonRE.FindStringIndex(fragment); fragment != "" && c != nil {
					a.col = runeCol(line, m[2]) + utf8.RuneCountInString(fragment[:c[0]])
					a.msg, a.helpText = "Colons in unquoted values must be followed by a non-space character.", colonHelp
					return
				}
			}
		}
		if m := quoteRE.FindStringSubmatchIndex(line); m != nil {
			value := line[m[2]:m[3]]
			first, last := value[0], value[len(value)-1]
			if first != last {
				a.col, a.msg, a.helpText = runeCol(line, m[2]), "Values starting with a quote must end with the same quote.", quoteHelp
				return
			}
			if strings.Count(line, string(first)) > 2 {
				a.col, a.msg, a.helpText = runeCol(line, m[2]),
					"Values starting with a quote must end with the same quote, and not contain that quote.", quoteTwiceHelp
				return
			}
		}
	}
	var parts []string
	for _, p := range []string{e.Context, e.Problem} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	msg := strings.TrimSpace(strings.Join(parts, " "))
	if msg != "" {
		r, size := utf8.DecodeRuneInString(msg)
		msg = string(unicode.ToUpper(r)) + msg[size:]
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	a.msg = strings.Join(strings.Fields(msg), " ")
}
