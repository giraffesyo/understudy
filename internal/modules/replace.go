package modules

import (
	"fmt"
	"regexp"
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
		if te, ok := err.(*pyTplError); ok && te.IndexError {
			return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + te.Msg}
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
