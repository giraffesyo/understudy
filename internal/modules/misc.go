package modules

import (
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(modprobeModule, "modprobe", "community.general.modprobe")
}

var modprobeSpec = args.Spec{
	"name":   {Required: true},
	"state":  {Default: "present", Choices: []string{"present", "absent"}},
	"params": {},
}

// modprobeModule loads/unloads kernel modules, checking /proc/modules.
func modprobeModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := modprobeSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	loaded := moduleLoaded(name)
	res := &agentproto.Result{Extra: map[string]any{"name": name}}

	if p.Str("state") == "present" {
		if loaded {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		argv := []string{name}
		if params := p.Str("params"); params != "" {
			argv = append(argv, strings.Fields(params)...)
		}
		if out, err := runOut(env, "modprobe", argv...); err != nil {
			return agentproto.Fail("modprobe %s failed: %v: %s", name, err, tail(out))
		}
		return res
	}
	// absent
	if !loaded {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	if out, err := runOut(env, "modprobe", "-r", name); err != nil {
		return agentproto.Fail("modprobe -r %s failed: %v: %s", name, err, tail(out))
	}
	return res
}

func moduleLoaded(name string) bool {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	normalized := strings.ReplaceAll(name, "-", "_")
	for _, line := range strings.Split(string(data), "\n") {
		if mod, _, ok := strings.Cut(line, " "); ok &&
			strings.ReplaceAll(mod, "-", "_") == normalized {
			return true
		}
	}
	return false
}
