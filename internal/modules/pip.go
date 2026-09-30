package modules

import (
	"bytes"
	"fmt"
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
	p, fail := parseModuleArgs(pipSpec, rawArgs, "pip")
	if fail != nil {
		return fail
	}
	if err := pipSpec.MutuallyExclusive(rawArgs, []string{"name", "requirements"}, []string{"executable", "virtualenv"},
		[]string{"editable", "requirements"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	names := stringList(p.List("name"))
	requirements := p.Str("requirements")
	state := p.Str("state")
	if len(names) == 0 && requirements == "" {
		return agentproto.Fail("one of the following is required: name, requirements")
	}
	// The module needs packaging (or setuptools' pkg_resources) in the
	// task's Python.
	if !pyLibAvailable(env, "packaging", "") && !pyLibAvailable(env, "setuptools", "") {
		res := agentproto.Fail("%s", missingRequiredLib(env, "packaging", "", ""))
		res.Cause = "No module named 'packaging'"
		return res
	}
	if u := p.Str("umask"); u != "" {
		n, err := strconv.ParseUint(u, 8, 32)
		if err != nil {
			return &agentproto.Result{Failed: true, Msg: "umask must be an octal integer",
				Extra: map[string]any{"details": fmt.Sprintf("invalid literal for int() with base 8: %s", pyStrRepr(u))}}
		}
		old := syscall.Umask(int(n))
		defer syscall.Umask(old)
	}
	if state == "latest" && p.Str("version") != "" {
		return agentproto.Fail("version is incompatible with state=latest")
	}
	if v := strings.TrimLeft(p.Str("version"), " "); v != "" {
		if len(names) != 1 {
			return agentproto.Fail("'version' argument is ambiguous when installing multiple package distributions. " +
				"Please specify version restrictions next to each package in 'name' argument.")
		}
		if strings.ContainsAny(names[0], "<>=!~") {
			return agentproto.Fail("The 'version' argument conflicts with any version specifier provided along with a package name. " +
				"Please keep the version specifier, but remove the 'version' argument.")
		}
		if v[0] >= '0' && v[0] <= '9' {
			names = []string{names[0] + "==" + v}
		} else {
			names = []string{names[0] + v}
		}
	}
	chdir := p.Str("chdir")
	venv := p.Str("virtualenv")
	if venv != "" && chdir != "" {
		venv = filepath.Join(chdir, venv)
	}

	res := &agentproto.Result{Extra: map[string]any{
		"name": namesOrNil(names), "version": nilIfEmpty(p.Str("version")), "state": state,
		"requirements": nilIfEmpty(requirements), "virtualenv": nilIfEmpty(venv),
	}}
	var out, errOut strings.Builder
	venvCreated := false

	// Create the virtualenv when missing.
	if venv != "" && !pathExists(filepath.Join(venv, "bin", "activate")) {
		if fail := setupVirtualenv(env, p, venv, chdir, &out, &errOut); fail != nil {
			return fail
		}
		venvCreated = true
	}

	pip, fail := pipCommand(env, venv, p.Str("executable"))
	if fail != nil {
		return fail
	}

	cmd := append([]string{}, pip...)
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
	res.Extra["cmd"] = anyList(cmd)

	if env.CheckMode {
		// Without running pip: present/absent is judged from `pip list`.
		if p.Str("extra_args") != "" || requirements != "" || state == "latest" || len(names) == 0 {
			return &agentproto.Result{Changed: true}
		}
		listCmd := append(append([]string{}, pip...), "list", "--format=freeze")
		freeze, freezeErr, _ := runCapture(env, chdir, listCmd...)
		check := &agentproto.Result{Extra: map[string]any{"cmd": strings.Join(listCmd, " ")}}
		check.Stdout, check.Stderr = out.String()+freeze, errOut.String()+freezeErr
		if check.Stdout == "" {
			check.Extra["stdout"] = ""
		}
		if check.Stderr == "" {
			check.Extra["stderr"] = ""
		}
		for _, n := range names {
			present := freezeHas(freeze, n)
			if (state == "absent") == present {
				check.Changed = true
				break
			}
		}
		return check
	}

	var before string
	if requirements != "" || hasVCS {
		before, _, _ = runCapture(env, chdir, append(append([]string{}, pip...), "list", "--format=freeze")...)
	}
	o, e, rc := runCapture(env, chdir, cmd...)
	out.WriteString(o)
	errOut.WriteString(e)
	res.Extra["stdout"], res.Extra["stderr"] = out.String(), errOut.String()
	if rc != 0 {
		if rc == 1 && state == "absent" && (strings.Contains(o, "not installed") || strings.Contains(e, "not installed")) {
			return res // nothing to uninstall
		}
		// _fail: the command and pip's output as the message.
		msg := ""
		if s := out.String(); s != "" {
			msg += "stdout: " + s
		}
		if s := errOut.String(); s != "" {
			msg += "\n:stderr: " + s
		}
		return &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{"cmd": anyList(cmd)}}
	}
	switch {
	case state == "absent":
		res.Changed = strings.Contains(o, "Successfully uninstalled")
	case before != "":
		after, _, _ := runCapture(env, chdir, append(append([]string{}, pip...), "list", "--format=freeze")...)
		res.Changed = before != after
	default:
		res.Changed = strings.Contains(o, "Successfully installed")
	}
	res.Changed = res.Changed || venvCreated
	return res
}

// setupVirtualenv is the pip module's setup_virtualenv: in check mode the
// task is just changed; otherwise virtualenv_command (found on PATH)
// makes it, told which Python to use (virtualenv_python, else the task's
// own) unless it is the venv module or pyvenv.
func setupVirtualenv(env *RunEnv, p *args.Parsed, venv, chdir string, out, errOut *strings.Builder) *agentproto.Result {
	if env.CheckMode {
		return &agentproto.Result{Changed: true}
	}
	command := p.Str("virtualenv_command")
	cmd := strings.Fields(command)
	if len(cmd) == 0 {
		return agentproto.Fail("virtualenv_command is empty")
	}
	if filepath.Base(cmd[0]) == cmd[0] {
		bin, err := getBinPathIn(envPATH(env), cmd[0])
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		cmd[0] = bin
	}
	if p.Bool("virtualenv_site_packages") {
		cmd = append(cmd, "--system-site-packages")
	} else if help, _, _ := runCapture(env, "", cmd[0], "--help"); strings.Contains(help, "--no-site-packages") {
		cmd = append(cmd, "--no-site-packages")
	}
	py := p.Str("virtualenv_python")
	if !pipIsVenvCommand(command) {
		if py == "" {
			py = targetPythonExecutable(env)
		}
		cmd = append(cmd, "-p"+py)
	} else if py != "" {
		return agentproto.Fail("virtualenv_python should not be used when using the venv module or pyvenv as virtualenv_command")
	}
	cmd = append(cmd, venv)
	o, e, rc := runCapture(env, chdir, cmd...)
	out.WriteString(o)
	errOut.WriteString(e)
	if rc != 0 {
		msg := ""
		if s := out.String(); s != "" {
			msg += "stdout: " + s
		}
		if s := errOut.String(); s != "" {
			msg += "\n:stderr: " + s
		}
		return &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{"cmd": anyList(cmd)}}
	}
	return nil
}

