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
	Register(firewalldModule, "firewalld", "ansible.posix.firewalld")
	Register(selinuxModule, "selinux", "ansible.posix.selinux")
}

var firewalldSpec = args.Spec{
	"service":    {},
	"port":       {},
	"rich_rule":  {},
	"source":     {},
	"interface":  {},
	"zone":       {},
	"state":      {Required: true, Choices: []string{"enabled", "disabled"}},
	"permanent":  {Type: "bool", Default: false},
	"immediate":  {Type: "bool", Default: false},
	"masquerade": {},
}

// firewalldModule manages firewalld entries via firewall-cmd, querying
// before mutating.
func firewalldModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := firewalldSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if _, err := runOut(env, "firewall-cmd", "--state"); err != nil {
		return agentproto.Fail("firewalld is not running (firewall-cmd --state failed)")
	}

	// Exactly one entry kind.
	var queryArg, changeArg string
	switch {
	case p.Str("service") != "":
		queryArg = "--query-service=" + p.Str("service")
		changeArg = "-service=" + p.Str("service")
	case p.Str("port") != "":
		queryArg = "--query-port=" + p.Str("port")
		changeArg = "-port=" + p.Str("port")
	case p.Str("rich_rule") != "":
		queryArg = "--query-rich-rule=" + p.Str("rich_rule")
		changeArg = "-rich-rule=" + p.Str("rich_rule")
	case p.Str("source") != "":
		queryArg = "--query-source=" + p.Str("source")
		changeArg = "-source=" + p.Str("source")
	case p.Str("interface") != "":
		queryArg = "--query-interface=" + p.Str("interface")
		changeArg = "-interface=" + p.Str("interface")
	default:
		return agentproto.Fail("firewalld requires one of: service, port, rich_rule, source, interface")
	}

	var zoneArgs []string
	if z := p.Str("zone"); z != "" {
		zoneArgs = []string{"--zone=" + z}
	}
	enable := p.Str("state") == "enabled"
	res := &agentproto.Result{Extra: map[string]any{}}

	apply := func(permanent bool) *agentproto.Result {
		queryArgv := append(append([]string{}, zoneArgs...), queryArg)
		if permanent {
			queryArgv = append([]string{"--permanent"}, queryArgv...)
		}
		_, qerr := runOut(env, "firewall-cmd", queryArgv...)
		present := qerr == nil
		if present == enable {
			return nil // already in desired state
		}
		res.Changed = true
		if env.CheckMode {
			return nil
		}
		verb := "--add"
		if !enable {
			verb = "--remove"
		}
		changeArgv := append(append([]string{}, zoneArgs...), verb+changeArg)
		if permanent {
			changeArgv = append([]string{"--permanent"}, changeArgv...)
		}
		if out, err := runOut(env, "firewall-cmd", changeArgv...); err != nil {
			return agentproto.Fail("firewall-cmd failed: %v: %s", err, tail(out))
		}
		return nil
	}

	if p.Bool("permanent") {
		if fail := apply(true); fail != nil {
			return fail
		}
		if p.Bool("immediate") {
			if fail := apply(false); fail != nil {
				return fail
			}
		}
	} else {
		if fail := apply(false); fail != nil {
			return fail
		}
	}
	return res
}

var selinuxSpec = args.Spec{
	"state":      {Required: true, Choices: []string{"enforcing", "permissive", "disabled"}},
	"policy":     {},
	"configfile": {Default: "/etc/selinux/config"},
}

// selinuxModule sets the SELinux mode in the config file and live (via
// setenforce, where transitions allow).
func selinuxModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := selinuxSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	state := p.Str("state")
	configFile := p.Str("configfile")

	res := &agentproto.Result{Extra: map[string]any{"state": state}}

	original, err := os.ReadFile(configFile)
	if err != nil {
		return agentproto.Fail("reading %s: %v (is SELinux installed?)", configFile, err)
	}
	lines := splitFileLines(original)
	setKey := func(key, value string) {
		found := false
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, key+"=") {
				lines[i] = key + "=" + value
				found = true
			}
		}
		if !found {
			lines = append(lines, key+"="+value)
		}
	}
	setKey("SELINUX", state)
	if pol := p.Str("policy"); pol != "" {
		setKey("SELINUXTYPE", pol)
	}
	newContent := joinFileLines(lines, true)
	if !bytes.Equal(original, newContent) {
		res.Changed = true
		if !env.CheckMode {
			if err := fsutil.AtomicWrite(configFile, bytes.NewReader(newContent), 0o644); err != nil {
				return agentproto.Fail("writing %s: %v", configFile, err)
			}
		}
	}

	// Live transition: enforcing<->permissive works via setenforce;
	// to/from disabled needs a reboot (report and flag).
	current, gerr := runOut(env, "getenforce")
	if gerr == nil {
		cur := strings.ToLower(strings.TrimSpace(current))
		if cur != state {
			if state == "disabled" || cur == "disabled" {
				res.Extra["reboot_required"] = true
				res.Msg = "config updated; a reboot is required to change to/from disabled"
			} else if !env.CheckMode {
				arg := "1"
				if state == "permissive" {
					arg = "0"
				}
				if out, err := runOut(env, "setenforce", arg); err != nil {
					return agentproto.Fail("setenforce failed: %v: %s", err, tail(out))
				}
				res.Changed = true
			} else {
				res.Changed = true
			}
		}
	}
	_ = fmt.Sprintf
	return res
}
