package modules

import (
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports ansible.builtin.getent.

func init() {
	names := []string{"getent", "ansible.builtin.getent"}
	Register(getentModule, names...)
	for _, n := range names {
		specs[n] = getentSpec
	}
}

var getentSpec = args.Spec{
	"database": {Required: true},
	"key":      {},
	"service":  {},
	"split":    {},
	"fail_key": {Type: "bool", Default: true},
}

func getentModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := getentSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	database := p.Str("database")
	bin, err := lookPath("getent")
	if err != nil {
		return agentproto.Fail("Failed to find required executable \"getent\" in paths: %s", strings.Join(append([]string{"/usr/bin", "/bin"}, sbinDirs...), ":"))
	}
	cmd := []string{bin, database}
	if p.Has("key") {
		cmd = append(cmd, p.Str("key"))
	}
	if p.Has("service") {
		cmd = append(cmd, "-s", p.Str("service"))
	}
	split, hasSplit := p.Str("split"), p.Has("split")
	if hasSplit && split == "" {
		return agentproto.Fail("Invalid split value. The value must be a non-empty string")
	}
	if !hasSplit {
		switch database {
		case "passwd", "shadow", "group", "gshadow":
			split, hasSplit = ":", true
		}
	}
	rc, out, _ := runCommand(env, cmd, cmdOpts{})
	tree := map[string]any{}
	dbtree := "getent_" + database
	switch rc {
	case 0:
		seen := map[string]int{}
		for _, line := range pySplitLines(out) {
			var rec []string
			if hasSplit {
				rec = strings.Split(line, split)
			} else {
				rec = strings.Fields(line)
			}
			if len(rec) == 0 {
				continue
			}
			rest := anyList(rec[1:])
			switch seen[rec[0]] {
			case 0:
				tree[rec[0]] = rest
			case 1:
				tree[rec[0]] = []any{tree[rec[0]], rest}
			default:
				tree[rec[0]] = append(tree[rec[0]].([]any), rest)
			}
			seen[rec[0]]++
		}
		return &agentproto.Result{AnsibleFacts: map[string]any{dbtree: tree}}
	case 1:
		return agentproto.Fail("Missing arguments, or database unknown.")
	case 2:
		msg := "One or more supplied key could not be found in the database."
		if !p.Bool("fail_key") {
			tree[p.Str("key")] = nil
			return &agentproto.Result{Msg: msg, AnsibleFacts: map[string]any{dbtree: tree}}
		}
		return agentproto.Fail("%s", msg)
	case 3:
		return agentproto.Fail("Enumeration not supported on this database.")
	}
	return agentproto.Fail("Unexpected failure!")
}

// pySplitLines is str.splitlines() for command output.
func pySplitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
