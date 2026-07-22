package modules

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(sysctlModule, "sysctl", "ansible.posix.sysctl")
}

var sysctlSpec = args.Spec{
	"name":         {Required: true, Aliases: []string{"key"}},
	"value":        {Aliases: []string{"val"}},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"sysctl_file":  {Default: "/etc/sysctl.conf"},
	"sysctl_set":   {Type: "bool", Default: false},
	"reload":       {Type: "bool", Default: true},
	"ignoreerrors": {Type: "bool", Default: false},
}

// sysctlModule manages kernel parameters in the sysctl file and,
// optionally, the live value.
func sysctlModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := sysctlSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	value := p.Str("value")
	state := p.Str("state")
	file := p.Str("sysctl_file")
	if state == "present" && !p.Has("value") {
		return agentproto.Fail("state=present requires 'value'")
	}

	res := &agentproto.Result{Extra: map[string]any{"name": name, "value": value}}

	original, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return agentproto.Fail("reading %s: %v", file, err)
	}
	lines := splitFileLines(original)

	found := false
	var newLines []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			newLines = append(newLines, line)
			continue
		}
		key, _, ok := strings.Cut(t, "=")
		if !ok || strings.TrimSpace(key) != name {
			newLines = append(newLines, line)
			continue
		}
		found = true
		if state == "absent" {
			continue // drop the line
		}
		newLines = append(newLines, fmt.Sprintf("%s = %s", name, value))
	}
	if state == "present" && !found {
		newLines = append(newLines, fmt.Sprintf("%s = %s", name, value))
	}

	newContent := joinFileLines(newLines, true)
	fileChanged := !bytes.Equal(original, newContent)
	if fileChanged {
		res.Changed = true
		if !env.CheckMode {
			if err := fsutil.AtomicWrite(file, bytes.NewReader(newContent), 0o644); err != nil {
				return agentproto.Fail("writing %s: %v", file, err)
			}
		}
	}

	if state == "present" && !env.CheckMode {
		// Live value: set when it differs (sysctl_set) or on reload.
		current, _ := runOut(env, "sysctl", "-n", name)
		liveDiffers := strings.TrimSpace(current) != value
		if liveDiffers && (p.Bool("sysctl_set") || p.Bool("reload")) {
			if out, err := runOut(env, "sysctl", "-w", fmt.Sprintf("%s=%s", name, value)); err != nil {
				if !p.Bool("ignoreerrors") {
					return agentproto.Fail("sysctl -w %s=%s failed: %v: %s", name, value, err, tail(out))
				}
			} else {
				res.Changed = true
			}
		}
	}
	return res
}