// pipIsVenvCommand is _is_venv_command: pyvenv, or a command running
// the venv module (-m venv).
func pipIsVenvCommand(command string) bool {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return false
	}
	if argv[0] == "pyvenv" {
		return true
	}
	m := ""
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-m" && i+1 < len(argv):
			m = argv[i+1]
			i++
		case strings.HasPrefix(a, "-m") && len(a) > 2:
			m = a[2:]
		}
	}
	return m == "venv"
}

// pipCommand is the pip module's _get_pip: an absolute executable as
// given, a named one looked up; else, outside a virtualenv, the task's
// Python running its pip module when it has one, else pip3 on PATH; in
// a virtualenv its pip3 or pip.
func pipCommand(env *RunEnv, venv, executable string) ([]string, *agentproto.Result) {
	candidates := []string{"pip3"}
	switch {
	case executable != "":
		if filepath.IsAbs(executable) {
			return []string{executable}, nil
		}
		candidates = []string{executable}
	case venv == "" && pyPackageImportable(env, "pip"):
		return []string{targetPythonExecutable(env), "-m", "pip"}, nil
	}
	if venv == "" {
		for _, name := range candidates {
			if p, err := getBinPathIn(envPATH(env), name); err == nil {
				return []string{p}, nil
			}
		}
		return nil, agentproto.Fail("Unable to find any of %s to use.  pip needs to be installed.", strings.Join(candidates, ", "))
	}
	candidates = []string{candidates[0], "pip"}
	for _, name := range candidates {
		p := filepath.Join(venv, "bin", name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return []string{p}, nil
		}
	}
	return nil, agentproto.Fail("Unable to find pip in the virtualenv, %s, under any of these names: %s. Make sure pip is present in the virtualenv.",
		venv, strings.Join(candidates, ", "))
}

// pyPackageImportable reports whether the task's Python can import a
// package (a directory with an __init__.py on its sys.path); without a
// Python, false.
func pyPackageImportable(env *RunEnv, pkg string) bool {
	if !targetHasPython(env) {
		return false
	}
	for _, d := range pySitePackages(env) {
		if isFile(filepath.Join(d, pkg, "__init__.py")) {
			return true
		}
	}
	return false
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
	c := env.Command(path, argv[1:]...)
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
