package modules

import (
	"bytes"
	"os"
	"os/exec"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(serviceModule, "service", "systemd", "systemd_service",
		"ansible.builtin.service", "ansible.builtin.systemd", "ansible.builtin.systemd_service")
}

var serviceSpec = args.Spec{
	"name":          {Aliases: []string{"service", "unit"}},
	"state":         {Choices: []string{"started", "stopped", "restarted", "reloaded"}},
	"enabled":       {Type: "bool"},
	"daemon_reload": {Type: "bool", Default: false, Aliases: []string{"daemon-reload"}},
	"masked":        {Type: "bool"},
}

// serviceModule manages systemd units by comparing is-active/is-enabled to
// the desired state. Non-systemd init systems fail with a clear message.
func serviceModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := serviceSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return agentproto.Fail("this host is not running systemd (only systemd is supported in this version)")
	}
	name := p.Str("name")
	state := p.Str("state")
	wantEnabledSet := p.Has("enabled")

	res := &agentproto.Result{Extra: map[string]any{"name": name}}

	if p.Bool("daemon_reload") {
		if !env.CheckMode {
			if out, err := systemctl(env, "daemon-reload"); err != nil {
				return agentproto.Fail("daemon-reload failed: %v: %s", err, out)
			}
		}
		// daemon-reload alone doesn't count as changed in Ansible.
	}
	if name == "" {
		if p.Bool("daemon_reload") {
			return res
		}
		return agentproto.Fail("'name' is required unless only daemon_reload is requested")
	}

	// Enablement.
	if wantEnabledSet {
		out, _ := systemctl(env, "is-enabled", name)
		current := strings.TrimSpace(out)
		isEnabled := current == "enabled" || current == "enabled-runtime" || current == "alias"
		want := p.Bool("enabled")
		if isEnabled != want {
			res.Changed = true
			if !env.CheckMode {
				verb := "enable"
				if !want {
					verb = "disable"
				}
				if out, err := systemctl(env, verb, name); err != nil {
					return agentproto.Fail("systemctl %s %s failed: %v: %s", verb, name, err, out)
				}
			}
		}
		res.Extra["enabled"] = want
	}

	// Activity state.
	if state != "" {
		out, _ := systemctl(env, "is-active", name)
		isActive := strings.TrimSpace(out) == "active"
		var verb string
		willChange := false
		switch state {
		case "started":
			verb, willChange = "start", !isActive
		case "stopped":
			verb, willChange = "stop", isActive
		case "restarted":
			verb, willChange = "restart", true
		case "reloaded":
			verb, willChange = "reload", true
		}
		if willChange {
			res.Changed = true
			if !env.CheckMode {
				if out, err := systemctl(env, verb, name); err != nil {
					return agentproto.Fail("systemctl %s %s failed: %v: %s", verb, name, err, strings.TrimSpace(out))
				}
			}
		}
		res.Extra["state"] = state
	}
	return res
}

func systemctl(env *RunEnv, argv ...string) (string, error) {
	path, err := lookPath("systemctl")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(path, argv...)
	applyEnv(cmd, env)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

// applyEnv layers the task's environment onto a shell-out.
func applyEnv(cmd *exec.Cmd, env *RunEnv) {
	if len(env.Env) == 0 {
		return
	}
	cmd.Env = env.Environ()
}
