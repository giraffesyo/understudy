package modules

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(pipModule, "pip", "ansible.builtin.pip")
}

var pipSpec = args.Spec{
	"name":                     {Type: "list"},
	"version":                  {},
	"state":                    {Default: "present", Choices: []string{"present", "absent", "latest", "forcereinstall"}},
	"requirements":             {},
	"virtualenv":               {},
	"virtualenv_site_packages": {Type: "bool", Default: false},
	"virtualenv_command":       {Default: "virtualenv"},
	"virtualenv_python":        {},
	"executable":               {},
	"extra_args":               {},
	"editable":                 {Type: "bool", Default: false},
	"chdir":                    {},
	"umask":                    {},
	"break_system_packages":    {Type: "bool", Default: false},
}

// pipModule mirrors ansible.builtin.pip: create the virtualenv if needed,
// then run pip install/uninstall and let pip decide — changed means pip
// reported installing/uninstalling something (or, for requirements files
// and VCS URLs, that `pip freeze` differs before and after).
func pipModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := pipSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	names := stringList(p.List("name"))
	requirements := p.Str("requirements")
	state := p.Str("state")
	if len(names) == 0 && requirements == "" {
		return agentproto.Fail("one of the following is required: name, requirements")
	}
	if p.Str("version") != "" {
		if len(names) != 1 {
			return agentproto.Fail("version is incompatible with multiple packages")
		}
		if state == "latest" {
			return agentproto.Fail("version is incompatible with state=latest")
		}
		names = []string{names[0] + "==" + p.Str("version")}
	}
	if u := p.Str("umask"); u != "" {
		n, err := strconv.ParseUint(u, 8, 32)
		if err != nil {
			return agentproto.Fail("umask must be an octal integer")
		}
		old := syscall.Umask(int(n))
		defer syscall.Umask(old)
	}
	chdir := p.Str("chdir")
	venv := p.Str("virtualenv")
	if venv != "" && chdir != "" && !filepath.IsAbs(venv) {
		venv = filepath.Join(chdir, venv)
	}

	res := &agentproto.Result{Extra: map[string]any{
		"name": namesOrNil(names), "version": nilIfEmpty(p.Str("version")), "state": state,
		"requirements": nilIfEmpty(requirements), "virtualenv": nilIfEmpty(venv),
	}}
	var out, errOut strings.Builder

	// Create the virtualenv when missing.
	if venv != "" {
		if _, err := os.Stat(filepath.Join(venv, "bin", "activate")); err != nil {
			if env.CheckMode {
				res.Changed = true
				return res
			}
			cmd := strings.Fields(p.Str("virtualenv_command"))
			if len(cmd) > 0 && strings.HasSuffix(cmd[len(cmd)-1], "venv") && len(cmd) >= 2 && cmd[len(cmd)-2] == "-m" {
				// "python -m venv": no --python flag, as Ansible handles it
			} else if py := p.Str("virtualenv_python"); py != "" {
				cmd = append(cmd, "-p"+py)
			}
			if p.Bool("virtualenv_site_packages") {
				cmd = append(cmd, "--system-site-packages")
			}
			cmd = append(cmd, venv)
			o, e, rc := runCapture(env, chdir, cmd...)
			out.WriteString(o)
			errOut.WriteString(e)
			if rc != 0 {
				res.Failed, res.Msg = true, "Failed to create virtualenv"
				res.RC = agentproto.IntPtr(rc)
				res.Stdout, res.Stderr = out.String(), errOut.String()
				res.Extra["cmd"] = strings.Join(cmd, " ")
				return res
			}
		}
	}

	pip := p.Str("executable")
	switch {
	case venv != "":
		pip = filepath.Join(venv, "bin", "pip")
	case pip == "":
		for _, cand := range []string{"pip3", "pip"} {
			if _, err := exec.LookPath(cand); err == nil {
				pip = cand
				break
			}
		}
		if pip == "" {
			return agentproto.Fail("Unable to find any of pip3, pip to use.  pip needs to be installed.")
		}
	}

	cmd := strings.Fields(pip)
	if state == "absent" {
		cmd = append(cmd, "uninstall", "-y")
	} else {
		cmd = append(cmd, "install")
		switch state {
		case "latest":
			cmd = append(cmd, "-U")
		case "forcereinstall":
			cmd = append(cmd, "--force-reinstall")
		}
	}
	if p.Bool("break_system_packages") {
		cmd = append(cmd, "--break-system-packages")
	}
	cmd = append(cmd, strings.Fields(p.Str("extra_args"))...)
	hasVCS := false
	for _, n := range names {
		if vcsRe.MatchString(n) {
			hasVCS = true
		}
	}
	if len(names) > 0 {
		if p.Bool("editable") {
			for _, n := range names {
				cmd = append(cmd, "-e", n)
			}
		} else {
			cmd = append(cmd, names...)
		}
	}
	if requirements != "" {
		cmd = append(cmd, "-r", requirements)
	}
	res.Extra["cmd"] = strings.Join(cmd, " ")

	if env.CheckMode {
		// Without running pip: present/absent is judged from `pip freeze`.
		if requirements != "" || hasVCS {
			res.Changed = true
			return res
		}
		freeze, _, _ := runCapture(env, chdir, append(strings.Fields(pip), "list", "--format=freeze")...)
		for _, n := range names {
			present := freezeHas(freeze, n)
			if (state == "absent") == present {
				res.Changed = true
				break
			}
		}
		return res
	}

	var before string
	if requirements != "" || hasVCS {
		before, _, _ = runCapture(env, chdir, append(strings.Fields(pip), "list", "--format=freeze")...)
	}
	o, e, rc := runCapture(env, chdir, cmd...)
	out.WriteString(o)
	errOut.WriteString(e)
	res.Stdout, res.Stderr = out.String(), errOut.String()
	if rc != 0 {
		if state == "absent" && strings.Contains(e, "not installed") {
			return res // nothing to uninstall
		}
		res.Failed = true
		res.RC = agentproto.IntPtr(rc)
		res.Msg = "stdout: " + o + "\n:stderr: " + e
		return res
	}
	switch {
	case state == "absent":
		res.Changed = strings.Contains(o, "Successfully uninstalled")
	case before != "":
		after, _, _ := runCapture(env, chdir, append(strings.Fields(pip), "list", "--format=freeze")...)
		res.Changed = before != after
	default:
		res.Changed = strings.Contains(o, "Successfully installed")
	}
	return res
}

var vcsRe = regexp.MustCompile(`(svn|git|hg|bzr)\+`)

// freezeHas reports whether a requirement's project appears in `pip list
// --format=freeze` output (name comparison per PEP 503).
func freezeHas(freeze, req string) bool {
	name := req
	if i := strings.IndexAny(name, "<>=!~;[ "); i > 0 {
		name = name[:i]
	}
	norm := func(s string) string {
		return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(s, "-"))
	}
	want := norm(name)
	for _, line := range strings.Split(freeze, "\n") {
		if n, _, ok := strings.Cut(line, "=="); ok && norm(strings.TrimSpace(n)) == want {
			return true
		}
	}
	return false
}

func runCapture(env *RunEnv, dir string, argv ...string) (string, string, int) {
	path, err := lookPath(argv[0])
	if err != nil {
		return "", err.Error(), 127
	}
	c := exec.Command(path, argv[1:]...)
	c.Dir = dir
	applyEnv(c, env)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return stdout.String(), stderr.String(), ee.ExitCode()
		}
		return stdout.String(), err.Error(), 1
	}
	return stdout.String(), stderr.String(), 0
}

func namesOrNil(names []string) any {
	if len(names) == 0 {
		return nil
	}
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
