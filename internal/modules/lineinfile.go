package modules

import (
	"bytes"
	"os"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(lineinfileModule, "lineinfile", "ansible.builtin.lineinfile")
}

var lineinfileSpec = args.Spec{
	"path":         {Required: true, Aliases: []string{"dest", "name", "destfile"}},
	"line":         {Aliases: []string{"value"}},
	"regexp":       {Aliases: []string{"regex"}},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"insertafter":  {},
	"insertbefore": {},
	"backrefs":     {Type: "bool", Default: false},
	"create":       {Type: "bool", Default: false},
	"backup":       {Type: "bool", Default: false},
	"firstmatch":   {Type: "bool", Default: false},
	"mode":         {Type: "any"},
	"owner":        {},
	"group":        {},
}

// lineinfileModule ensures a single line is present (or absent) in a file.
// Changed only when the resulting bytes differ.
func lineinfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := lineinfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")
	state := p.Str("state")
	line := p.Str("line")
	pattern := p.Str("regexp")
	backrefs := p.Bool("backrefs")

	if state == "present" && !p.Has("line") {
		return agentproto.Fail("line is required with state=present")
	}
	if backrefs && pattern == "" {
		return agentproto.Fail("backrefs=true requires regexp")
	}
	if p.Str("insertafter") != "" && p.Str("insertbefore") != "" {
		return agentproto.Fail("insertafter and insertbefore are mutually exclusive")
	}

	var re *regexp.Regexp
	if pattern != "" {
		re, err = compilePyPattern(pattern)
		if err != nil {
			return agentproto.Fail("regexp: %v", err)
		}
	}

	original, err := os.ReadFile(path)
	exists := err == nil
	if !exists {
		if state == "absent" {
			return &agentproto.Result{Msg: "file not present"}
		}
		if !p.Bool("create") {
			return agentproto.Fail("file %s does not exist (use create=true)", path)
		}
		original = nil
	}

	hadTrailingNewline := len(original) == 0 || bytes.HasSuffix(original, []byte("\n"))
	lines := splitFileLines(original)

	var newLines []string
	var changedMsg string
	if state == "absent" {
		newLines = removeLines(lines, re, line, p.Has("line"))
		changedMsg = "line(s) removed"
	} else {
		newLines, changedMsg, err = ensureLine(lines, re, line, backrefs,
			p.Str("insertafter"), p.Str("insertbefore"), p.Bool("firstmatch"))
		if err != nil {
			return agentproto.Fail("%v", err)
		}
	}

	newContent := joinFileLines(newLines, hadTrailingNewline || len(lines) == 0)
	changed := !bytes.Equal(original, newContent) || !exists

	res := &agentproto.Result{Changed: changed, Msg: changedMsg}
	if !changed {
		res.Msg = ""
	}
	if env.DiffMode && changed {
		res.Diff = []any{textDiff(path, string(original), path, string(newContent))}
	}
	if !changed || env.CheckMode {
		return res
	}

	if p.Bool("backup") && exists {
		backupPath, err := fsutil.Backup(path)
		if err != nil {
			return agentproto.Fail("backup of %s failed: %v", path, err)
		}
		res.Extra = map[string]any{"backup_file": backupPath}
	}
	// Preserve the existing file's mode and owner across the rewrite (new
	// files get 0644); explicit mode/owner/group override afterward.
	if err := fsutil.AtomicRewrite(path, bytes.NewReader(newContent), 0o644); err != nil {
		return agentproto.Fail("writing %s: %v", path, err)
	}
	if p.Has("mode") || p.Str("owner") != "" || p.Str("group") != "" {
		var mode any
		if p.Has("mode") {
			mode = p.Any("mode")
		}
		if _, err := fsutil.ApplyFileAttrs(path, mode, p.Str("owner"), p.Str("group"), true); err != nil {
			return agentproto.Fail("%v", err)
		}
	}
	return res
}

