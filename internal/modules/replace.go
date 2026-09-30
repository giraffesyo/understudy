package modules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

func init() {
	Register(replaceModule, "replace", "ansible.builtin.replace")
}

var replaceSpec = args.Spec{
	"path":          {Required: true, Aliases: []string{"dest", "destfile", "name"}},
	"regexp":        {Required: true},
	"replace":       {Default: ""},
	"after":         {},
	"before":        {},
	"backup":        {Type: "bool", Default: false},
	"validate":      {},
	"encoding":      {Default: "utf-8"},
	"unsafe_writes": {Type: "bool", Default: false},
	"mode":          {Type: "any"},
	"owner":         {},
	"group":         {},
	"seuser":        {},
	"serole":        {},
	"setype":        {},
	"selevel":       {},
	"attributes":    {Aliases: []string{"attr"}},
}

// replaceModule is ansible.builtin.replace: re.subn of a MULTILINE pattern
// over the file (or the section between after/before), with Python
// replacement syntax.
func replaceModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := replaceSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if enc := strings.ToLower(p.Str("encoding")); enc != "utf-8" && enc != "utf8" {
		return agentproto.Fail("replace: encoding %q is not supported (utf-8 only)", p.Str("encoding"))
	}
	path := pyExpandPath(p.Str("path"))
	if isDir(path) {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s is a directory !", path),
			Extra: map[string]any{"rc": int64(256)}}
	}
	if !pathExists(path) {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s does not exist !", path),
			Extra: map[string]any{"rc": int64(257)}}
	}
	lines, err := readLines(path) // text mode: universal newlines
	if err != nil {
		return moduleCrash(err)
	}
	contents := strings.Join(lines, "")

	// after/before narrow the substitution to a DOTALL subsection.
	section, lo, hi := contents, 0, len(contents)
	pattern := ""
	switch after, before := p.Str("after"), p.Str("before"); {
	case after != "" && before != "":
		pattern = after + "(?P<subsection>.*?)" + before
	case after != "":
		pattern = after + "(?P<subsection>.*)"
	case before != "":
		pattern = "(?P<subsection>.*)" + before
	}
	if pattern != "" {
		sre, fail := pyCompile(pattern)
		if fail != nil {
			return fail
		}
		sre, err = compilePyPattern("(?s)" + pattern)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		m := sre.FindStringSubmatchIndex(contents)
		if m == nil {
			return &agentproto.Result{Msg: "Pattern for before/after params did not match the given file: " + pattern,
				Extra: map[string]any{"rc": int64(0)}}
		}
		i := sre.SubexpIndex("subsection")
		lo, hi = m[2*i], m[2*i+1]
		section = contents[lo:hi]
	}

	if _, fail := pyCompile(p.Str("regexp")); fail != nil {
		return fail
	}
	re, err := compilePyPattern("(?m)" + p.Str("regexp"))
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	replaced, count, err := pySubn(re, p.Str("replace"), section)
	if err != nil {
		if te, ok := err.(*pyTplError); ok && te.indexError {
			return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + te.msg}
		}
		return agentproto.Fail("Unable to process replace due to error: %v", err)
	}

	res := &agentproto.Result{Extra: map[string]any{"rc": int64(0)}}
	msg := ""
	changed := count > 0 && replaced != section
	newContents := contents
	if changed {
		newContents = contents[:lo] + replaced + contents[hi:]
		msg = fmt.Sprintf("%d replacements made", count)
		if env.DiffMode {
			res.Diff = map[string]any{"before_header": path, "before": contents,
				"after_header": path, "after": newContents}
		}
	}
	if changed && !env.CheckMode {
		if p.Bool("backup") && pathExists(path) {
			b, err := fsutil.Backup(path)
			if err != nil {
				return moduleCrash(err)
			}
			res.Extra["backup_file"] = b
		}
		if fail := writeChanges(env, []byte(newContents), pyRealpath(path), p.Str("validate"), p.Bool("unsafe_writes")); fail != nil {
			return fail
		}
	}
	msg, changed, fail := checkFileAttrs(env, loadFileAttrs(p, path, false), changed, msg, nil)
	if fail != nil {
		return fail
	}
	res.Changed = changed
	res.Msg = msg
	if msg == "" {
		setMsgEmpty(res)
	}
	return res
}

// setMsgEmpty records an explicit empty msg (Ansible's no-op replace).
func setMsgEmpty(res *agentproto.Result) {
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	res.Extra["msg"] = ""
}

// pyTplError is an error from Python's replacement-template parser:
// re.error, or IndexError for an unknown group name.
type pyTplError struct {
	msg        string
	indexError bool
}

func (e *pyTplError) Error() string { return e.msg }

// pyTplPart is a literal (group < 0) or a group reference.
type pyTplPart struct {
	lit   string
	group int
}

func isPyIdentifier(s string) bool {
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

// parsePyTemplate is sre_parse.parse_template: \1..\99, \g<n>, \g<name>,
// octal and character escapes, with Python's errors and positions.
func parsePyTemplate(re *regexp.Regexp, repl string) ([]pyTplPart, error) {
	var parts []pyTplPart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, pyTplPart{lit: lit.String(), group: -1})
			lit.Reset()
		}
	}
	errAt := func(msg string, pos int) error {
		return &pyTplError{msg: fmt.Sprintf("%s at position %d", msg, pos)}
	}
	groups := re.NumSubexp()
	addGroup := func(idx, pos int) error {
		if idx > groups {
			return errAt(fmt.Sprintf("invalid group reference %d", idx), pos)
		}
		flush()
		parts = append(parts, pyTplPart{group: idx})
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
			} else if !isPyIdentifier(name) {
				return nil, errAt(fmt.Sprintf("bad character in group name %s", pyStrRepr(name)), i-len(name)-1)
			} else {
				idx = re.SubexpIndex(name)
				if idx < 0 {
					return nil, &pyTplError{msg: fmt.Sprintf("unknown group name %s", pyStrRepr(name)), indexError: true}
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

// expandPyTemplate renders a parsed template for one match (unmatched
// groups expand to "").
func expandPyTemplate(parts []pyTplPart, s string, m []int) string {
	var b strings.Builder
	for _, p := range parts {
		if p.group < 0 {
			b.WriteString(p.lit)
			continue
		}
		if 2*p.group+1 < len(m) && m[2*p.group] >= 0 {
			b.WriteString(s[m[2*p.group]:m[2*p.group+1]])
		}
	}
	return b.String()
}

// pySubn is Python's re.subn(pattern, repl, s): every non-overlapping match
// replaced, repl interpreted with Python's escape and group syntax (parsed
// up front, so a bad template fails even without matches).
func pySubn(re *regexp.Regexp, repl, s string) (string, int, error) {
	parts, err := parsePyTemplate(re, repl)
	if err != nil {
		return "", 0, err
	}
	matches := pyre.FindAllSubmatchIndex(re, s, -1)
	if len(matches) == 0 {
		return s, 0, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		b.WriteString(expandPyTemplate(parts, s, m))
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), len(matches), nil
}

// pyExpand is match.expand(template) for one match.
func pyExpand(re *regexp.Regexp, repl, s string, m []int) (string, error) {
	parts, err := parsePyTemplate(re, repl)
	if err != nil {
		return "", err
	}
	return expandPyTemplate(parts, s, m), nil
}
