package modules

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(pipModule, "pip", "ansible.builtin.pip")
}

var pipSpec = args.Spec{
	"name":               {Type: "list"},
	"state":              {Default: "present", Choices: []string{"present", "absent", "latest", "forcereinstall"}},
	"requirements":       {},
	"virtualenv":         {},
	"executable":         {},
	"extra_args":         {},
	"virtualenv_command": {},
	"virtualenv_python":  {},
}

// pipModule manages Python packages: `pip show` for presence, then
// install/uninstall. Virtualenvs are created on demand.
func pipModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := pipSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	names := stringList(p.List("name"))
	requirements := p.Str("requirements")
	if len(names) == 0 && requirements == "" {
		return agentproto.Fail("pip requires 'name' or 'requirements'")
	}

	venv := p.Str("virtualenv")
	pip := p.Str("executable")
	if pip == "" {
		if venv != "" {
			pip = venv + "/bin/pip"
		} else {
			for _, cand := range []string{"pip3", "pip"} {
				if _, err := exec.LookPath(cand); err == nil {
					pip = cand
					break
				}
			}
			if pip == "" {
				return agentproto.Fail("no pip executable found on the target")
			}
		}
	}

	res := &agentproto.Result{Extra: map[string]any{}}

	// Create the virtualenv when missing.
	if venv != "" {
		if _, err := runOut(env, "test", "-x", venv+"/bin/pip"); err != nil {
			res.Changed = true
			if env.CheckMode {
				return res
			}
			cmd := p.Str("virtualenv_command")
			python := p.Str("virtualenv_python")
			if python == "" {
				python = "python3"
			}
			var out string
			var verr error
			if cmd != "" {
				parts := strings.Fields(cmd)
				out, verr = runOut(env, parts[0], append(parts[1:], venv)...)
			} else {
				out, verr = runOut(env, python, "-m", "venv", venv)
			}
			if verr != nil {
				return agentproto.Fail("creating virtualenv %s failed: %v: %s", venv, verr, tail(out))
			}
		}
	}

	state := p.Str("state")
	extra := strings.Fields(p.Str("extra_args"))

	if requirements != "" {
		if env.CheckMode {
			res.Changed = true // cannot know without resolving; assume change
			return res
		}
		argv := append([]string{"install", "-r", requirements}, extra...)
		out, err := runOut(env, pip, argv...)
		if err != nil {
			return agentproto.Fail("pip install -r failed: %v: %s", err, tail(out))
		}
		res.Changed = !strings.Contains(out, "Requirement already satisfied") ||
			strings.Contains(out, "Successfully installed")
		res.Stdout = tail(out)
		return res
	}

	installed := func(pkg string) bool {
		base := pkg
		for _, sep := range []string{"==", ">=", "<=", ">", "<", "~="} {
			if i := strings.Index(base, sep); i > 0 {
				base = base[:i]
				break
			}
		}
		_, err := runOut(env, pip, "show", base)
		return err == nil
	}

	switch state {
	case "absent":
		var present []string
		for _, pkg := range names {
			if installed(pkg) {
				present = append(present, pkg)
			}
		}
		if len(present) == 0 {
			return res
		}
		res.Changed = true
		if env.CheckMode {
			return res
		}
		argv := append([]string{"uninstall", "-y"}, present...)
		if out, err := runOut(env, pip, argv...); err != nil {
			return agentproto.Fail("pip uninstall failed: %v: %s", err, tail(out))
		}
	default: // present, latest, forcereinstall
		var missing []string
		for _, pkg := range names {
			if !installed(pkg) {
				missing = append(missing, pkg)
			}
		}
		targets := missing
		if state == "latest" || state == "forcereinstall" {
			targets = names
		}
		if len(targets) == 0 {
			return res
		}
		if env.CheckMode {
			res.Changed = true
			return res
		}
		argv := []string{"install"}
		if state == "latest" {
			argv = append(argv, "--upgrade")
		}
		if state == "forcereinstall" {
			argv = append(argv, "--force-reinstall")
		}
		argv = append(append(argv, extra...), targets...)
		out, err := runOut(env, pip, argv...)
		if err != nil {
			return agentproto.Fail("pip install failed: %v: %s", err, tail(out))
		}
		res.Changed = len(missing) > 0 || strings.Contains(out, "Successfully installed")
		res.Stdout = tail(out)
	}
	res.Extra["name"] = names
	_ = fmt.Sprintf
	return res
}
