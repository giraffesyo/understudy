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

	if state == "present" && !env.CheckMode && (p.Bool("sysctl_set") || p.Bool("reload")) {
		// The live value lives at /proc/sys/<name with dots as slashes>.
		// Writing there directly is what `sysctl -w` does, minus the
		// procps-ng dependency.
		procPath := "/proc/sys/" + strings.ReplaceAll(name, ".", "/")
		current, rerr := os.ReadFile(procPath)
		if rerr != nil {
			if !p.Bool("ignoreerrors") {
				return agentproto.Fail("cannot read %s (%v); is %q a valid kernel parameter?", procPath, rerr, name)
			}
		} else if normalizeSysctl(string(current)) != normalizeSysctl(value) {
			if werr := os.WriteFile(procPath, []byte(value+"\n"), 0o644); werr != nil {
				if !p.Bool("ignoreerrors") {
					return agentproto.Fail("writing %s failed: %v", procPath, werr)
				}
			} else {
				res.Changed = true
			}
		}
	}
	return res
}

// normalizeSysctl collapses whitespace so tab- and space-separated values
// (e.g. "1\t2\t3") compare equal to the requested form.
func normalizeSysctl(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
