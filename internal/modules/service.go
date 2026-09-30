package modules

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(serviceModule, "service", "ansible.builtin.service")
	Register(systemdModule, "systemd", "systemd_service", "ansible.builtin.systemd", "ansible.builtin.systemd_service")
}

// systemdSpec is ansible.builtin.systemd_service's argument spec.
var systemdSpec = args.Spec{
	"name":          {Aliases: []string{"service", "unit"}},
	"state":         {Choices: []string{"reloaded", "restarted", "started", "stopped"}},
	"enabled":       {Type: "bool"},
	"force":         {Type: "bool"},
	"masked":        {Type: "bool"},
	"daemon_reload": {Type: "bool", Default: false, Aliases: []string{"daemon-reload"}},
	"daemon_reexec": {Type: "bool", Default: false, Aliases: []string{"daemon-reexec"}},
	"scope":         {Default: "system", Choices: []string{"system", "user", "global"}},
	"no_block":      {Type: "bool", Default: false},
}

// serviceUnusedParams are the service options the service action drops,
// with a warning, when it runs the systemd module (UNUSED_PARAMS).
var serviceUnusedParams = []string{"pattern", "runlevel", "sleep", "arguments", "args"}

// serviceSpec is what a service task accepts: the service module's own
// options (use is the action's) plus systemd's, which the service action
// forwards to the systemd module it runs on systemd hosts.
var serviceSpec = func() args.Spec {
	s := args.Spec{
		"pattern":   {},
		"runlevel":  {Default: "default"},
		"sleep":     {Type: "int"},
		"arguments": {Default: "", Aliases: []string{"args"}},
		"use":       {Default: "auto"},
	}
	for k, v := range systemdSpec {
		if k == "state" {
			v.Choices = []string{"started", "stopped", "reloaded", "restarted"}
		}
		s[k] = v
	}
	return s
}()

// serviceModule is the service action on a systemd host: drop the
// options systemd does not use (warning about each) and run the systemd
// module. Other service managers are not supported.
func serviceModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if _, err := serviceSpec.Parse(rawArgs); err != nil {
		return agentproto.Fail("%v", err)
	}
	if use, ok := rawArgs["use"].(string); ok && use != "auto" && use != "systemd" {
		return agentproto.Fail("service use=%s is not supported (only systemd)", use)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return agentproto.Fail("this host is not running systemd (only systemd is supported in this version)")
	}
	fwd := map[string]any{}
	for k, v := range rawArgs {
		if k != "use" {
			fwd[k] = v
		}
	}
	var warnings []any
	for _, k := range serviceUnusedParams {
		if _, ok := fwd[k]; ok {
			delete(fwd, k)
			warnings = append(warnings, fmt.Sprintf("Ignoring \"%s\" as it is not used in \"systemd\"", k))
		}
	}
	res := systemdModule(env, fwd)
	if len(warnings) > 0 {
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		prev, _ := res.Extra["warnings"].([]any)
		res.Extra["warnings"] = append(warnings, prev...)
	}
	return res
}

// systemdRun carries one systemd invocation's systemctl command.
type systemdRun struct {
	env       *RunEnv
	systemctl []string
	xdg       string // XDG_RUNTIME_DIR to set when the environment lacks it
}

func (s *systemdRun) run(argv ...string) (int, string, string) {
	full := append(append([]string{}, s.systemctl...), argv...)
	o := cmdOpts{}
	if s.xdg != "" {
		o.Env = map[string]string{"XDG_RUNTIME_DIR": s.xdg}
	}
	return runCommand(s.env, full, o)
}

