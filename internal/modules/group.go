package modules

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(groupModule, "group", "ansible.builtin.group")
}

var groupSpec = args.Spec{
	"name":   {Required: true},
	"state":  {Default: "present", Choices: []string{"present", "absent"}},
	"gid":    {Type: "int"},
	"system": {Type: "bool", Default: false},
}

func groupModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := groupSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	res := &agentproto.Result{Extra: map[string]any{"name": name, "state": p.Str("state")}}

	currentGID := groupID(env, name)
	exists := currentGID != ""

	if p.Str("state") == "absent" {
		if !exists {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if out, err := runOut(env, "groupdel", name); err != nil {
			return agentproto.Fail("groupdel %s failed: %v: %s", name, err, tail(out))
		}
		return res
	}

	if !exists {
		res.Changed = true
		if env.CheckMode {
			return res
		}
		var argv []string
		if p.Has("gid") {
			argv = append(argv, "-g", fmt.Sprintf("%d", p.Int("gid")))
		}
		if p.Bool("system") {
			argv = append(argv, "-r")
		}
		argv = append(argv, name)
		if out, err := runOut(env, "groupadd", argv...); err != nil {
			return agentproto.Fail("groupadd %s failed: %v: %s", name, err, tail(out))
		}
	} else if p.Has("gid") && fmt.Sprintf("%d", p.Int("gid")) != strings.TrimSpace(currentGID) {
		res.Changed = true
		if env.CheckMode {
			return res
		}
		if out, err := runOut(env, "groupmod", "-g", fmt.Sprintf("%d", p.Int("gid")), name); err != nil {
			return agentproto.Fail("groupmod %s failed: %v: %s", name, err, tail(out))
		}
	}
	if gid := groupID(env, name); gid != "" {
		res.Extra["gid"] = atoiOr(strings.TrimSpace(gid))
	} else if p.Has("gid") {
		res.Extra["gid"] = p.Int("gid")
	}
	res.Extra["system"] = p.Bool("system")
	return res
}
