package modules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// pipStateArgs is the module's state_map.
var pipStateArgs = map[string][]string{
	"present":        {"install"},
	"absent":         {"uninstall", "-y"},
	"latest":         {"install", "-U"},
	"forcereinstall": {"install", "-U", "--force-reinstall"},
}

// pipRun is the pip module's state: its run_command settings.
type pipRun struct {
	env   *RunEnv
	umask int // -1: unchanged
	// extraEnv is what the module puts in os.environ for every command
	// (PIP_BREAK_SYSTEM_PACKAGES).
	extraEnv map[string]string
	flavor   pkgFlavor
	// safeExtras: the Requirement is pkg_resources', whose extras are
	// safe_extra()ed.
	safeExtras bool
	warnings   []any
}

// pipModule is ansible.builtin.pip: create the virtualenv if needed,
// then run pip install/uninstall and let pip decide; changed means pip
// reported installing/uninstalling something (or, for requirements files
// and VCS URLs, that the package list differs before and after). Names
// are parsed as `packaging` (the release the task's Python has) parses
// PEP 508 requirements, which decides how they appear on pip's command
// line and, in check mode, whether an installed version satisfies them.
func pipModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, fail := parseModuleArgs(pipSpec, rawArgs, "pip")
	if fail != nil {
		return fail
	}
	if err := pipSpec.MutuallyExclusive(rawArgs, []string{"name", "requirements"}, []string{"executable", "virtualenv"},
		[]string{"editable", "requirements"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	_, hasName := rawArgs["name"]
	_, hasReq := rawArgs["requirements"]
	if !hasName && !hasReq {
		return agentproto.Fail("one of the following is required: name, requirements")
	}
	run := &pipRun{env: env, umask: -1}
	// The module imports packaging's Requirement, or pkg_resources'.
	switch f, ok := pyImportableVersion(env, "packaging"); {
	case ok:
		run.flavor = f
	default:
		f, ok := pkgResourcesFlavor(env)
		if !ok {
			res := agentproto.Fail("%s", missingRequiredLib(env, "packaging", "", ""))
			res.Cause = "No module named 'packaging'"
			return res
		}
		run.flavor, run.safeExtras = f, true
	}

	var name []string
	nameGiven := rawArgs["name"] != nil
	if nameGiven {
		name = pipNameParam(rawArgs["name"])
	}
	version, versionGiven := "", rawArgs["version"] != nil
	if versionGiven {
		version = p.Str("version")
	}
	state := p.Str("state")
	requirements := p.Str("requirements")
	extraArgs := p.Str("extra_args")
	chdir := pyExpandPath(p.Str("chdir"))
	venv := pyExpandPath(p.Str("virtualenv"))
	executable := pyExpandPath(p.Str("executable"))
	editable := p.Bool("editable")

	if venv != "" && chdir != "" {
		venv = pyJoin(chdir, venv)
	}
	if u := p.Str("umask"); u != "" {
		n, ok := pyIntBase8(u)
		if !ok {
			return &agentproto.Result{Failed: true, Msg: "umask must be an octal integer",
				Extra: map[string]any{"details": fmt.Sprintf("invalid literal for int() with base 8: %s", pyStrRepr(u))}}
		}
		run.umask = int(n)
	}
	if state == "latest" && versionGiven {
		return agentproto.Fail("version is incompatible with state=latest")
	}
	if chdir == "" {
		// Avoids permission issues under privilege escalation.
		chdir = pyGettempdir()
	}

	var out, errOut string
	var venvCmd []string
	venvCreated := false
	pyBin := ""
	if venv != "" {
		if !pathExists(filepath.Join(venv, "bin", "activate")) {
			venvCreated = true
			if env.CheckMode {
				return &agentproto.Result{Changed: true}
			}
			var fail *agentproto.Result
			if venvCmd, fail = run.setupVirtualenv(p, venv, chdir, &out, &errOut); fail != nil {
				return fail
			}
		}
		pyBin = filepath.Join(venv, "bin", "python")
	} else {
		pyBin = executable
		if pyBin == "" {
			pyBin = targetPythonExecutable(env)
		}
	}

	pip, fail := pipCommand(env, venv, executable)
	if fail != nil {
		return fail
	}
	cmd := append(append([]string{}, pip...), pipStateArgs[state]...)
	pathPrefix := ""
	if venv != "" {
		pathPrefix = filepath.Join(venv, "bin")
	}

	hasVCS := false
	var packages []*pipPackage
	if len(name) > 0 {
		for _, n := range name {
			if n != "" && pipVCSRe.MatchString(n) {
				hasVCS = true
				break
			}
		}
		for _, n := range pipRecoverPackageNames(name) {
			packages = append(packages, run.newPackage(n, "", false))
		}
		if versionGiven {
			if len(packages) > 1 {
				return agentproto.Fail("'version' argument is ambiguous when installing multiple package distributions. " +
					"Please specify version restrictions next to each package in 'name' argument.")
			}
			if packages[0].hasVersionSpecifier() {
				return agentproto.Fail("The 'version' argument conflicts with any version specifier provided along with a package name. " +
					"Please keep the version specifier, but remove the 'version' argument.")
			}
			packages[0] = run.newPackage(packages[0].String(), version, true)
		}
	}
	if extraArgs != "" {
		words, err := shlexSplit(extraArgs)
		if err != nil {
			return agentproto.Fail("%s", "No closing quotation")
		}
		cmd = append(cmd, words...)
	}
	if p.Bool("break_system_packages") {
		// An environment variable rather than --break-system-packages,
		// which pip 23.0.0 and earlier reject.
		run.extraEnv = map[string]string{"PIP_BREAK_SYSTEM_PACKAGES": "1"}
	}

	switch {
	case len(name) > 0:
		for _, pkg := range packages {
			if editable {
				cmd = append(cmd, "-e")
			}
			cmd = append(cmd, pkg.String())
		}
	case requirements != "":
		cmd = append(cmd, "-r", requirements)
	case venvCreated:
		// Only creating an empty virtualenv.
		return &agentproto.Result{Changed: true, Extra: map[string]any{
			"cmd": anyList(venvCmd), "name": pipNameResult(name, nameGiven), "version": nilUnless(version, versionGiven),
			"state": state, "requirements": nilIfEmptyGiven(requirements, hasReq && rawArgs["requirements"] != nil),
			"virtualenv": nilIfEmpty(venv), "stdout": out, "stderr": errOut}}
	default:
		return &agentproto.Result{Extra: map[string]any{"warnings": []any{"No valid name or requirements file found."}}}
	}

	if env.CheckMode {
		if extraArgs != "" || requirements != "" || state == "latest" || len(name) == 0 {
			return &agentproto.Result{Changed: true}
		}
		pkgCmd, outPip, errPip, fail := run.getPackages(pip, chdir)
		if fail != nil {
			return fail
		}
		out += outPip
		errOut += errPip
		changed := false
		var pkgList []string
		for _, line := range strings.Split(out, "\n") {
			if line != "" && !strings.HasPrefix(line, "You are using") && !strings.HasPrefix(line, "You should consider") {
				pkgList = append(pkgList, line)
			}
		}
		if strings.HasSuffix(pkgCmd, " freeze") && (pipContains(name, "pip") || pipContains(name, "setuptools")) {
			// pip freeze does not list setuptools or pip.
			for _, pkg := range []string{"setuptools", "pip"} {
				if pipContains(name, pkg) {
					if dep, ok := run.packageInfo(pkg, pyBin); ok {
						pkgList = append(pkgList, dep)
						out += dep + "\n"
					}
				}
			}
		}
		resolved, fail := run.resolvePackageNames(packages, pip, pyBin)
		if fail != nil {
			return fail
		}
		for _, pkg := range resolved {
			present, err := pkg.isPresent(pkgList)
			if err != nil {
				return pipModuleCrash(err)
			}
			if (state == "present" && !present) || (state == "absent" && present) {
				changed = true
				break
			}
		}
		res := &agentproto.Result{Changed: changed, Extra: map[string]any{"cmd": pkgCmd, "stdout": out, "stderr": errOut}}
		if len(run.warnings) > 0 {
			res.Extra["warnings"] = run.warnings
		}
		return res
	}

	var freezeBefore *string
	if requirements != "" || hasVCS {
		_, before, _, fail := run.getPackages(pip, chdir)
		if fail != nil {
			return fail
		}
		freezeBefore = &before
	}
	rc, outPip, errPip := run.command(cmd, chdir, pathPrefix, nil)
	out += outPip
	errOut += errPip
	if rc == 1 && state == "absent" && (strings.Contains(outPip, "not installed") || strings.Contains(errPip, "not installed")) {
		// rc is 1 when uninstalling a package that is not installed.
	} else if rc != 0 {
		return pipFail(cmd, out, errOut)
	}
	changed := false
	switch {
	case state == "absent":
		changed = strings.Contains(outPip, "Successfully uninstalled")
	case freezeBefore == nil:
		changed = strings.Contains(outPip, "Successfully installed")
	default:
		_, after, _, fail := run.getPackages(pip, chdir)
		if fail != nil {
			return fail
		}
		changed = *freezeBefore != after
	}
	return &agentproto.Result{Changed: changed || venvCreated, Extra: map[string]any{
		"cmd": anyList(cmd), "name": pipNameResult(name, nameGiven), "version": nilUnless(version, versionGiven),
		"state": state, "requirements": nilIfEmptyGiven(requirements, hasReq && rawArgs["requirements"] != nil),
		"virtualenv": nilIfEmpty(venv), "stdout": out, "stderr": errOut}}
}

// pyIntBase8 is int(s, 8): surrounding whitespace, a sign, a 0o prefix
// and single underscores between digits allowed.
func pyIntBase8(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'o' || s[1] == 'O') {
		s = strings.TrimPrefix(s[2:], "_")
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 8, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// pipNameParam is the name option as AnsibleModule converts it (a list
// of str elements; a string splits on commas, unstripped).
func pipNameParam(v any) []string {
	switch t := v.(type) {
	case string:
		return strings.Split(t, ",")
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			out = append(out, pyStrElement(it))
		}
		return out
	}
	return []string{pyStrElement(v)}
}

// pyStrElement is a list element converted to str.
func pyStrElement(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	case float64:
		return pyFloatRepr(t)
	}
	return fmt.Sprint(v)
}