// systemdModule ports ansible.builtin.systemd_service.
func systemdModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := systemdSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	present := func(k string) bool {
		if v, ok := rawArgs[k]; ok && v != nil {
			return true
		}
		for _, a := range systemdSpec[k].Aliases {
			if v, ok := rawArgs[a]; ok && v != nil {
				return true
			}
		}
		return false
	}
	// required_one_of (state, enabled, masked, daemon_reload,
	// daemon_reexec) always holds: the daemon_* defaults count.
	for _, k := range []string{"state", "enabled", "masked"} {
		if present(k) && !present("name") {
			return agentproto.Fail("missing parameter(s) required by '%s': name", k)
		}
	}
	unit := p.Str("name")
	for _, g := range []string{"*", "?", "["} {
		if strings.Contains(unit, g) {
			return agentproto.Fail("This module does not currently support using glob patterns, found '%s' in service name: %s", g, unit)
		}
	}
	bin, err := getBinPath("systemctl")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	s := &systemdRun{env: env, systemctl: []string{bin}}
	if _, ok := env.Env["XDG_RUNTIME_DIR"]; !ok && os.Getenv("XDG_RUNTIME_DIR") == "" {
		s.xdg = fmt.Sprintf("/run/user/%d", os.Geteuid())
	}
	if scope := p.Str("scope"); scope != "system" {
		s.systemctl = append(s.systemctl, "--"+scope)
	}
	if p.Bool("no_block") {
		s.systemctl = append(s.systemctl, "--no-block")
	}
	if p.Bool("force") {
		s.systemctl = append(s.systemctl, "--force")
	}

	var warnings []any
	status := map[string]any{}
	extra := map[string]any{"name": nil, "status": status}
	if unit != "" {
		extra["name"] = unit
	}
	changed := false
	done := func() *agentproto.Result {
		res := &agentproto.Result{Changed: changed, Extra: extra}
		if len(warnings) > 0 {
			res.Extra["warnings"] = warnings
		}
		return res
	}
	fail := func(msg string) *agentproto.Result {
		return &agentproto.Result{Failed: true, Msg: msg}
	}
	offline := func() bool { return isChroot() || envLookup(env, "SYSTEMD_OFFLINE") == "1" }

	for _, d := range []struct{ param, verb string }{{"daemon_reload", "daemon-reload"}, {"daemon_reexec", "daemon-reexec"}} {
		if !p.Bool(d.param) || env.CheckMode {
			continue
		}
		if rc, _, errOut := s.run(d.verb); rc != 0 {
			if offline() {
				warnings = append(warnings, fmt.Sprintf("%s failed, but target is a chroot or systemd is offline. Continuing. Error was: %d / %s", d.verb, rc, errOut))
			} else {
				return fail(fmt.Sprintf("failure %d during %s: %s", rc, d.verb, errOut))
			}
		}
	}
	if unit == "" {
		return done()
	}

	isInitd := pathExists(sysvScript(unit))
	isSystemd := false
	rc, out, errOut := s.run("show", unit)
	switch {
	case rc == 0 && !requestWasIgnored(out) && !requestWasIgnored(errOut):
		if out != "" {
			for k, v := range parseSystemctlShow(strings.Split(out, "\n")) {
				status[k] = v
			}
			load, hasLoad := status["LoadState"].(string)
			isSystemd = hasLoad && load != "not-found"
			isMasked := hasLoad && load == "masked"
			if le, ok := status["LoadError"]; isSystemd && !isMasked && ok {
				return fail(fmt.Sprintf("Error loading unit file '%s': %s", unit, le))
			}
		}
	case errOut != "" && rc == 1 && strings.Contains(errOut, "Failed to parse bus message"):
		for k, v := range parseSystemctlShow(strings.Split(out, "\n")) {
			status[k] = v
		}
		base, _, sep := strings.Cut(unit, "@")
		search := base
		if sep {
			search += "@"
		}
		_, out, _ = s.run("list-unit-files", search+"*")
		isSystemd = strings.Contains(out, search)
		_, out, _ = s.run("is-active", unit)
		status["ActiveState"] = strings.TrimRight(out, "\n")
	default:
		valid := map[string]bool{"enabled": true, "enabled-runtime": true, "linked": true, "linked-runtime": true,
			"masked": true, "masked-runtime": true, "static": true, "indirect": true, "disabled": true,
			"generated": true, "transient": true}
		_, out, _ = s.run("is-enabled", unit)
		if valid[strings.TrimSpace(out)] {
			isSystemd = true
		} else if rc, _, _ := s.run("list-unit-files", unit); rc == 0 {
			isSystemd = true
		} else if rc, out, errOut := runCommand(env, s.systemctl, cmdOpts{}); rc != 0 {
			msg := errOut
			if msg == "" {
				msg = out
			}
			return &agentproto.Result{Failed: true, Msg: strings.TrimSpace(msg), RC: agentproto.IntPtr(rc),
				Stdout: out, Stderr: errOut, Extra: map[string]any{"cmd": strings.Join(s.systemctl, " ")}}
		}
	}
	found := isSystemd || isInitd
	if isInitd && !isSystemd {
		warnings = append(warnings, fmt.Sprintf("The service (%s) is actually an init script but the system is managed by systemd", unit))
	}
	missing := func() *agentproto.Result {
		if !found {
			return fail(fmt.Sprintf("Could not find the requested service %s: host", unit))
		}
		return nil
	}

	if p.Has("masked") {
		_, out, _ := s.run("is-enabled", unit)
		masked := strings.TrimSpace(out) == "masked"
		if masked != p.Bool("masked") {
			changed = true
			action := "unmask"
			if p.Bool("masked") {
				action = "mask"
			}
			if !env.CheckMode {
				if rc, _, errOut := s.run(action, unit); rc != 0 {
					if r := missing(); r != nil {
						return r
					}
					return fail(fmt.Sprintf("Failed to %s the service (%s): %s", action, unit, strings.TrimSpace(errOut)))
				}
			}
		}
	}

	if p.Has("enabled") {
		want := p.Bool("enabled")
		action := "disable"
		if want {
			action = "enable"
		}
		if r := missing(); r != nil {
			return r
		}
		enabled := false
		rc, out, _ := s.run("is-enabled", unit, "-l")
		switch rc {
		case 0:
			switch strings.TrimRight(out, " \t\r\n") {
			case "enabled-runtime", "indirect", "alias":
			default:
				enabled = true
			}
		case 1:
			if p.Str("scope") == "system" && isInitd && !strings.HasSuffix(strings.TrimSpace(out), "disabled") && sysvIsEnabled(unit) {
				enabled = true
			}
		}
		extra["enabled"] = enabled
		if enabled != want {
			changed = true
			if !env.CheckMode {
				if rc, out, errOut := s.run(action, unit); rc != 0 {
					return fail(fmt.Sprintf("Unable to %s service %s: %s", action, unit, out+errOut))
				}
			}
			extra["enabled"] = !enabled
		}
	}

	if p.Has("state") {
		if r := missing(); r != nil {
			return r
		}
		state := p.Str("state")
		extra["state"] = state
		if active, ok := status["ActiveState"].(string); ok {
			running := active == "active" || active == "activating"
			action := ""
			switch state {
			case "started":
				if !running {
					action = "start"
				}
			case "stopped":
				if running || active == "deactivating" {
					action = "stop"
				}
			default:
				if !running {
					action = "start"
				} else {
					action = strings.TrimSuffix(state, "ed")
				}
				extra["state"] = "started"
			}
			if action != "" {
				changed = true
				if !env.CheckMode {
					if rc, _, errOut := s.run(action, unit); rc != 0 {
						return fail(fmt.Sprintf("Unable to %s service %s: %s", action, unit, errOut))
					}
				}
			}
		} else if offline() {
			warnings = append(warnings, "Target is a chroot or systemd is offline. This can lead to false positives or prevent the init system tools from working.")
		} else {
			return &agentproto.Result{Failed: true, Msg: "Service is in unknown state", Extra: map[string]any{"status": status}}
		}
	}
	return done()
}

