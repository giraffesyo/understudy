package modules

import (
	"fmt"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

func init() {
	Register(lineinfileModule, "lineinfile", "ansible.builtin.lineinfile")
}

var lineinfileSpec = args.Spec{
	"path":          {Required: true, Aliases: []string{"dest", "destfile", "name"}},
	"state":         {Default: "present", Choices: []string{"absent", "present"}},
	"regexp":        {Aliases: []string{"regex"}},
	"search_string": {},
	"line":          {Aliases: []string{"value"}},
	"encoding":      {Default: "utf-8"},
	"insertafter":   {},
	"insertbefore":  {},
	"backrefs":      {Type: "bool", Default: false},
	"create":        {Type: "bool", Default: false},
	"backup":        {Type: "bool", Default: false},
	"firstmatch":    {Type: "bool", Default: false},
	"validate":      {},
	"mode":          {Type: "any"},
	"owner":         {},
	"group":         {},
	"seuser":        {},
	"serole":        {},
	"setype":        {},
	"selevel":       {},
	"attributes":    {Aliases: []string{"attr"}},
	"unsafe_writes": {Type: "bool", Default: false},
}

// lineinfileRun holds one invocation (the module's parameters).
type lineinfileRun struct {
	env                 *RunEnv
	p                   *args.Parsed
	path                string
	regexp, search      *string
	line                *string
	insertAfter, insBef *string
}

func optStr(p *args.Parsed, k string) *string {
	if !p.Has(k) {
		return nil
	}
	s := p.Str(k)
	return &s
}

// lineinfileModule ports ansible.builtin.lineinfile: results carry msg,
// backup and a [content, file attributes] diff list, like Ansible's.
func lineinfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if err := lineinfileSpec.MutuallyExclusive(rawArgs,
		[]string{"insertbefore", "insertafter"}, []string{"regexp", "search_string"},
		[]string{"backrefs", "search_string"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := lineinfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if enc := strings.ToLower(p.Str("encoding")); enc != "utf-8" && enc != "utf8" {
		return agentproto.Fail("lineinfile: encoding %q is not supported (utf-8 only)", p.Str("encoding"))
	}
	l := &lineinfileRun{env: env, p: p, path: pyExpandPath(p.Str("path")),
		regexp: optStr(p, "regexp"), search: optStr(p, "search_string"), line: optStr(p, "line"),
		insertAfter: optStr(p, "insertafter"), insBef: optStr(p, "insertbefore")}

	var warnings []any
	if (l.regexp != nil && *l.regexp == "") || (l.search != nil && *l.search == "") {
		name := "search string"
		msg := "The %s is an empty string, which will match every line in the file. " +
			"This may have unintended consequences, such as replacing the last line in the file rather than appending."
		if l.regexp != nil && *l.regexp == "" {
			name = "regular expression"
			msg += " If this is desired, use '^' to match every line in the file and avoid this warning."
		}
		warnings = append(warnings, fmt.Sprintf(msg, name))
	}
	res := l.run()
	if len(warnings) > 0 {
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		res.Extra["warnings"] = warnings
	}
	return res
}

func (l *lineinfileRun) run() *agentproto.Result {
	if isDir(l.path) {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s is a directory !", l.path),
			Extra: map[string]any{"rc": int64(256)}}
	}
	if l.p.Str("state") == "present" {
		if l.p.Bool("backrefs") && l.regexp == nil {
			return agentproto.Fail("regexp is required with backrefs=true")
		}
		if l.line == nil {
			return agentproto.Fail("line is required with state=present")
		}
		ia, ib := l.insertAfter, l.insBef
		if ia == nil && ib == nil {
			eof := "EOF"
			ia = &eof
		}
		return l.present(ia, ib)
	}
	if l.regexp == nil && l.search == nil && l.line == nil {
		return agentproto.Fail("one of line, search_string, or regexp is required with state=absent")
	}
	return l.absent()
}

// readLines is open(path).readlines() in text mode: universal newlines
// ("\r\n" and "\r" become "\n"), line endings kept.
func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.ReplaceAll(pyToText(data), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var lines []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:i+1])
		s = s[i+1:]
	}
	return lines, nil
}

func (l *lineinfileRun) fileAttrs() fileAttrs {
	return loadFileAttrs(l.p, l.path, false)
}

func (l *lineinfileRun) contentDiff() map[string]any {
	return map[string]any{"before": "", "after": "",
		"before_header": l.path + " (content)", "after_header": l.path + " (content)"}
}