func pipNameResult(name []string, given bool) any {
	if !given {
		return nil
	}
	return anyList(name)
}

func nilUnless(s string, given bool) any {
	if !given {
		return nil
	}
	return s
}

func nilIfEmptyGiven(s string, given bool) any {
	if !given {
		return nil
	}
	return s
}

func pipContains(names []string, s string) bool {
	for _, n := range names {
		if n == s {
			return true
		}
	}
	return false
}

// pipFail is the module's _fail: the command and pip's output as msg.
func pipFail(cmd []string, out, errOut string) *agentproto.Result {
	msg := ""
	if out != "" {
		msg += "stdout: " + out
	}
	if errOut != "" {
		msg += "\n:stderr: " + errOut
	}
	return &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{"cmd": anyList(cmd)}}
}

// pipModuleCrash is the module dying on an exception it does not catch.
func pipModuleCrash(err error) *agentproto.Result {
	return &agentproto.Result{Failed: true, Msg: "MODULE FAILURE: No start of json char found\nSee stdout/stderr for the exact error",
		Extra: map[string]any{"module_stdout": "", "module_stderr": "Traceback (most recent call last):\n" +
			"packaging.version.InvalidVersion: " + err.Error() + "\n", "rc": int64(1)}}
}

// command is module.run_command(argv, cwd=..., path_prefix=...,
// environ_update=...) under the module's umask and environment.
func (r *pipRun) command(argv []string, cwd, pathPrefix string, update map[string]string) (int, string, string) {
	envUpdate := map[string]string{}
	for k, v := range r.extraEnv {
		envUpdate[k] = v
	}
	for k, v := range update {
		envUpdate[k] = v
	}
	if pathPrefix != "" {
		if path := envPATH(r.env); path != "" {
			envUpdate["PATH"] = pathPrefix + ":" + path
		} else {
			envUpdate["PATH"] = pathPrefix
		}
	}
	if r.umask >= 0 {
		path := argv[0]
		if !strings.Contains(path, "/") {
			if p, err := lookPath(path); err == nil {
				path = p
			}
		}
		argv = append([]string{"/bin/sh", "-c", fmt.Sprintf("umask %o; exec \"$@\"", r.umask), "sh", path}, argv[1:]...)
	}
	if len(envUpdate) == 0 {
		envUpdate = nil
	}
	return runCommand(r.env, argv, cmdOpts{Cwd: cwd, Env: envUpdate})
}