func envLookup(env *RunEnv, k string) string {
	if v, ok := env.Env[k]; ok {
		return v
	}
	return os.Getenv(k)
}

// requestWasIgnored is systemd_service's request_was_ignored().
func requestWasIgnored(out string) bool {
	return !strings.Contains(out, "=") && (strings.Contains(out, "ignoring request") || strings.Contains(out, "ignoring command"))
}

// parseSystemctlShow is parse_systemctl_show(): Key=Value lines, with
// multi-line {...} values only for Exec* keys.
func parseSystemctlShow(lines []string) map[string]string {
	parsed := map[string]string{}
	var multival []string
	k := ""
	for _, line := range lines {
		if k == "" {
			key, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			if strings.HasPrefix(key, "Exec") && strings.HasPrefix(strings.TrimLeft(v, " \t"), "{") &&
				!strings.HasSuffix(strings.TrimRight(v, " \t\r"), "}") {
				k = key
				multival = append(multival, v)
				continue
			}
			parsed[key] = strings.TrimSpace(v)
			continue
		}
		multival = append(multival, line)
		if strings.HasSuffix(strings.TrimRight(line, " \t\r"), "}") {
			parsed[k] = strings.TrimSpace(strings.Join(multival, "\n"))
			multival, k = nil, ""
		}
	}
	return parsed
}

// sysvScript is get_sysv_script().
func sysvScript(name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	return "/etc/init.d/" + name
}

// sysvIsEnabled is sysv_is_enabled(name) without a runlevel.
func sysvIsEnabled(name string) bool {
	pattern := "/etc/rc?.d/S??" + name
	if !isDir("/etc/rc0.d/") {
		pattern = "/etc/init.d/rc?.d/S??" + name
	}
	m, _ := filepath.Glob(pattern)
	return len(m) > 0
}

// isChroot is facts.system.chroot.is_chroot().
func isChroot() bool {
	if os.Getenv("debian_chroot") != "" {
		return true
	}
	var root, proc syscall.Stat_t
	if syscall.Stat("/", &root) != nil {
		return false
	}
	if syscall.Stat("/proc/1/root/.", &proc) == nil {
		return root.Ino != proc.Ino || uint64(root.Dev) != uint64(proc.Dev)
	}
	return root.Ino != 2
}

// applyEnv layers the task's environment onto a shell-out.
func applyEnv(cmd *exec.Cmd, env *RunEnv) {
	if len(env.Env) == 0 {
		return
	}
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(env.Env))
	for k := range env.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, env.Env[k]))
	}
}