func (l *lineinfileRun) present(insertAfter, insertBefore *string) *agentproto.Result {
	env, path := l.env, l.path
	diff := l.contentDiff()
	var lines []string
	if !pathExists(path) {
		if !l.p.Bool("create") {
			return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Destination %s does not exist !", path),
				Extra: map[string]any{"rc": int64(257)}}
		}
		if dir := dirOf(path); dir != "" && !pathExists(dir) && !env.CheckMode {
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return agentproto.Fail("Error creating %s (%s)", dir, pyOSError(err))
			}
		}
	} else {
		var err error
		if lines, err = readLines(path); err != nil {
			return moduleCrash(err)
		}
	}
	if env.DiffMode {
		diff["before"] = strings.Join(lines, "")
	}

	var reM, reIns *pyre.Pattern
	var fail *agentproto.Result
	if l.regexp != nil {
		if reM, fail = pyCompile(*l.regexp); fail != nil {
			return fail
		}
	}
	if insertAfter != nil && *insertAfter != "BOF" && *insertAfter != "EOF" {
		if reIns, fail = pyCompile(*insertAfter); fail != nil {
			return fail
		}
	} else if insertBefore != nil && *insertBefore != "BOF" {
		if reIns, fail = pyCompile(*insertBefore); fail != nil {
			return fail
		}
	}

	line := *l.line
	firstmatch := l.p.Bool("firstmatch")
	index := [2]int{-1, -1}
	var match []int
	var matchLine string
	matched := false
	exactLineMatch := false
	if reM != nil {
		for n, cur := range lines {
			if m := pySearch(reM, cur); m != nil {
				index[0], match, matchLine, matched = n, m, strings.TrimSuffix(cur, "\n"), true
				if firstmatch {
					break
				}
			}
		}
	}
	if l.search != nil {
		for n, cur := range lines {
			if strings.Contains(cur, *l.search) {
				index[0], matched = n, true
				if firstmatch {
					break
				}
			}
		}
	}
	if !matched {
		for n, cur := range lines {
			if line == strings.TrimRight(cur, "\r\n") {
				index[0] = n
				exactLineMatch = true
			} else if reIns != nil && pySearch(reIns, cur) != nil {
				if insertAfter != nil {
					index[1] = n + 1
					if firstmatch {
						break
					}
				}
				if insertBefore != nil {
					index[1] = n
					if firstmatch {
						break
					}
				}
			}
		}
	}

	msg := ""
	changed := false
	const sep = "\n"
	insertAt := func(i int, s string) {
		lines = append(lines[:i], append([]string{s}, lines[i:]...)...)
	}
	isAfter := insertAfter != nil && *insertAfter != ""
	isBefore := insertBefore != nil && *insertBefore != ""
	switch {
	case index[0] != -1:
		newLine := line
		if l.p.Bool("backrefs") && match != nil {
			expanded, err := pyExpand(reM, line, matchLine, match)
			if err != nil {
				return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + err.Error()}
			}
			newLine = expanded
		}
		if !strings.HasSuffix(newLine, sep) {
			newLine += sep
		}
		if l.regexp == nil && l.search == nil && !matched && !exactLineMatch {
			if isAfter && *insertAfter != "EOF" {
				if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") && !strings.HasSuffix(lines[len(lines)-1], "\r") {
					lines[len(lines)-1] += sep
				}
				if len(lines) == index[1] {
					if strings.TrimRight(lines[index[1]-1], "\r\n") != line {
						lines = append(lines, line+sep)
						msg, changed = "line added", true
					}
				} else if strings.TrimRight(lines[index[1]], "\r\n") != line {
					insertAt(index[1], line+sep)
					msg, changed = "line added", true
				}
			} else if isBefore && *insertBefore != "BOF" {
				if index[1] <= 0 {
					if strings.TrimRight(lines[pyIndex(index[1], len(lines))], "\r\n") != line {
						insertAt(pyIndex(index[1], len(lines)), line+sep)
						msg, changed = "line added", true
					}
				} else if strings.TrimRight(lines[index[1]-1], "\r\n") != line {
					insertAt(index[1], line+sep)
					msg, changed = "line added", true
				}
			}
		} else if lines[index[0]] != newLine {
			lines[index[0]] = newLine
			msg, changed = "line replaced", true
		}
	case l.p.Bool("backrefs"):
		// Nothing: without a match the backrefs cannot be populated.
	case (insertBefore != nil && *insertBefore == "BOF") || (insertAfter != nil && *insertAfter == "BOF"):
		insertAt(0, line+sep)
		msg, changed = "line added", true
	case (insertAfter != nil && *insertAfter == "EOF") || index[1] == -1:
		if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") && !strings.HasSuffix(lines[len(lines)-1], "\r") {
			lines = append(lines, sep)
		}
		lines = append(lines, line+sep)
		msg, changed = "line added", true
	case isAfter && index[1] != -1:
		if len(lines) == index[1] {
			if strings.TrimRight(lines[index[1]-1], "\r\n") != line {
				lines = append(lines, line+sep)
				msg, changed = "line added", true
			}
		} else if line != strings.TrimRight(lines[index[1]], "\n\r") {
			insertAt(index[1], line+sep)
			msg, changed = "line added", true
		}
	default:
		insertAt(index[1], line+sep)
		msg, changed = "line added", true
	}

	if env.DiffMode {
		diff["after"] = strings.Join(lines, "")
	}
	backupDest := ""
	if changed && !env.CheckMode {
		if l.p.Bool("backup") && pathExists(path) {
			b, err := fsutil.Backup(path)
			if err != nil {
				return moduleCrash(err)
			}
			backupDest = b
		}
		if fail := writeChanges(env, []byte(strings.Join(lines, "")), pyRealpath(path), l.p.Str("validate"), l.p.Bool("unsafe_writes")); fail != nil {
			return fail
		}
	}
	if env.CheckMode && !pathExists(path) {
		res := &agentproto.Result{Changed: changed, Msg: msg, Diff: diff,
			Extra: map[string]any{"backup": backupDest}}
		if msg == "" {
			setMsgEmpty(res)
		}
		return res
	}
	return l.finish(changed, msg, backupDest, diff, nil)
}