// getPackages is _get_packages: `pip list --format=freeze`, else (an
// old pip) `pip freeze`. It returns the command as a string.
func (r *pipRun) getPackages(pip []string, chdir string) (string, string, string, *agentproto.Result) {
	command := append(append([]string{}, pip...), "list", "--format=freeze")
	rc, out, errOut := r.command(command, chdir, "", localeEnv3(bestParsableLocale(r.env)))
	if rc != 0 {
		command = append(append([]string{}, pip...), "freeze")
		rc, out, errOut = r.command(command, chdir, "", nil)
		if rc != 0 {
			return "", "", "", pipFail(command, out, errOut)
		}
	}
	return strings.Join(command, " "), out, errOut, nil
}

// localeEnv3 is the LANG/LC_ALL/LC_MESSAGES environ_update _get_packages
// passes.
func localeEnv3(loc string) map[string]string {
	return map[string]string{"LANG": loc, "LC_ALL": loc, "LC_MESSAGES": loc}
}

// packageInfo is _get_package_info: the version of pip or setuptools
// (which `pip freeze` does not list) as "name==version", asked of the
// Python that runs pip.
func (r *pipRun) packageInfo(pkg, pyBin string) (string, bool) {
	checkers := map[string]map[string]string{
		"importlib": {
			"setuptools": `from importlib.metadata import version; print(version("setuptools"))`,
			"pip":        `from importlib.metadata import version; print(version("pip"))`,
		},
		"pkg_resources": {
			"setuptools": "import setuptools; print(setuptools.__version__)",
			"pip":        `import pkg_resources; print(pkg_resources.get_distribution("pip").version)`,
		},
	}
	mechanism := "pkg_resources"
	if rc, _, _ := r.command([]string{pyBin, "-c", "import importlib.metadata"}, "", "", nil); rc == 0 {
		mechanism = "importlib"
	}
	rc, out, _ := r.command([]string{pyBin, "-c", checkers[mechanism][pkg]}, "", "", nil)
	if rc != 0 {
		return "", false
	}
	return pkg + "==" + strings.TrimSpace(out), true
}