// compilePyPattern rejects RE2-unsupported Python regex constructs with a
// clear error instead of silently mis-matching.
func compilePyPattern(pattern string) (*regexp.Regexp, error) {
	for _, bad := range []struct{ needle, what string }{
		{"(?=", "lookahead"}, {"(?!", "lookahead"},
		{"(?<=", "lookbehind"}, {"(?<!", "lookbehind"},
	} {
		if strings.Contains(pattern, bad.needle) {
			return nil, regexpUnsupportedError(bad.what)
		}
	}
	return regexp.Compile(pattern)
}

type regexpUnsupportedError string

func (e regexpUnsupportedError) Error() string {
	return "pattern uses " + string(e) + ", which is not supported by this regex engine"
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

func removeLines(lines []string, re *regexp.Regexp, line string, haveLine bool) []string {
	var out []string
	for _, l := range lines {
		switch {
		case re != nil && re.MatchString(l):
			continue
		case re == nil && haveLine && l == line:
			continue
		}
		out = append(out, l)
	}
	return out
}

// ensureLine implements state=present: replace the (last) regexp match, or
// insert relative to insertafter/insertbefore, or append at EOF.
func ensureLine(lines []string, re *regexp.Regexp, line string, backrefs bool, insertAfter, insertBefore string, firstmatch bool) ([]string, string, error) {
	// Find the match to replace: last match wins unless firstmatch.
	matchIdx := -1
	if re != nil {
		for i, l := range lines {
			if re.MatchString(l) {
				matchIdx = i
				if firstmatch {
					break
				}
			}
		}
	} else {
		for i, l := range lines {
			if l == line {
				matchIdx = i
				if firstmatch {
					break
				}
			}
		}
	}

	if matchIdx >= 0 {
		replacement := line
		if backrefs {
			// Expand \1-style backrefs from the matched line.
			m := re.FindStringSubmatchIndex(lines[matchIdx])
			replacement = string(re.ExpandString(nil, pyReplToGo(line), lines[matchIdx], m))
		}
		out := append([]string{}, lines...)
		out[matchIdx] = replacement
		return out, "line replaced", nil
	}
	if backrefs {
		// No match with backrefs: leave the file alone (Ansible semantics).
		return lines, "", nil
	}

	// Insertion point.
	insertAt := len(lines) // EOF default
	switch {
	case insertBefore == "BOF":
		insertAt = 0
	case insertBefore != "":
		bre, err := compilePyPattern(insertBefore)
		if err != nil {
			return nil, "", err
		}
		for i, l := range lines {
			if bre.MatchString(l) {
				insertAt = i
				if firstmatch {
					break
				}
			}
		}
	case insertAfter != "" && insertAfter != "EOF":
		are, err := compilePyPattern(insertAfter)
		if err != nil {
			return nil, "", err
		}
		for i, l := range lines {
			if are.MatchString(l) {
				insertAt = i + 1
				if firstmatch {
					break
				}
			}
		}
	}

	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, line)
	out = append(out, lines[insertAt:]...)
	return out, "line added", nil
}

// pyReplToGo converts Python replacement backrefs (\1, \g<name>) to Go's
// ${1}/${name} for ExpandString.
func pyReplToGo(repl string) string {
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c == '$' {
			b.WriteString("$$")
			continue
		}
		if c == '\\' && i+1 < len(repl) {
			next := repl[i+1]
			switch {
			case next >= '0' && next <= '9':
				b.WriteString("${")
				b.WriteByte(next)
				i++
				for i+1 < len(repl) && repl[i+1] >= '0' && repl[i+1] <= '9' {
					i++
					b.WriteByte(repl[i])
				}
				b.WriteString("}")
				continue
			case next == 'g' && i+2 < len(repl) && repl[i+2] == '<':
				if end := strings.IndexByte(repl[i+3:], '>'); end >= 0 {
					b.WriteString("${" + repl[i+3:i+3+end] + "}")
					i += 3 + end
					continue
				}
			case next == '\\':
				b.WriteByte('\\')
				i++
				continue
			}
			b.WriteByte(next)
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