// pyIndex resolves a Python index (negative counts from the end).
func pyIndex(i, n int) int {
	if i < 0 {
		return i + n
	}
	return i
}

func dirOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	if i == 0 {
		return "/"
	}
	return p[:i]
}

// finish runs check_file_attrs and assembles the exit_json result.
func (l *lineinfileRun) finish(changed bool, msg, backupDest string, diff map[string]any, found *int) *agentproto.Result {
	attr := &fileDiff{}
	msg, changed, fail := checkFileAttrs(l.env, l.fileAttrs(), changed, msg, attr)
	if fail != nil {
		return fail
	}
	res := &agentproto.Result{Changed: changed, Msg: msg,
		Diff:  []any{diff, attrDiffValue(attr, l.path)},
		Extra: map[string]any{"backup": backupDest}}
	if found != nil {
		res.Extra["found"] = int64(*found)
	}
	if msg == "" {
		setMsgEmpty(res)
	}
	return res
}

func (l *lineinfileRun) absent() *agentproto.Result {
	env, path := l.env, l.path
	if !pathExists(path) {
		return &agentproto.Result{Msg: "file not present"}
	}
	diff := l.contentDiff()
	lines, err := readLines(path)
	if err != nil {
		return moduleCrash(err)
	}
	if env.DiffMode {
		diff["before"] = strings.Join(lines, "")
	}
	var re *pyre.Pattern
	if l.regexp != nil {
		var fail *agentproto.Result
		if re, fail = pyCompile(*l.regexp); fail != nil {
			return fail
		}
	}
	found := 0
	var kept []string
	for _, cur := range lines {
		var hit bool
		switch {
		case l.regexp != nil:
			hit = pySearch(re, cur) != nil
		case l.search != nil:
			hit = strings.Contains(cur, *l.search)
		default:
			hit = *l.line == strings.TrimRight(cur, "\r\n")
		}
		if hit {
			found++
		} else {
			kept = append(kept, cur)
		}
	}
	changed := found > 0
	if env.DiffMode {
		diff["after"] = strings.Join(kept, "")
	}
	backupDest := ""
	if changed && !env.CheckMode {
		if l.p.Bool("backup") {
			b, err := fsutil.Backup(path)
			if err != nil {
				return moduleCrash(err)
			}
			backupDest = b
		}
		if fail := writeChanges(env, []byte(strings.Join(kept, "")), pyRealpath(path), l.p.Str("validate"), l.p.Bool("unsafe_writes")); fail != nil {
			return fail
		}
	}
	msg := ""
	if changed {
		msg = fmt.Sprintf("%d line(s) removed", found)
	}
	return l.finish(changed, msg, backupDest, diff, &found)
}

func splitFileLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n")
}

func joinFileLines(lines []string, trailingNewline bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return []byte(out)
}