// resolvePackageNames is _resolve_package_names: check mode asks pip
// (24.1+, `install --dry-run --report`) which distributions the
// references packaging cannot parse (VCS URLs, paths, archives) are.
func (r *pipRun) resolvePackageNames(packages []*pipPackage, pip []string, pyBin string) ([]*pipPackage, *agentproto.Result) {
	var toResolve, other []*pipPackage
	for _, pkg := range packages {
		if pkg.req == nil {
			toResolve = append(toResolve, pkg)
		} else {
			other = append(other, pkg)
		}
	}
	if len(toResolve) == 0 {
		return packages, nil
	}
	dep, ok := r.packageInfo("pip", pyBin)
	if !ok {
		return nil, &agentproto.Result{Failed: true,
			Msg: "MODULE FAILURE: No start of json char found\nSee stdout/stderr for the exact error",
			Extra: map[string]any{"module_stdout": "", "rc": int64(1), "module_stderr": "Traceback (most recent call last):\n" +
				"AttributeError: 'NoneType' object has no attribute 'split'\n"}}
	}
	if _, installed, _ := strings.Cut(dep, "=="); looseVersionLess(installed, "24.1") {
		r.warnings = append(r.warnings, "Using check mode with packages from vcs urls, file paths, or archives will not behave as expected when using pip versions <24.1.")
		return packages, nil
	}
	// tempfile.NamedTemporaryFile(): tmp + 8 random characters.
	var tmp *os.File
	var err error
	for i := 0; i < 100; i++ {
		tmp, err = os.OpenFile(filepath.Join(pyGettempdir(), "tmp"+pyTempName()), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if !os.IsExist(err) {
			break
		}
	}
	if err != nil {
		return nil, agentproto.Fail("%v", err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	argv := append(append([]string{}, pip...), "install", "--dry-run", "--ignore-installed", "--report="+tmp.Name())
	for _, pkg := range toResolve {
		argv = append(argv, pkg.String())
	}
	rc, out, errOut := r.command(argv, "", "", nil)
	if rc != 0 {
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = shlexQuote(a)
		}
		return nil, &agentproto.Result{Failed: true, Msg: heuristicLogSanitize(strings.TrimRight(errOut, " \t\r\n\v\f")),
			RC: agentproto.IntPtr(rc), Stdout: out, Stderr: errOut, Extra: map[string]any{"cmd": strings.Join(quoted, " ")}}
	}
	data, _ := os.ReadFile(tmp.Name())
	var report struct {
		Install []struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"install"`
	}
	json.Unmarshal(data, &report)
	for _, inst := range report.Install {
		other = append(other, r.newPackage(inst.Metadata.Name, inst.Metadata.Version, true))
	}
	return other, nil
}

// setupVirtualenv is setup_virtualenv: virtualenv_command (found on
// PATH) makes the environment, told which Python to use
// (virtualenv_python, else the task's own) unless it is the venv module
// or pyvenv.
func (r *pipRun) setupVirtualenv(p *args.Parsed, venv, chdir string, out, errOut *string) ([]string, *agentproto.Result) {
	command := pyExpandPath(p.Str("virtualenv_command"))
	cmd, err := shlexSplit(command)
	if err != nil || len(cmd) == 0 {
		return nil, agentproto.Fail("%s", "virtualenv_command could not be parsed")
	}
	if filepath.Base(cmd[0]) == cmd[0] {
		bin, err := getBinPathIn(envPATH(r.env), cmd[0])
		if err != nil {
			return nil, agentproto.Fail("%v", err)
		}
		cmd[0] = bin
	}
	if p.Bool("virtualenv_site_packages") {
		cmd = append(cmd, "--system-site-packages")
	} else {
		helpCmd, _ := shlexSplit(cmd[0] + " --help")
		rc, o, e := r.command(helpCmd, "", "", nil)
		if rc != 0 {
			return nil, agentproto.Fail("Could not get output from %s --help: %s", cmd[0], o+e)
		}
		for _, w := range strings.Fields(strings.TrimSpace(o)) {
			if w == "--no-site-packages" {
				cmd = append(cmd, "--no-site-packages")
				break
			}
		}
	}
	py := p.Str("virtualenv_python")
	if !pipIsVenvCommand(command) {
		if py == "" {
			py = targetPythonExecutable(r.env)
		}
		cmd = append(cmd, "-p"+py)
	} else if py != "" {
		return nil, agentproto.Fail("virtualenv_python should not be used when using the venv module or pyvenv as virtualenv_command")
	}
	cmd = append(cmd, venv)
	rc, o, e := r.command(cmd, chdir, "", nil)
	*out += o
	*errOut += e
	if rc != 0 {
		return nil, pipFail(cmd, *out, *errOut)
	}
	return cmd, nil
}

// pipIsVenvCommand is _is_venv_command: pyvenv, or a command whose -m
// option (as argparse reads it) is venv.
func pipIsVenvCommand(command string) bool {
	argv, err := shlexSplit(command)
	if err != nil || len(argv) == 0 {
		return false
	}
	if argv[0] == "pyvenv" {
		return true
	}
	m := ""
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--":
			i = len(argv)
		case a == "-m" && i+1 < len(argv):
			m = argv[i+1]
			i++
		case strings.HasPrefix(a, "-m="):
			m = a[3:]
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
	return pyPackageDir(env, pkg) != ""
}

// pyPackageDir is the directory a package is imported from on the task's
// Python's path ("" when none).
func pyPackageDir(env *RunEnv, pkg string) string {
	for _, d := range pySitePackages(env) {
		if isFile(filepath.Join(d, pkg, "__init__.py")) {
			return filepath.Join(d, pkg)
		}
	}
	return ""
}

// pyImportableVersion is the release (its __version__) of a package the
// task's Python imports; without a Python, the newest behavior stands
// in.
func pyImportableVersion(env *RunEnv, pkg string) (pkgFlavor, bool) {
	if !targetHasPython(env) {
		return pkgFlavorOf(""), true
	}
	dir := pyPackageDir(env, pkg)
	if dir == "" {
		return pkgFlavor{}, false
	}
	return pkgFlavorOf(pyModuleVersion(dir)), true
}

var pyDunderVersionRe = regexp.MustCompile(`(?m)^__version__\s*=\s*['"]([^'"]+)['"]`)

// pyModuleVersion reads a package's __version__ (from __init__.py or,
// as older releases kept it, __about__.py).
func pyModuleVersion(dir string) string {
	for _, f := range []string{"__init__.py", "__about__.py"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if m := pyDunderVersionRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// pkgResourcesFlavor is the fallback when packaging is missing:
// setuptools' pkg_resources, whose Requirement is its vendored
// packaging's.
func pkgResourcesFlavor(env *RunEnv) (pkgFlavor, bool) {
	dir := pyPackageDir(env, "pkg_resources")
	if dir == "" {
		return pkgFlavor{}, false
	}
	parent := filepath.Dir(dir)
	for _, d := range []string{filepath.Join(parent, "setuptools", "_vendor", "packaging"),
		filepath.Join(dir, "_vendor", "packaging")} {
		if v := pyModuleVersion(d); v != "" {
			return pkgFlavorOf(v), true
		}
	}
	return pkgFlavorOf(""), true
}

var pipVCSRe = regexp.MustCompile(`^(svn|git|hg|bzr)\+`)

// pipRecoverPackageNames is _recover_package_name: the name list
// re-split on commas and rejoined so a version specifier given as its
// own item (or after a comma) stays with its package.
func pipRecoverPackageNames(names []string) []string {
	var flat []string
	for _, line := range names {
		flat = append(flat, strings.Split(line, ",")...)
	}
	isPackageName := func(n string) bool {
		n = strings.TrimLeftFunc(n, unicode.IsSpace)
		for _, op := range []string{">=", "<=", ">", "<", "==", "!=", "~="} {
			if strings.HasPrefix(n, op) {
				return false
			}
		}
		return true
	}
	var parts, out []string
	inBrackets := false
	for _, n := range flat {
		if isPackageName(n) && !inBrackets {
			if len(parts) > 0 {
				out = append(out, strings.Join(parts, ","))
			}
			parts = nil
		}
		if strings.Contains(n, "[") {
			inBrackets = true
		}
		if inBrackets && strings.Contains(n, "]") {
			inBrackets = false
		}
		parts = append(parts, n)
	}
	return append(out, strings.Join(parts, ","))
}

// pipPackage is the module's Package: a requirement packaging could
// parse (plain), or a reference only pip understands.
type pipPackage struct {
	name string // package_name: the canonical project name, or the text
	req  *pyRequirement
	run  *pipRun
}

// newPackage is Package(name_string, version_string): a version joins
// the name with == (when it starts with a digit) or a space.
func (r *pipRun) newPackage(nameString, version string, withVersion bool) *pipPackage {
	pkg := &pipPackage{name: nameString, run: r}
	if withVersion && version != "" {
		version = strings.TrimLeftFunc(version, unicode.IsSpace)
		sep := " "
		if c, _ := utf8.DecodeRuneInString(version); unicode.IsDigit(c) {
			sep = "=="
		}
		nameString += sep + version
	}
	req, ok := parseRequirement(nameString, r.flavor)
	if !ok {
		return pkg
	}
	pkg.req = req
	project := canonicalizeName(req.name)
	if project == "distribute" && strings.Contains(nameString, "setuptools") {
		pkg.name = "setuptools"
	} else {
		pkg.name = project
	}
	return pkg
}

func (p *pipPackage) hasVersionSpecifier() bool { return p.req != nil && p.req.hasSpecifier() }

// String is str(Package): the requirement as packaging prints it, else
// the text as given.
func (p *pipPackage) String() string {
	if p.req == nil {
		return p.name
	}
	if p.run.safeExtras {
		c := *p.req
		c.extras = nil
		for _, e := range p.req.extras {
			c.extras = append(c.extras, strings.ToLower(pipSafeExtraRe.ReplaceAllString(e, "_")))
		}
		return c.String()
	}
	return p.req.String()
}

var pipSafeExtraRe = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// isPresent is _is_present: some name==version line of the package list
// names this project at a version its specifier allows (any, prereleases
// included).
func (p *pipPackage) isPresent(installed []string) (bool, error) {
	for _, line := range installed {
		n, v, ok := strings.Cut(line, "==")
		if !ok {
			continue
		}
		if canonicalizeName(n) != p.name || p.req == nil {
			continue
		}
		in, err := p.req.spec.contains(v)
		if err != nil {
			return false, err
		}
		if in {
			return true, nil
		}
	}
	return false, nil
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

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
