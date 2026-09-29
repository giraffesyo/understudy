package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
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
	path := p.Str("path")
	info, err := os.Stat(path)
	if err != nil {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s does not exist !", path),
			Extra: map[string]any{"rc": int64(257)}}
	}
	if info.IsDir() {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s is a directory !", path),
			Extra: map[string]any{"rc": int64(256)}}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agentproto.Fail("Unable to read the contents of '%s': %v", path, err)
	}
	contents := string(data)

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
		sre, err := compilePyPattern("(?s)" + pattern)
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

	re, err := compilePyPattern("(?m)" + p.Str("regexp"))
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	replaced, count, err := pySubn(re, p.Str("replace"), section)
	if err != nil {
		return agentproto.Fail("%v", err)
	}

	res := &agentproto.Result{Extra: map[string]any{}}
	msg := ""
	changed := count > 0 && replaced != section
	newContents := contents
	if changed {
		newContents = contents[:lo] + replaced + contents[hi:]
		msg = fmt.Sprintf("%d replacements made", count)
		if env.DiffMode {
			res.Diff = []agentproto.Diff{{BeforeHeader: path, Before: contents, AfterHeader: path, After: newContents}}
		}
	}
	if changed && !env.CheckMode {
		if v := p.Str("validate"); v != "" {
			if err := fsutil.Validate(v, path, []byte(newContents)); err != nil {
				return validateFailure(err)
			}
		}
		if p.Bool("backup") {
			b, err := fsutil.Backup(path)
			if err != nil {
				return agentproto.Fail("backup of %s failed: %v", path, err)
			}
			res.Extra["backup_file"] = b
		}
		real, err := filepath.EvalSymlinks(path) // always change the real file
		if err != nil {
			real = path
		}
		if err := fsutil.AtomicRewrite(real, strings.NewReader(newContents), 0o644); err != nil {
			return agentproto.Fail("writing %s: %v", path, err)
		}
	}
	if !env.CheckMode && (p.Has("mode") || p.Str("owner") != "" || p.Str("group") != "") {
		var mode any
		if p.Has("mode") {
			mode = p.Any("mode")
		}
		attrChanged, err := fsutil.ApplyFileAttrs(path, mode, p.Str("owner"), p.Str("group"), true)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		if attrChanged {
			if msg != "" {
				msg += " and "
			}
			msg += "ownership, perms or SE linux context changed"
			changed = true
		}
	}
	res.Changed = changed
	res.Msg = msg
	if msg == "" {
		setMsgEmpty(res)
	}
	res.Extra["rc"] = int64(0) // replace always reports rc=0
	return res
}

// setMsgEmpty records an explicit empty msg (Ansible's no-op replace).
func setMsgEmpty(res *agentproto.Result) {
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	res.Extra["msg"] = ""
}

// pySubn is Python's re.subn(pattern, repl, s): every non-overlapping match
// replaced, repl interpreted with Python's escape and group syntax.
func pySubn(re *regexp.Regexp, repl, s string) (string, int, error) {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, 0, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		out, err := pyExpand(re, repl, s, m)
		if err != nil {
			return "", 0, err
		}
		b.WriteString(out)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), len(matches), nil
}

// pyExpand renders one replacement: \1, \g<1>, \g<name>, and the character
// escapes Python's re module processes in templates.
func pyExpand(re *regexp.Regexp, repl, s string, m []int) (string, error) {
	group := func(idx int) (string, error) {
		if idx < 0 || 2*idx+1 >= len(m) {
			return "", fmt.Errorf("invalid group reference %d", idx)
		}
		if m[2*idx] < 0 {
			return "", nil // unmatched group -> empty (Python 3.5+)
		}
		return s[m[2*idx]:m[2*idx+1]], nil
	}
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c != '\\' || i+1 >= len(repl) {
			b.WriteByte(c)
			continue
		}
		i++
		switch n := repl[i]; {
		case n >= '0' && n <= '9':
			j := i + 1
			if j < len(repl) && repl[j] >= '0' && repl[j] <= '9' {
				j++
			}
			idx, _ := strconv.Atoi(repl[i:j])
			g, err := group(idx)
			if err != nil {
				return "", err
			}
			b.WriteString(g)
			i = j - 1
		case n == 'g' && i+1 < len(repl) && repl[i+1] == '<':
			end := strings.IndexByte(repl[i+2:], '>')
			if end < 0 {
				return "", fmt.Errorf("missing > in group reference")
			}
			name := repl[i+2 : i+2+end]
			idx, err := strconv.Atoi(name)
			if err != nil {
				idx = re.SubexpIndex(name)
				if idx < 0 {
					return "", fmt.Errorf("unknown group name %q", name)
				}
			}
			g, err := group(idx)
			if err != nil {
				return "", err
			}
			b.WriteString(g)
			i += 2 + end
		default:
			if esc, ok := map[byte]string{'n': "\n", 't': "\t", 'r': "\r", 'f': "\f", 'v': "\v", 'a': "\a", 'b': "\b", '\\': "\\"}[n]; ok {
				b.WriteString(esc)
			} else if (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
				return "", fmt.Errorf("bad escape \\%c in replacement", n)
			} else {
				b.WriteByte('\\')
				b.WriteByte(n)
			}
		}
	}
	return b.String(), nil
}
