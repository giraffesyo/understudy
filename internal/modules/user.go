package modules

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func osEnviron() []string { return os.Environ() }

func init() {
	Register(userModule, "user", "ansible.builtin.user")
}

var userSpec = args.Spec{
	"name":        {Required: true, Aliases: []string{"user"}},
	"state":       {Default: "present", Choices: []string{"present", "absent"}},
	"uid":         {Type: "int"},
	"group":       {},
	"groups":      {Type: "list"},
	"append":      {Type: "bool", Default: false},
	"shell":       {},
	"home":        {},
	"create_home": {Type: "bool", Default: true, Aliases: []string{"createhome"}},
	"comment":     {},
	"system":      {Type: "bool", Default: false},
	"remove":      {Type: "bool", Default: false},
	"password":    {}, // crypted hash; passed to usermod -p
}

// passwdEntry is the current state parsed from getent passwd.
type passwdEntry struct {
	uid, gid             string
	comment, home, shell string
}

// userModule manages accounts: getent for current state, then
// useradd/usermod/userdel with only the flags that differ.
func userModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := userSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	res := &agentproto.Result{Extra: map[string]any{"name": name}}

	current, exists := getentPasswd(env, name)

	if p.Str("state") == "absent" {
		if !exists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		argv := []string{}
		if p.Bool("remove") {
			argv = append(argv, "-r")
		}
		argv = append(argv, name)
		if out, err := runOut(env, "userdel", argv...); err != nil {
			return agentproto.Fail("userdel %s failed: %v: %s", name, err, tail(out))
		}
		return res
	}

	wantGroups := stringList(p.List("groups"))

	if !exists {
		res.Changed = true
		if env.CheckMode {
			return res
		}
		argv := buildUserArgs(p, wantGroups, true)
		argv = append(argv, name)
		if out, err := runOut(env, "useradd", argv...); err != nil {
			return agentproto.Fail("useradd %s failed: %v: %s", name, err, tail(out))
		}
		if entry, ok := getentPasswd(env, name); ok {
			res.Extra["uid"] = entry.uid
			res.Extra["home"] = entry.home
		}
		return res
	}

	// Existing user: compute the delta.
	var argv []string
	if p.Has("uid") && fmt.Sprintf("%d", p.Int("uid")) != current.uid {
		argv = append(argv, "-u", fmt.Sprintf("%d", p.Int("uid")))
	}
	if p.Str("shell") != "" && p.Str("shell") != current.shell {
		argv = append(argv, "-s", p.Str("shell"))
	}
	if p.Str("home") != "" && p.Str("home") != current.home {
		argv = append(argv, "-d", p.Str("home"))
	}
	if p.Has("comment") && p.Str("comment") != current.comment {
		argv = append(argv, "-c", p.Str("comment"))
	}
	if p.Str("group") != "" {
		if gid := groupID(env, p.Str("group")); gid != "" && gid != current.gid {
			argv = append(argv, "-g", p.Str("group"))
		}
	}
	if len(wantGroups) > 0 {
		currentGroups := supplementaryGroups(env, name)
		if !groupsSatisfied(currentGroups, wantGroups, p.Bool("append")) {
			flag := []string{"-G", strings.Join(wantGroups, ",")}
			if p.Bool("append") {
				flag = append(flag, "-a")
			}
			argv = append(argv, flag...)
		}
	}
	if p.Str("password") != "" {
		// No portable way to read the current hash without shadow access;
		// setting a password always applies (Ansible reads /etc/shadow as
		// root — acceptable divergence for v0.1).
		argv = append(argv, "-p", p.Str("password"))
	}

	if len(argv) == 0 {
		res.Extra["uid"] = current.uid
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	argv = append(argv, name)
	if out, err := runOut(env, "usermod", argv...); err != nil {
		return agentproto.Fail("usermod %s failed: %v: %s", name, err, tail(out))
	}
	return res
}

func buildUserArgs(p *args.Parsed, wantGroups []string, creating bool) []string {
	var argv []string
	if p.Has("uid") {
		argv = append(argv, "-u", fmt.Sprintf("%d", p.Int("uid")))
	}
	if p.Str("group") != "" {
		argv = append(argv, "-g", p.Str("group"))
	}
	if len(wantGroups) > 0 {
		argv = append(argv, "-G", strings.Join(wantGroups, ","))
	}
	if p.Str("shell") != "" {
		argv = append(argv, "-s", p.Str("shell"))
	}
	if p.Str("home") != "" {
		argv = append(argv, "-d", p.Str("home"))
	}
	if p.Has("comment") {
		argv = append(argv, "-c", p.Str("comment"))
	}
	if p.Str("password") != "" {
		argv = append(argv, "-p", p.Str("password"))
	}
	if creating {
		if p.Bool("system") {
			argv = append(argv, "-r")
		}
		if p.Bool("create_home") {
			argv = append(argv, "-m")
		} else {
			argv = append(argv, "-M")
		}
	}
	return argv
}

func getentPasswd(env *RunEnv, name string) (passwdEntry, bool) {
	out, err := runOut(env, "getent", "passwd", name)
	if err != nil {
		return passwdEntry{}, false
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 7 {
		return passwdEntry{}, false
	}
	return passwdEntry{
		uid: fields[2], gid: fields[3],
		comment: fields[4], home: fields[5], shell: fields[6],
	}, true
}

func groupID(env *RunEnv, group string) string {
	out, err := runOut(env, "getent", "group", group)
	if err != nil {
		return ""
	}
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

// supplementaryGroups returns the user's group names via id -Gn.
func supplementaryGroups(env *RunEnv, name string) []string {
	out, err := runOut(env, "id", "-Gn", name)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// groupsSatisfied compares current vs wanted groups as sets. Without
// append, they must match exactly (minus the primary group nuance —
// compared loosely: wanted ⊆ current and, if exact, current ⊆ wanted+1).
func groupsSatisfied(current, want []string, appendMode bool) bool {
	curSet := map[string]bool{}
	for _, g := range current {
		curSet[g] = true
	}
	for _, g := range want {
		if !curSet[g] {
			return false
		}
	}
	if appendMode {
		return true
	}
	// Exact mode: no extra supplementary groups beyond wanted + primary.
	wantSet := map[string]bool{}
	for _, g := range want {
		wantSet[g] = true
	}
	extras := 0
	for _, g := range current {
		if !wantSet[g] {
			extras++
		}
	}
	return extras <= 1 // the primary group shows up in id -Gn
}

func stringList(items []any) []string {
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
