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
	Register(blockinfileModule, "blockinfile", "ansible.builtin.blockinfile")
}

var blockinfileSpec = args.Spec{
	"path":         {Required: true, Aliases: []string{"dest", "name", "destfile"}},
	"block":        {Aliases: []string{"content"}},
	"marker":       {Default: "# {mark} ANSIBLE MANAGED BLOCK"},
	"marker_begin": {Default: "BEGIN"},
	"marker_end":   {Default: "END"},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"insertafter":  {},
	"insertbefore": {},
	"create":       {Type: "bool", Default: false},
	"backup":       {Type: "bool", Default: false},
	"mode":         {Type: "any"},
	"owner":        {},
	"group":        {},
}

// blockinfileModule maintains a marker-delimited block of text.
func blockinfileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := blockinfileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")
	state := p.Str("state")
	block := p.Str("block")
	if state == "present" && block == "" {
		state = "absent" // Ansible: empty block means remove
	}

	markerTmpl := p.Str("marker")
	beginMarker := strings.ReplaceAll(markerTmpl, "{mark}", p.Str("marker_begin"))
	endMarker := strings.ReplaceAll(markerTmpl, "{mark}", p.Str("marker_end"))

	original, err := os.ReadFile(path)
	exists := err == nil
	if !exists {
		if state == "absent" {
			return &agentproto.Result{Msg: "file not present"}
		}
		if !p.Bool("create") {
			return agentproto.Fail("file %s does not exist (use create=true)", path)
		}
	}

	lines := splitFileLines(original)
	begin, end := -1, -1
	for i, l := range lines {
		if l == beginMarker && begin < 0 {
			begin = i
		}
		if l == endMarker {
			end = i
		}
	}

	// Remove any existing block.
	var without []string
	if begin >= 0 && end >= begin {
		without = append(without, lines[:begin]...)
		without = append(without, lines[end+1:]...)
	} else {
		without = lines
	}

	var newLines []string
	if state == "absent" {
		newLines = without
	} else {
		blockLines := append([]string{beginMarker},
			append(strings.Split(strings.TrimRight(block, "\n"), "\n"), endMarker)...)
		insertAt := len(without) // EOF default; reuse the original position when replacing
		if begin >= 0 {
			insertAt = begin
		} else if ib := p.Str("insertbefore"); ib != "" {
			if ib == "BOF" {
				insertAt = 0
			} else if re, err := regexp.Compile(ib); err == nil {
				for i, l := range without {
					if re.MatchString(l) {
						insertAt = i
					}
				}
			} else {
				return agentproto.Fail("insertbefore: %v", err)
			}
		} else if ia := p.Str("insertafter"); ia != "" && ia != "EOF" {
			if re, err := regexp.Compile(ia); err == nil {
				for i, l := range without {
					if re.MatchString(l) {
						insertAt = i + 1
					}
				}
			} else {
				return agentproto.Fail("insertafter: %v", err)
			}
		}
		newLines = make([]string, 0, len(without)+len(blockLines))
		newLines = append(newLines, without[:insertAt]...)
		newLines = append(newLines, blockLines...)
		newLines = append(newLines, without[insertAt:]...)
	}

	newContent := joinFileLines(newLines, true)
	changed := !bytes.Equal(original, newContent) || !exists
	res := &agentproto.Result{Changed: changed}
	if !changed {
		return res
	}
	if env.DiffMode {
		res.Diff = []agentproto.Diff{{
			BeforeHeader: path, AfterHeader: path,
			Before: string(original), After: string(newContent),
		}}
	}
	if env.CheckMode {
		return res
	}
	if p.Bool("backup") && exists {
		backupPath := path + ".understudy-backup"
		os.WriteFile(backupPath, original, 0o600)
		res.Extra = map[string]any{"backup_file": backupPath}
	}
	mode := os.FileMode(0o644)
	if exists {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	}
	if p.Has("mode") {
		if m, err := fsutil.ParseMode(p.Any("mode")); err == nil {
			mode = m
		} else {
			return agentproto.Fail("%v", err)
		}
	}
	if err := fsutil.AtomicWrite(path, bytes.NewReader(newContent), mode); err != nil {
		return agentproto.Fail("writing %s: %v", path, err)
	}
	if p.Str("owner") != "" || p.Str("group") != "" {
		if _, err := fsutil.ApplyFileAttrs(path, nil, p.Str("owner"), p.Str("group"), true); err != nil {
			return agentproto.Fail("%v", err)
		}
	}
	return res
}
