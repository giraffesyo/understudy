package modules

import (
	"bytes"
	"os"
	"path/filepath"
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

// groupModule ports ansible.builtin.group (the Linux Group class, and
// DarwinGroup on a macOS controller's local connection).
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
	darwin := platformSystem() == "Darwin"
	switch {
	case darwin:
		if argv, fail = darwinGroupArgv(env, p, found); fail != nil {
			return fail
		}
	case p.Str("state") == "absent":
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
	case !found:
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
	default:
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
			if !filepath.IsAbs(argv[0]) {
				bin, err := getBinPath(argv[0])
				if err != nil {
					return agentproto.Fail("%v", err)
				}
				argv[0] = bin
			}
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

// darwinGroupArgv is DarwinGroup's group_add / group_del / group_mod:
// dseditgroup, and for a new system group the lowest free gid under 500.
// Like upstream, a gid change runs even in check mode (DarwinGroup's
// group_mod does not consult it).
func darwinGroupArgv(env *RunEnv, p *args.Parsed, found bool) ([]string, *agentproto.Result) {
	name := p.Str("name")
	dsedit := func() (string, *agentproto.Result) {
		bin, err := getBinPath("dseditgroup")
		if err != nil {
			return "", agentproto.Fail("%v", err)
		}
		return bin, nil
	}
	switch {
	case p.Str("state") == "absent":
		if !found {
			return nil, nil
		}
		if env.CheckMode {
			return nil, &agentproto.Result{Changed: true}
		}
		if p.Bool("force") {
			return nil, agentproto.Fail("The force option is not supported for group deletion on this platform.")
		}
		bin, fail := dsedit()
		if fail != nil {
			return nil, fail
		}
		return []string{bin, "-o", "delete", "-L", name}, nil
	case !found:
		if env.CheckMode {
			return nil, &agentproto.Result{Changed: true}
		}
		bin, fail := dsedit()
		if fail != nil {
			return nil, fail
		}
		argv := []string{bin, "-o", "create"}
		if p.Has("gid") {
			argv = append(argv, "-i", strconv.FormatInt(p.Int("gid"), 10))
		} else if p.Bool("system") {
			if gid, ok := darwinLowestSystemGID(env); ok {
				argv = append(argv, "-i", strconv.FormatInt(gid, 10))
			}
		}
		return append(argv, "-L", name), nil
	default:
		if !p.Has("gid") || strconv.FormatInt(p.Int("gid"), 10) == groupID(env, name) {
			return nil, nil
		}
		bin, fail := dsedit()
		if fail != nil {
			return nil, fail
		}
		return []string{bin, "-o", "edit", "-i", strconv.FormatInt(p.Int("gid"), 10), "-L", name}, nil
	}
}

// darwinLowestSystemGID is get_lowest_available_system_gid: one past the
// highest gid under 500, unless there is none or it is 499.
func darwinLowestSystemGID(env *RunEnv) (int64, bool) {
	bin, err := getBinPath("dscl")
	if err != nil {
		return 0, false
	}
	_, out, _ := runCommand(env, []string{bin, "/Local/Default", "-list", "/Groups", "PrimaryGroupID"}, cmdOpts{})
	var highest int64
	for _, line := range pySplitlines(out) {
		parts := strings.Split(line, " ")
		if len(parts) < 2 {
			continue
		}
		gid, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		if err != nil {
			return 0, false
		}
		if gid > highest && gid < 500 {
			highest = gid
		}
	}
	if highest == 0 || highest == 499 {
		return 0, false
	}
	return highest + 1, true
}
