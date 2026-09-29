package modules

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(blockinfileModule, "blockinfile", "ansible.builtin.blockinfile")
}

var blockinfileSpec = args.Spec{
	"path":            {Required: true, Aliases: []string{"dest", "destfile", "name"}},
	"state":           {Default: "present", Choices: []string{"absent", "present"}},
	"marker":          {Default: "# {mark} ANSIBLE MANAGED BLOCK"},
	"block":           {Default: "", Aliases: []string{"content"}},
	"insertafter":     {},
	"insertbefore":    {},
	"create":          {Type: "bool", Default: false},
	"backup":          {Type: "bool", Default: false},
	"validate":        {},
	"marker_begin":    {Default: "BEGIN"},
	"marker_end":      {Default: "END"},
	"append_newline":  {Type: "bool", Default: false},
	"prepend_newline": {Type: "bool", Default: false},
	"encoding":        {Default: "utf-8"},
	"mode":            {Type: "any"},
	"owner":           {},
	"group":           {},
	"seuser":          {},
	"serole":          {},
	"setype":          {},
	"selevel":         {},
	"attributes":      {Aliases: []string{"attr"}},
	"unsafe_writes":   {Type: "bool", Default: false},
}

// blockinfileModule ports ansible.builtin.blockinfile, result shapes and
// messages included.
func blockinfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if err := blockinfileSpec.MutuallyExclusive(rawArgs, []string{"insertbefore", "insertafter"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := blockinfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if enc := strings.ToLower(p.Str("encoding")); enc != "utf-8" && enc != "utf8" {
		return agentproto.Fail("blockinfile: encoding %q is not supported (utf-8 only)", p.Str("encoding"))
	}
	path := pyExpandPath(p.Str("path"))
	if isDir(path) {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s is a directory !", path),
			Extra: map[string]any{"rc": int64(256)}}
	}
	pathExisted := pathExists(path)
	var original *string
	var lines []string
	if !pathExisted {
		if !p.Bool("create") {
			return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Path %s does not exist !", path),
				Extra: map[string]any{"rc": int64(257)}}
		}
		if dir := dirOf(path); dir != "" && !pathExists(dir) && !env.CheckMode {
			if err := os.MkdirAll(dir, 0o777); err != nil {
				return agentproto.Fail("Error creating %s Error: %s", dir, pyOSError(err))
			}
		}
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return moduleCrash(err)
		}
		s := pyToText(data)
		original = &s
		lines = splitLinesKeepNL(s)
	}

	diff := map[string]any{"before": "", "after": "",
		"before_header": path + " (content)", "after_header": path + " (content)"}
	if env.DiffMode && original != nil && *original != "" {
		diff["before"] = *original
	}

	insertBefore, insertAfter := optStr(p, "insertbefore"), optStr(p, "insertafter")
	block := p.Str("block")
	present := p.Str("state") == "present"
	const sep = "\n"

	if !present && !pathExisted {
		return &agentproto.Result{Msg: fmt.Sprintf("File %s not present", path)}
	}
	if insertBefore == nil && insertAfter == nil {
		eof := "EOF"
		insertAfter = &eof
	}
	var insertRe *regexp.Regexp
	insertPattern := ""
	hasPattern := false
	if insertAfter != nil && *insertAfter != "EOF" {
		insertPattern, hasPattern = *insertAfter, true
	} else if insertBefore != nil && *insertBefore != "BOF" {
		insertPattern, hasPattern = *insertBefore, true
	}
	if hasPattern {
		var fail *agentproto.Result
		if insertRe, fail = pyCompile(insertPattern); fail != nil {
			return fail
		}
	}

	marker := p.Str("marker")
	marker0 := strings.ReplaceAll(marker, "{mark}", p.Str("marker_begin")) + sep
	marker1 := strings.ReplaceAll(marker, "{mark}", p.Str("marker_end")) + sep

	var blockLines []string
	if present && block != "" {
		if !strings.HasSuffix(block, sep) {
			block += sep
		}
		blockLines = append([]string{marker0}, splitLinesKeepNL(block)...)
		blockLines = append(blockLines, marker1)
	}

	n0, n1 := -1, -1
	for i, l := range lines {
		if l == marker0 {
			n0 = i
		}
		if l == marker1 {
			n1 = i
		}
	}
	switch {
	case n0 < 0 || n1 < 0:
		n0 = -1
		switch {
		case insertRe != nil:
			if pyPatternMultiline(insertPattern) {
				text := ""
				if original != nil {
					text = *original
				}
				if m := insertRe.FindStringIndex(text); m != nil {
					if insertAfter != nil {
						n0 = strings.Count(text[:m[1]], "\n")
					} else if insertBefore != nil {
						n0 = strings.Count(text[:m[0]], "\n")
					}
				}
			} else {
				for i, l := range lines {
					if pySearch(insertRe, l) != nil {
						n0 = i
					}
				}
			}
			if n0 < 0 {
				n0 = len(lines)
			} else if insertAfter != nil {
				n0++
			}
		case insertBefore != nil:
			n0 = 0 // insertbefore=BOF
		default:
			n0 = len(lines) // insertafter=EOF
		}
	case n0 < n1:
		lines = append(lines[:n0], lines[n1+1:]...)
	default:
		lines = append(lines[:n1], lines[n0+1:]...)
		n0 = n1
	}

	if n0 > 0 && !strings.HasSuffix(lines[n0-1], sep) {
		lines[n0-1] += sep
	}
	if p.Bool("prepend_newline") && present {
		if n0 != 0 && lines[n0-1] != sep {
			lines = append(lines[:n0], append([]string{sep}, lines[n0:]...)...)
			n0++
		}
	}
	lines = append(lines[:n0], append(append([]string{}, blockLines...), lines[n0:]...)...)
	if p.Bool("append_newline") && present {
		after := n0 + len(blockLines)
		if after < len(lines) && lines[after] != sep {
			lines = append(lines[:after], append([]string{sep}, lines[after:]...)...)
		}
	}
	result := strings.Join(lines, "")
	if env.DiffMode {
		diff["after"] = result
	}

	var msg string
	var changed bool
	switch {
	case original != nil && *original == result:
		msg, changed = "", false
	case original == nil:
		msg, changed = "File created", true
	case len(blockLines) == 0:
		msg, changed = "Block removed", true
	default:
		msg, changed = "Block inserted", true
	}

	backupFile := ""
	if changed && !env.CheckMode {
		if p.Bool("backup") && pathExisted {
			b, err := fsutil.Backup(path)
			if err != nil {
				return moduleCrash(err)
			}
			backupFile = b
		}
		if fail := writeChanges(env, []byte(result), pyRealpath(path), p.Str("validate"), p.Bool("unsafe_writes")); fail != nil {
			return fail
		}
	}
	if env.CheckMode && !pathExisted {
		res := &agentproto.Result{Changed: changed, Msg: msg, Diff: diff}
		if msg == "" {
			setMsgEmpty(res)
		}
		return res
	}
	attr := &fileDiff{}
	msg, changed, fail := checkFileAttrs(env, loadFileAttrs(p, path, false), changed, msg, attr)
	if fail != nil {
		return fail
	}
	res := &agentproto.Result{Changed: changed, Msg: msg, Diff: []any{diff, attrDiffValue(attr, path)}}
	if backupFile != "" {
		res.Extra = map[string]any{"backup_file": backupFile}
	}
	if msg == "" {
		setMsgEmpty(res)
	}
	return res
}

// splitLinesKeepNL is str.splitlines(True) restricted to the separators
// that matter for text files ("\n", "\r\n", "\r").
func splitLinesKeepNL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, s[start:i+1])
			start = i + 1
		case '\r':
			end := i + 1
			if end < len(s) && s[end] == '\n' {
				end++
				i++
			}
			out = append(out, s[start:end])
			start = end
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyPatternMultiline reports whether a pattern turns on re.MULTILINE with
// a leading inline flag group.
func pyPatternMultiline(p string) bool {
	if !strings.HasPrefix(p, "(?") {
		return false
	}
	end := strings.IndexAny(p[2:], ":)")
	return end >= 0 && strings.ContainsRune(p[2:2+end], 'm')
}
