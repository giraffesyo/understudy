package modules

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(groupModule, "group", "ansible.builtin.group")
}

var groupSpec = args.Spec{
	"name":       {Required: true},
	"state":      {Default: "present", Choices: []string{"absent", "present"}},
	"force":      {Type: "bool", Default: false},
	"gid":        {Type: "int"},
	"system":     {Type: "bool", Default: false},
	"local":      {Type: "bool", Default: false},
	"non_unique": {Type: "bool", Default: false},
	"gid_min":    {Type: "int"},
	"gid_max":    {Type: "int"},
}

const groupFile = "/etc/group"

// groupModule ports ansible.builtin.group (the Linux Group class).
func groupModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := groupSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if p.Bool("non_unique") && !p.Has("gid") {
		return agentproto.Fail("non_unique is True but all of the following are missing: gid")
	}
	name := p.Str("name")
	local := p.Bool("local")
	if p.Bool("force") && local {
		return agentproto.Fail("force is not a valid option for local, force=True and local=True are mutually exclusive")
	}
	if local {
		if p.Has("gid_min") {
			return agentproto.Fail("'gid_min' can not be used with 'local'")
		}
		if p.Has("gid_max") {
			return agentproto.Fail("'gid_max' can not be used with 'local'")
		}
	}
	cmdName := func(base string) string {
		if local {
			return "l" + base
		}
		return base
	}
	exists := func() (bool, *agentproto.Result) {
		if !local {
			return groupID(env, name) != "", nil
		}
		data, err := os.ReadFile(groupFile)
		if err != nil {
			return false, agentproto.Fail("'local: true' specified but unable to find local group file %s to parse.", groupFile)
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte(name+":")) {
				return true, nil
			}
		}
		return false, nil
	}
	// _local_check_gid_exists: another group already owning the gid.
	checkGID := func() *agentproto.Result {
		if !local || !p.Has("gid") || p.Int("gid") == 0 {
			return nil
		}
		out, _ := runOut(env, "getent", "group")
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, ":")
			if len(f) >= 3 && f[2] == strconv.FormatInt(p.Int("gid"), 10) && f[0] != name {
				return agentproto.Fail("GID '%d' already exists with group '%s'", p.Int("gid"), f[0])
			}
		}
		return nil
	}

	var argv []string // nil: nothing to run (rc None)
	found, fail := exists()
	if fail != nil {
		return fail
	}
	if p.Str("state") == "absent" {
		if found {
			if env.CheckMode {
				return &agentproto.Result{Changed: true}
			}
			argv = []string{cmdName("groupdel")}
			if p.Bool("force") {
				argv = append(argv, "-f")
			}
			argv = append(argv, name)
		}
	} else if !found {
		if env.CheckMode {
			return &agentproto.Result{Changed: true}
		}
		if f := checkGID(); f != nil {
			return f
		}
		argv = []string{cmdName("groupadd")}
		if p.Has("gid") {
			argv = append(argv, "-g", strconv.FormatInt(p.Int("gid"), 10))
			if p.Bool("non_unique") {
				argv = append(argv, "-o")
			}
		}
		if p.Bool("system") {
			argv = append(argv, "-r")
		}
		if p.Has("gid_min") {
			argv = append(argv, "-K", "GID_MIN="+strconv.FormatInt(p.Int("gid_min"), 10))
		}
		if p.Has("gid_max") {
			argv = append(argv, "-K", "GID_MAX="+strconv.FormatInt(p.Int("gid_max"), 10))
		}
		argv = append(argv, name)
	} else {
		if f := checkGID(); f != nil {
			return f
		}
		if p.Has("gid") && strconv.FormatInt(p.Int("gid"), 10) != groupID(env, name) {
			argv = []string{cmdName("groupmod"), "-g", strconv.FormatInt(p.Int("gid"), 10)}
			if p.Bool("non_unique") {
				argv = append(argv, "-o")
			}
			if env.CheckMode {
				argv = []string{} // rc 0 without running
			} else {
				argv = append(argv, name)
			}
		}
	}

	res := &agentproto.Result{Extra: map[string]any{"name": name, "state": p.Str("state")}}
	if argv != nil {
		res.Changed = true
		if len(argv) > 0 {
			bin, err := getBinPath(argv[0])
			if err != nil {
				return agentproto.Fail("%v", err)
			}
			argv[0] = bin
			rc, out, errOut := runCommand(env, argv, cmdOpts{})
			if rc != 0 {
				r := agentproto.Fail("%s", errOut)
				r.Extra = map[string]any{"name": name}
				return r
			}
			if out != "" {
				res.Extra["stdout"] = out
			}
			if errOut != "" {
				res.Extra["stderr"] = errOut
			}
		}
	}
	if ok, _ := exists(); ok {
		if gid := groupID(env, name); gid != "" {
			res.Extra["system"] = p.Bool("system")
			res.Extra["gid"] = atoiOr(strings.TrimSpace(gid))
		}
	}
	return res
}
