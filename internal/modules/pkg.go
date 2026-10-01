package modules

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(packageModule, "package", "ansible.builtin.package")
	Register(aptModule, "apt", "ansible.builtin.apt")
	// yum redirects to dnf: both run through the dnf action.
	Register(dnfActionModule, "dnf", "ansible.builtin.dnf", "yum", "ansible.builtin.yum")
	Register(dnf5Module, "dnf5", "ansible.builtin.dnf5")
	Register(apkModule, "apk", "community.general.apk")
}

// aptSpec is ansible.builtin.apt's argument spec.
var aptSpec = args.Spec{
	"state":                        {Default: "present", Choices: []string{"absent", "build-dep", "fixed", "latest", "present"}},
	"update_cache":                 {Type: "bool", Aliases: []string{"update-cache"}},
	"update_cache_retries":         {Type: "int", Default: 5},
	"update_cache_retry_max_delay": {Type: "int", Default: 12},
	"cache_valid_time":             {Type: "int", Default: 0},
	"purge":                        {Type: "bool", Default: false},
	"package":                      {Type: "list", Aliases: []string{"pkg", "name"}},
	"deb":                          {},
	"default_release":              {Aliases: []string{"default-release"}},
	"install_recommends":           {Type: "bool", Aliases: []string{"install-recommends"}},
	"force":                        {Type: "bool", Default: false},
	"upgrade":                      {Default: "no", Choices: []string{"dist", "full", "no", "safe", "yes"}},
	"dpkg_options":                 {Default: "force-confdef,force-confold"},
	"autoremove":                   {Type: "bool", Default: false},
	"autoclean":                    {Type: "bool", Default: false},
	"fail_on_autoremove":           {Type: "bool", Default: false},
	"policy_rc_d":                  {Type: "int"},
	"only_upgrade":                 {Type: "bool", Default: false},
	"force_apt_get":                {Type: "bool", Default: false},
	"clean":                        {Type: "bool", Default: false},
	"allow_unauthenticated":        {Type: "bool", Default: false, Aliases: []string{"allow-unauthenticated"}},
	"allow_downgrade":              {Type: "bool", Default: false, Aliases: []string{"allow-downgrade", "allow_downgrades", "allow-downgrades"}},
	"allow_change_held_packages":   {Type: "bool", Default: false},
	"lock_timeout":                 {Type: "int", Default: 60},
	// Installing python3-apt is moot without Python; accepted for
	// compatibility.
	"auto_install_module_deps": {Type: "bool", Default: true},
}

// yumdnfSpec is module_utils.yumdnf's yumdnf_argument_spec, shared by the
// dnf and dnf5 modules.
func yumdnfSpec(extra args.Spec) args.Spec {
	s := args.Spec{
		"allow_downgrade":   {Type: "bool", Default: false},
		"allowerasing":      {Type: "bool", Default: false},
		"autoremove":        {Type: "bool", Default: false},
		"best":              {Type: "bool"},
		"bugfix":            {Type: "bool", Default: false},
		"cacheonly":         {Type: "bool", Default: false},
		"conf_file":         {},
		"disable_excludes":  {},
		"disable_gpg_check": {Type: "bool", Default: false},
		"disable_plugin":    {Type: "list", Default: []any{}},
		"disablerepo":       {Type: "list", Default: []any{}},
		"download_only":     {Type: "bool", Default: false},
		"download_dir":      {},
		"enable_plugin":     {Type: "list", Default: []any{}},
		"enablerepo":        {Type: "list", Default: []any{}},
		"exclude":           {Type: "list", Default: []any{}},
		"installroot":       {Default: "/"},
		"install_weak_deps": {Type: "bool", Default: true},
		"list":              {},
		"name":              {Type: "list", Aliases: []string{"pkg"}, Default: []any{}},
		"nobest":            {Type: "bool"},
		"releasever":        {},
		"security":          {Type: "bool", Default: false},
		"skip_broken":       {Type: "bool", Default: false},
		// removed==absent, installed==present; no default: autoremove
		// alone means absent.
		"state":          {Choices: []string{"absent", "installed", "latest", "present", "removed"}},
		"update_cache":   {Type: "bool", Default: false, Aliases: []string{"expire-cache"}},
		"update_only":    {Type: "bool", Default: false},
		"validate_certs": {Type: "bool", Default: true},
		"sslverify":      {Type: "bool", Default: true},
		"lock_timeout":   {Type: "int", Default: 30},
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// dnfSpec is ansible.builtin.dnf's (and so yum's) argument spec.
var dnfSpec = yumdnfSpec(args.Spec{
	"use_backend": {Default: "auto", Choices: []string{"auto", "dnf", "yum", "yum4", "dnf4", "dnf5"}},
})

// dnf5Spec is ansible.builtin.dnf5's argument spec.
var dnf5Spec = yumdnfSpec(args.Spec{
	"auto_install_module_deps": {Type: "bool", Default: true},
})

// packageSpec is ansible.builtin.package's documented options; the action
// hands everything but use to the package manager's module.
var packageSpec = args.Spec{
	"name":  {Required: true, Type: "any"},
	"state": {Required: true},
	"use":   {Default: "auto"},
}

// Hidden arguments the package actions pass their module.
const (
	// pkgMgrFactKey carries the host's ansible_facts.pkg_mgr, when set.
	pkgMgrFactKey = "_understudy_pkg_mgr"
	// pkgUseVarKey carries the ansible_package_use variable (package).
	pkgUseVarKey = "_understudy_package_use"
	// pkgReportFactKey asks the dnf action to report a pkg_mgr it had
	// to detect as ansible_facts, as the dnf action plugin does.
	pkgReportFactKey = "_understudy_report_pkg_mgr"
)

// takeHidden removes a hidden argument from raw, returning its value.
func takeHidden(raw map[string]any, key string) any {
	v := raw[key]
	delete(raw, key)
	return v
}

func copyRaw(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	return out
}

// detectPkgMgrFact is the pkg_mgr fact setup would report (gather_subset
// !all, filter ansible_pkg_mgr).
func detectPkgMgrFact(env *RunEnv) string {
	facts, err := gatherFacts(newFactEnv("/etc/ansible/facts.d", 10*time.Second), []string{"!all"}, []string{"ansible_pkg_mgr"})
	if err != nil {
		return "auto"
	}
	if s, ok := facts["ansible_pkg_mgr"].(string); ok {
		return s
	}
	return "auto"
}

// aptModule is ansible.builtin.apt.
func aptModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	return aptModuleAs(env, rawArgs, "apt")
}

func aptModuleAs(env *RunEnv, rawArgs map[string]any, name string) *agentproto.Result {
	raw := copyRaw(rawArgs)
	takeHidden(raw, pkgMgrFactKey)
	p, fail := parseModuleArgs(aptSpec, raw, name)
	if fail != nil {
		return fail
	}
	if aptBindingsMissing(env) {
		if fail := aptInstallBindings(env, p); fail != nil {
			return fail
		}
	}
	return runPkg(env, pkgManagers["apt"], p, raw, "")
}

// aptBindingInterpreters are the system Pythons the apt module probes for
// python3-apt.
var aptBindingInterpreters = []any{"/usr/bin/python3", "/usr/bin/python"}

// aptInstallBindings is the apt module without python3-apt: check mode
// fails; with auto_install_module_deps it updates the cache (unless
// update_cache is false) and installs python3-apt with apt-get, then
// carries on under the Python that can import it (the warnings it gave
// stay with the process it replaced); otherwise, or if the bindings
// are still missing, it fails naming the Python and its sys.version.
func aptInstallBindings(env *RunEnv, p *args.Parsed) *agentproto.Result {
	if env.CheckMode {
		return agentproto.Fail("python3-apt must be installed to use check mode. " +
			"If run normally this module can auto-install it, see the auto_install_module_deps option.")
	}
	var warnings []any
	withWarnings := func(res *agentproto.Result) *agentproto.Result {
		if len(warnings) > 0 {
			if res.Extra == nil {
				res.Extra = map[string]any{}
			}
			res.Extra["warnings"] = warnings
		}
		return res
	}
	if p.Bool("auto_install_module_deps") {
		aptGet := aptGetPath()
		run := func(argv ...string) *agentproto.Result {
			rc, out, errOut := runAptCmd(env, argv...)
			if rc == 0 {
				return nil
			}
			quoted := make([]string, len(argv))
			for i, a := range argv {
				quoted[i] = shlexQuote(a)
			}
			return withWarnings(&agentproto.Result{Failed: true, Msg: heuristicLogSanitize(strings.TrimRight(errOut, " \t\r\n\v\f")),
				RC: agentproto.IntPtr(rc), Stdout: out, Stderr: errOut, Extra: map[string]any{"cmd": strings.Join(quoted, " ")}})
		}
		if p.Has("update_cache") && !p.Bool("update_cache") {
			warnings = append(warnings, "Auto-installing missing dependency without updating cache: python3-apt")
		} else {
			warnings = append(warnings, "Updating cache and auto-installing missing dependency: python3-apt")
			if fail := run(aptGet, "update"); fail != nil {
				return fail
			}
		}
		argv := []string{aptGet, "install", "python3-apt", "-y", "-q", aptDpkgOptions(p)}
		if p.Has("install_recommends") {
			if p.Bool("install_recommends") {
				argv = append(argv, "-o", "APT::Install-Recommends=yes")
			} else {
				argv = append(argv, "-o", "APT::Install-Recommends=no")
			}
		}
		if fail := run(argv...); fail != nil {
			return fail
		}
		if !aptBindingsMissing(env) {
			return nil
		}
	}
	return withWarnings(agentproto.Fail("Could not import the python3-apt module using %s (%s). "+
		"Ensure python3-apt package is installed (either manually or via the auto_install_module_deps option) "+
		"or that you have specified the correct ansible_python_interpreter. (attempted %s).",
		targetPythonExecutable(env), pySysVersionMessage(env), pyReprValue(aptBindingInterpreters)))
}

// aptBindingsMissing reports whether the apt modules would find no
// python3-apt: the task has a Python, and neither it nor the system
// Pythons can import apt. (Without any Python, understudy's native code
// stands in for the bindings.)
func aptBindingsMissing(env *RunEnv) bool {
	return targetHasPython(env) && !pyModuleInstalled(env, "apt")
}

// dnfActionModule is the dnf action plugin (yum redirects to it): the
// backend comes from use_backend (or use), else the pkg_mgr fact, else
// setup's detection, which the result then reports as ansible_facts.
func dnfActionModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	raw := copyRaw(rawArgs)
	fact, _ := takeHidden(raw, pkgMgrFactKey).(string)
	report, _ := takeHidden(raw, pkgReportFactKey).(bool)
	_, hasUse := raw["use"]
	_, hasBackend := raw["use_backend"]
	if hasUse && hasBackend {
		return agentproto.Fail("parameters are mutually exclusive: ('use', 'use_backend')")
	}
	module := "auto"
	if v, ok := raw["use"]; ok {
		module = fmt.Sprint(v)
	} else if v, ok := raw["use_backend"]; ok {
		module = fmt.Sprint(v)
	}
	if (module == "yum" || module == "auto") && fact != "" {
		module = fact
	}
	var facts map[string]any
	valid := map[string]bool{"yum": true, "yum4": true, "dnf": true, "dnf4": true, "dnf5": true}
	if !valid[module] {
		module = detectPkgMgrFact(env)
		if report && module != "auto" {
			facts = map[string]any{"pkg_mgr": module}
		}
	}
	if !valid[module] {
		// The action's msg is a tuple: the result shows it as a list, the
		// error its str().
		msg := []any{
			"Could not detect which major revision of dnf is in use, which is required to determine module backend.",
			"You should manually specify use_backend to tell the module whether to use the dnf4 or dnf5 backend})"}
		return &agentproto.Result{Failed: true, AnsibleFacts: facts, Origin: "action",
			Msg:   "(" + pyStrRepr(msg[0].(string)) + ", " + pyStrRepr(msg[1].(string)) + ")",
			Extra: map[string]any{"msg": msg}}
	}
	if module == "yum" {
		// ansible.legacy.yum is only a redirect to the dnf action.
		return &agentproto.Result{Failed: true, AnsibleFacts: facts, Origin: "action",
			Msg: "Could not find a dnf module backend for ansible.legacy.yum."}
	}
	delete(raw, "use")
	delete(raw, "use_backend")
	var res *agentproto.Result
	if module == "dnf5" {
		res = dnfModuleAs(env, raw, "dnf5", "ansible.legacy.dnf5")
	} else {
		res = dnfModuleAs(env, raw, "dnf", "ansible.legacy.dnf")
	}
	if facts != nil && res != nil {
		if res.AnsibleFacts == nil {
			res.AnsibleFacts = map[string]any{}
		}
		for k, v := range facts {
			if _, set := res.AnsibleFacts[k]; !set {
				res.AnsibleFacts[k] = v
			}
		}
	}
	return res
}

// dnf5Module is ansible.builtin.dnf5 run directly.
func dnf5Module(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	raw := copyRaw(rawArgs)
	takeHidden(raw, pkgMgrFactKey)
	return dnfModuleAs(env, raw, "dnf5", "dnf5")
}

// dnfModuleAs runs the dnf (dnf4) or dnf5 module, named as invoked.
func dnfModuleAs(env *RunEnv, raw map[string]any, backend, name string) *agentproto.Result {
	spec := dnfSpec
	if backend == "dnf5" {
		spec = dnf5Spec
	}
	p, fail := parseModuleArgs(spec, raw, name)
	if fail != nil {
		return fail
	}
	if err := spec.MutuallyExclusive(raw, []string{"name", "list"}, []string{"best", "nobest"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	if fail := dnfBindings(env, p, backend); fail != nil {
		return fail
	}
	mgr := rpmManager("dnf")
	if backend == "dnf5" {
		mgr = rpmManager("dnf5")
	}
	return runPkg(env, mgr, p, raw, backend)
}

// dnfBindings is the modules' check (in their constructors) that some
// Python can import their bindings: dnf's probes the task's Python and
// the system ones for the dnf package; dnf5 looks for libdnf5 in the
// system Pythons and, outside check mode, first installs
// python3-libdnf5 (auto_install_module_deps).
func dnfBindings(env *RunEnv, p *args.Parsed, backend string) *agentproto.Result {
	if backend != "dnf5" {
		if pyModuleInstalled(env, "dnf") {
			return nil
		}
		attempted := []any{targetPythonExecutable(env), "/usr/libexec/platform-python", "/usr/bin/python3", "/usr/bin/python"}
		return &agentproto.Result{Failed: true,
			Msg:   "Could not import the dnf python module. Please install `python3-dnf` package. (attempted " + pyReprValue(attempted) + ")",
			Extra: map[string]any{"results": []any{}}}
	}
	// Without any Python, understudy's native code stands in.
	if !targetHasPython(env) || pyModuleInstalled(env, "libdnf5") {
		return nil
	}
	switch {
	case env.CheckMode:
		return agentproto.Fail("python3-libdnf5 must be installed to use check mode. " +
			"If run normally this module can auto-install it, see the auto_install_module_deps option.")
	case p.Bool("auto_install_module_deps"):
		argv := []string{"dnf", "install", "-y", "python3-libdnf5"}
		if _, err := lookPath("dnf"); err != nil {
			return &agentproto.Result{Failed: true, Msg: "Error executing command.", RC: agentproto.IntPtr(2),
				Cause: "[Errno 2] No such file or directory: b'dnf'",
				Extra: map[string]any{"cmd": strings.Join(argv, " ")}}
		}
		rc, out, errOut := runCommand(env, argv, cmdOpts{Env: localeEnv(bestParsableLocale(env))})
		if rc != 0 {
			return &agentproto.Result{Failed: true, Msg: heuristicLogSanitize(strings.TrimRight(errOut, " \t\r\n\v\f")),
				RC: agentproto.IntPtr(rc), Stdout: out, Stderr: errOut, Extra: map[string]any{"cmd": strings.Join(argv, " ")}}
		}
		// The module then carries on under the Python that can import
		// the bindings it installed.
		if pyModuleInstalled(env, "libdnf5") {
			return nil
		}
	}
	return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Could not import the libdnf5 python module using %s (%s). "+
		"Ensure python3-libdnf5 package is installed (either manually or via the auto_install_module_deps option) "+
		"or that you have specified the correct ansible_python_interpreter. (attempted %s).",
		targetPythonExecutable(env), pySysVersionMessage(env), pyReprValue(dnf5BindingInterpreters)),
		Extra: map[string]any{"failures": []any{}}}
}

// dnf5BindingInterpreters are the system Pythons the dnf5 module probes
// for libdnf5.
var dnf5BindingInterpreters = []any{"/usr/libexec/platform-python", "/usr/bin/python3", "/usr/bin/python"}

// pyModuleInstalled reports whether a Python package (a directory with
// an __init__.py) is importable from the task's Python (env non-nil) or
// the system Pythons' site directories.
func pyModuleInstalled(env *RunEnv, pkg string) bool {
	var dirs []string
	if env != nil {
		dirs = pySitePackages(env)
	}
	for _, pat := range []string{"/usr/lib/python3*/site-packages", "/usr/lib64/python3*/site-packages",
		"/usr/lib/python3/dist-packages", "/usr/local/lib/python3*/site-packages"} {
		m, _ := filepath.Glob(pat)
		dirs = append(dirs, m...)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, pkg, "__init__.py")); err == nil {
			return true
		}
	}
	return false
}

// packageModule is the package action: use (or ansible_package_use, else
// the pkg_mgr fact, else detection) names the module the other arguments
// go to, run as ansible.legacy.<module>.
func packageModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	raw := copyRaw(rawArgs)
	fact, _ := takeHidden(raw, pkgMgrFactKey).(string)
	useVar, _ := takeHidden(raw, pkgUseVarKey).(string)
	module := "auto"
	if v, ok := raw["use"]; ok {
		module = fmt.Sprint(v)
	}
	delete(raw, "use")
	if module == "auto" {
		module = useVar
		if module == "" {
			module = fact
		}
		if module == "" {
			module = detectPkgMgrFact(env)
		}
	}
	if module == "" || module == "auto" {
		return agentproto.Fail("Could not detect which package manager to use. Try gathering facts or setting the \"use\" option.")
	}
	short := module[strings.LastIndexByte(module, '.')+1:]
	switch short {
	case "apt":
		return aptModuleAs(env, raw, "ansible.legacy.apt")
	case "dnf", "yum":
		// The package action runs the dnf module itself, not the action.
		return dnfModuleAs(env, raw, "dnf", "ansible.legacy."+short)
	case "dnf5":
		return dnfModuleAs(env, raw, "dnf5", "ansible.legacy.dnf5")
	case "apk":
		return apkModuleAs(env, raw, "ansible.legacy.apk")
	}
	return agentproto.Fail("Could not find a matching action for the \"%s\" package manager.", module)
}

// pkgManager abstracts one package manager's query and mutate commands.
// opts are manager-specific CLI flags derived from the module options.
type pkgManager struct {
	name      string
	installed func(env *RunEnv, pkg string) bool
	install   func(env *RunEnv, pkgs []string, latest bool, opts []string) (string, error)
	remove    func(env *RunEnv, pkgs []string, opts []string) (string, error)
	refresh   func(env *RunEnv, opts []string) (string, error)
}

var pkgManagers = map[string]*pkgManager{
	"apt": {
		name: "apt",
		installed: func(env *RunEnv, pkg string) bool {
			out, err := runOut(env, "dpkg-query", "-W", "-f=${Status}", pkg)
			return err == nil && strings.Contains(out, "install ok installed")
		},
		install: func(env *RunEnv, pkgs []string, latest bool, opts []string) (string, error) {
			argv := append(append([]string{"install", "-y"}, opts...), pkgs...)
			return runAptGet(env, argv...)
		},
		remove: func(env *RunEnv, pkgs []string, opts []string) (string, error) {
			argv := append(append([]string{"remove", "-y"}, opts...), pkgs...)
			return runAptGet(env, argv...)
		},
		refresh: func(env *RunEnv, _ []string) (string, error) {
			return runAptGet(env, "update")
		},
	},
	"dnf": rpmManager("dnf"),
	"yum": rpmManager("yum"),
}

func rpmManager(cmd string) *pkgManager {
	return &pkgManager{
		name: cmd,
		installed: func(env *RunEnv, pkg string) bool {
			// --whatprovides resolves provides aliases and file paths
			// (libselinux-python3 -> python3-libselinux), as dnf does.
			_, err := runOut(env, "rpm", "-q", "--whatprovides", pkg)
			return err == nil
		},
		install: func(env *RunEnv, pkgs []string, latest bool, opts []string) (string, error) {
			return runOut(env, cmd, append(append([]string{"install", "-y"}, opts...), pkgs...)...)
		},
		remove: func(env *RunEnv, pkgs []string, opts []string) (string, error) {
			return runOut(env, cmd, append(append([]string{"remove", "-y"}, opts...), pkgs...)...)
		},
		refresh: func(env *RunEnv, opts []string) (string, error) {
			return runOut(env, cmd, append([]string{"makecache"}, opts...)...)
		},
	}
}

// runPkg installs, removes or upgrades packages with one manager; backend
// is the dnf module flavor ("dnf", "dnf5") for rpm managers, "" for apt.
func runPkg(env *RunEnv, mgr *pkgManager, p *args.Parsed, rawArgs map[string]any, backend string) *agentproto.Result {
	if _, err := exec.LookPath(pkgBinary(mgr.name)); err != nil {
		return agentproto.Fail("package manager %q is not available on this host", mgr.name)
	}
	opts, err := pkgOptions(mgr.name, p, rawArgs, rpmRepoMatcher(env, mgr.name))
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	// dnf/apt only lock the final transaction, not their download
	// cache: concurrent package tasks (parallel blocks, async jobs)
	// must not overlap. The lock is cross-process (each agent call is
	// its own process).
	if !env.CheckMode {
		unlock := lockPackageManager()
		defer unlock()
	}

	nameKey := "name"
	if mgr.name == "apt" {
		nameKey = "package"
	}
	var names []string
	for _, n := range p.List(nameKey) {
		s, ok := n.(string)
		if !ok {
			s = pyStrValue(n)
		}
		names = append(names, s)
	}
	if backend != "" {
		return runDnf(env, mgr, p, rawArgs, names, opts, backend)
	}

	state := p.Str("state")
	switch state {
	case "installed":
		state = "present"
	case "removed":
		state = "absent"
	}

	res := &agentproto.Result{Extra: map[string]any{}}
	apt := mgr.name == "apt"
	upgradeMode := p.Str("upgrade")
	if upgradeMode == "no" {
		upgradeMode = ""
	}
	deb := pyExpandPath(p.Str("deb"))
	if apt {
		present := 0
		for _, group := range [][]string{{"deb"}, {"name", "pkg", "package"}, {"upgrade"}} {
			for _, k := range group {
				if v, ok := rawArgs[k]; ok && v != nil {
					present++
					break
				}
			}
		}
		if present > 1 {
			return agentproto.Fail("parameters are mutually exclusive: deb|package|upgrade")
		}
		if p.Bool("clean") {
			rc, out, errOut := runAptCmd(env, "apt-get", "clean")
			if rc != 0 {
				return &agentproto.Result{Failed: true, Msg: "apt-get clean failed", Stdout: out, RC: agentproto.IntPtr(rc)}
			}
			if errOut != "" {
				return &agentproto.Result{Failed: true, Msg: "apt-get clean failed: " + errOut, Stdout: out, RC: agentproto.IntPtr(rc)}
			}
			if len(names) == 0 && upgradeMode == "" && deb == "" {
				return aptOutput(true, &out, out, errOut)
			}
		}
	}
	if !apt && p.Has("list") {
		return dnfList(env, mgr.name, opts.repo, p.Str("list"))
	}

	wantUpdate := p.Bool("update_cache") || (apt && p.Int("cache_valid_time") > 0)
	if wantUpdate && apt {
		// ansible.builtin.apt: refresh unless the cache is younger than
		// cache_valid_time; with nothing else to do, changed reports
		// whether the cache was refreshed.
		before := aptCacheMtime()
		updated := false
		if !aptCacheFresh(mgr.name, int(p.Int("cache_valid_time"))) {
			if !env.CheckMode {
				if fail := aptUpdateWithRetries(env, mgr, opts.repo, p); fail != nil {
					return fail
				}
			}
			after := aptCacheMtime()
			updated = env.CheckMode || after != before
			before = after
		}
		res.Extra["cache_updated"] = updated
		res.Extra["cache_update_time"] = before
		if len(names) == 0 && upgradeMode == "" && deb == "" {
			res.Changed = updated
			return res
		}
	} else if p.Bool("update_cache") {
		if !env.CheckMode {
			if out, err := mgr.refresh(env, opts.repo); err != nil {
				return agentproto.Fail("cache update failed: %v: %s", err, tail(out))
			}
		}
		res.Extra["cache_updated"] = true
		if len(names) == 0 {
			return res
		}
	}
	if apt {
		// install() results always carry the cache state.
		if _, ok := res.Extra["cache_updated"]; !ok {
			res.Extra["cache_updated"] = false
			res.Extra["cache_update_time"] = aptCacheMtime()
		}
		if upgradeMode != "" {
			return aptUpgrade(env, p, upgradeMode)
		}
		if deb != "" {
			if state != "present" {
				return agentproto.Fail("deb only supports state=present")
			}
			return aptInstallDeb(env, p, deb)
		}
		var filtered []string
		all := false
		for _, n := range names {
			if n == "*" {
				all = true
				continue
			}
			filtered = append(filtered, strings.TrimSpace(n))
		}
		names = filtered
		if state == "latest" && all {
			if len(names) > 0 {
				return agentproto.Fail("unable to install additional packages when upgrading all installed packages")
			}
			return aptUpgrade(env, p, "yes")
		}
		for _, n := range names {
			if strings.Count(n, "=") > 1 {
				return agentproto.Fail("invalid package spec: %s", n)
			}
		}
		if len(names) == 0 {
			if p.Bool("autoclean") {
				return aptCleanup(env, p, "autoclean")
			}
			if p.Bool("autoremove") {
				return aptCleanup(env, p, "autoremove")
			}
		}
		if state == "build-dep" || state == "fixed" {
			r := aptBuildDepOrFixed(env, p, names, state)
			if r.Extra == nil {
				r.Extra = map[string]any{}
			}
			r.Extra["cache_updated"] = res.Extra["cache_updated"]
			r.Extra["cache_update_time"] = res.Extra["cache_update_time"]
			return r
		}
	} else if state == "build-dep" || state == "fixed" {
		return agentproto.Fail("state=%s is only supported by apt", state)
	}
	if len(names) == 0 {
		return agentproto.Fail("'name' is required (or update_cache alone)")
	}
	onlyInstalled := (apt && p.Bool("only_upgrade")) || (!apt && state == "latest" && p.Bool("update_only"))

	switch state {
	case "present", "latest":
		var missing []string
		for _, pkg := range names {
			if !mgr.installed(env, pkg) {
				missing = append(missing, pkg)
			}
		}
		targets := missing
		if state == "latest" {
			targets = names // latest always runs the install command
		}
		var skipped []any
		if onlyInstalled {
			// only_upgrade / update_only: packages that are not
			// installed are skipped, the rest only upgraded.
			var keep []string
			for _, t := range targets {
				if !containsStr(missing, t) {
					keep = append(keep, t)
				} else if !apt {
					skipped = append(skipped, fmt.Sprintf("Packages providing %s not installed due to update_only specified", t))
				}
			}
			targets = keep
		}
		if len(targets) == 0 {
			if skipped != nil {
				// The dnf module's no-op result.
				res.Msg = "Nothing to do"
				res.Extra["results"] = skipped
			}
			return res
		}
		if env.CheckMode {
			res.Changed = len(missing) > 0 || state == "latest"
			res.Extra["would_install"] = targets
			return res
		}
		if apt {
			argv := append(append([]string{aptGetPath(), "install", "-y"}, opts.install...), targets...)
			return aptRunResult(env, p, res, argv, true)
		}
		var out string
		err := withPolicyRcD(apt, p, func() error {
			var e error
			out, e = mgr.install(env, targets, state == "latest", opts.install)
			return e
		})
		if err != nil {
			return agentproto.Fail("package install failed: %v: %s", err, tail(out))
		}
		// The transaction decides: names that only resolve through dnf
		// (groups, modules) can look missing and still be "Nothing to do".
		res.Changed = pkgOutputShowsChange(mgr.name, out)
	case "absent":
		// remove() exits without the cache state.
		delete(res.Extra, "cache_updated")
		delete(res.Extra, "cache_update_time")
		var present []string
		for _, pkg := range names {
			if mgr.installed(env, pkg) {
				present = append(present, pkg)
			}
		}
		if len(present) == 0 {
			return res
		}
		if env.CheckMode {
			res.Changed = true
			res.Extra["would_remove"] = present
			return res
		}
		if apt {
			argv := append(append([]string{aptGetPath(), "remove", "-y"}, opts.remove...), present...)
			return aptRunResult(env, p, res, argv, false)
		}
		var out string
		err := withPolicyRcD(apt, p, func() error {
			var e error
			out, e = mgr.remove(env, present, opts.remove)
			return e
		})
		if err != nil {
			return agentproto.Fail("package removal failed: %v: %s", err, tail(out))
		}
		res.Changed = true
	}
	return res
}

func pkgBinary(name string) string {
	if name == "apt" {
		return "apt-get"
	}
	return name
}

// pkgOutputShowsChange detects "already newest" no-ops in latest mode.
func pkgOutputShowsChange(mgr, out string) bool {
	switch mgr {
	case "apt":
		return !strings.Contains(out, "0 upgraded, 0 newly installed")
	case "dnf", "yum":
		return !strings.Contains(out, "Nothing to do")
	case "apk":
		return strings.Contains(out, "Installing") || strings.Contains(out, "Upgrading")
	}
	return true
}

func runOut(env *RunEnv, name string, argv ...string) (string, error) {
	path, err := lookPath(name)
	if err != nil {
		return "", err
	}
	cmd := env.Command(path, argv...)
	applyEnv(cmd, env)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

// runAptGet wraps apt-get with the noninteractive frontend.
func runAptGet(env *RunEnv, argv ...string) (string, error) {
	cmd := env.Command("apt-get", argv...)
	applyEnv(cmd, env)
	if cmd.Env == nil {
		cmd.Env = append(cmd.Env, environWith("DEBIAN_FRONTEND", "noninteractive")...)
	} else {
		cmd.Env = append(cmd.Env, "DEBIAN_FRONTEND=noninteractive")
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return s
}

func environWith(k, v string) []string {
	return append(osEnviron(), fmt.Sprintf("%s=%s", k, v))
}

// rpmRepoMatcher lists the configured repo ids once (dnf repolist --all)
// and reports whether a glob pattern matches any of them. nil for non-rpm
// managers or when the list cannot be read (patterns pass through).
func rpmRepoMatcher(env *RunEnv, mgr string) func(string) bool {
	if mgr != "dnf" && mgr != "yum" && mgr != "dnf5" {
		return nil
	}
	var ids []string
	loaded := false
	return func(pattern string) bool {
		if !loaded {
			loaded = true
			out, err := runOut(env, pkgBinary(mgr), "-q", "repolist", "--all")
			if err != nil {
				ids = nil
			} else {
				for _, line := range strings.Split(out, "\n") {
					f := strings.Fields(line)
					if len(f) == 0 || (len(f) > 1 && f[0] == "repo" && f[1] == "id") {
						continue
					}
					ids = append(ids, f[0])
				}
			}
		}
		if ids == nil {
			return true
		}
		for _, id := range ids {
			if ok, _ := path.Match(pattern, id); ok {
				return true
			}
		}
		return false
	}
}

type pkgOpts struct {
	repo, install, remove []string
}

// pkgOptions maps module options onto the manager's CLI flags (each
// module's argument spec admits only its own options).
func pkgOptions(mgr string, p *args.Parsed, raw map[string]any, repoKnown func(string) bool) (pkgOpts, error) {
	var o pkgOpts
	rpm := mgr == "dnf" || mgr == "yum" || mgr == "dnf5"
	if rpm {
		// The dnf module enables/disables base.repos.get_matching(pattern):
		// a pattern matching no configured repo is a no-op, where the dnf
		// CLI would fail with "Unknown repo".
		for _, r := range p.List("enablerepo") {
			if repoKnown == nil || repoKnown(fmt.Sprint(r)) {
				o.repo = append(o.repo, "--enablerepo="+fmt.Sprint(r))
			}
		}
		for _, r := range p.List("disablerepo") {
			if repoKnown == nil || repoKnown(fmt.Sprint(r)) {
				o.repo = append(o.repo, "--disablerepo="+fmt.Sprint(r))
			}
		}
		for _, x := range p.List("exclude") {
			o.repo = append(o.repo, "--exclude="+fmt.Sprint(x))
		}
		for flag, key := range map[string]string{"--config=": "conf_file", "--releasever=": "releasever",
			"--installroot=": "installroot", "--disableexcludes=": "disable_excludes"} {
			if v := p.Str(key); v != "" {
				o.repo = append(o.repo, flag+v)
			}
		}
		if !p.Bool("sslverify") || !p.Bool("validate_certs") {
			o.repo = append(o.repo, "--setopt=sslverify=False")
		}
		if p.Bool("cacheonly") {
			o.repo = append(o.repo, "--cacheonly")
		}
		for _, x := range p.List("disable_plugin") {
			o.repo = append(o.repo, "--disableplugin="+fmt.Sprint(x))
		}
		for _, x := range p.List("enable_plugin") {
			o.repo = append(o.repo, "--enableplugin="+fmt.Sprint(x))
		}
		o.install = append([]string(nil), o.repo...)
		if p.Bool("disable_gpg_check") {
			o.install = append(o.install, "--nogpgcheck")
		}
		if p.Bool("skip_broken") {
			o.install = append(o.install, "--skip-broken")
		}
		if p.Bool("allowerasing") {
			o.install = append(o.install, "--allowerasing")
		}
		if p.Bool("nobest") {
			o.install = append(o.install, "--nobest")
		} else if _, set := raw["nobest"]; !set && p.Has("best") {
			// nobest wins over best, as in the dnf module.
			o.install = append(o.install, fmt.Sprintf("--setopt=best=%v", pyBool(p.Bool("best"))))
		}
		if !p.Bool("install_weak_deps") {
			o.install = append(o.install, "--setopt=install_weak_deps=False")
		}
		if p.Bool("security") {
			o.install = append(o.install, "--security")
		}
		if p.Bool("bugfix") {
			o.install = append(o.install, "--bugfix")
		}
		if p.Bool("download_only") {
			o.install = append(o.install, "--downloadonly")
			if d := p.Str("download_dir"); d != "" {
				o.install = append(o.install, "--downloaddir="+d)
			}
		}
		o.remove = append([]string(nil), o.repo...)
		if !p.Bool("autoremove") {
			o.remove = append(o.remove, "--setopt=clean_requirements_on_remove=False")
		}
		return o, nil
	}
	if mgr == "apt" {
		// expand_dpkg_options: every install/remove passes the dpkg
		// options (default force-confdef,force-confold), so a conffile a
		// role templated before installing never prompts.
		for _, opt := range strings.Split(p.Str("dpkg_options"), ",") {
			if opt = strings.TrimSpace(opt); opt != "" {
				o.install = append(o.install, "-o", "Dpkg::Options::=--"+opt)
				o.remove = append(o.remove, "-o", "Dpkg::Options::=--"+opt)
			}
		}
		if _, set := raw["install_recommends"]; set {
			if p.Bool("install_recommends") {
				o.install = append(o.install, "--install-recommends")
			} else {
				o.install = append(o.install, "--no-install-recommends")
			}
		}
		if p.Bool("autoremove") {
			o.remove = append(o.remove, "--auto-remove")
		}
		// apt module: -t <default_release> on installs, --purge on removal.
		if rel := p.Str("default_release"); rel != "" {
			o.install = append(o.install, "-t", rel)
		}
		if p.Bool("purge") {
			o.remove = append(o.remove, "--purge")
		}
		if p.Bool("allow_downgrade") {
			o.install = append(o.install, "--allow-downgrades")
		}
		// apt module install/remove flags.
		if p.Bool("force") {
			o.install = append(o.install, "--force-yes")
			o.remove = append(o.remove, "--force-yes")
		}
		if p.Bool("autoremove") {
			o.install = append(o.install, "--auto-remove")
		}
		if p.Bool("fail_on_autoremove") {
			o.install = append(o.install, "--no-remove")
		}
		if p.Bool("only_upgrade") {
			o.install = append(o.install, "--only-upgrade")
		}
		if p.Bool("allow_unauthenticated") {
			o.install = append(o.install, "--allow-unauthenticated")
		}
		if p.Bool("allow_change_held_packages") {
			o.install = append(o.install, "--allow-change-held-packages")
			o.remove = append(o.remove, "--allow-change-held-packages")
		}
	}
	return o, nil
}

// aptCacheFresh reports whether apt's cache is younger than validSecs
// (cache_valid_time), letting update_cache skip the refresh.
// aptCacheMtime is the apt module's get_cache_mtime as an integer
// timestamp (0 when there is no cache).
func aptCacheMtime() int64 {
	for _, stamp := range []string{"/var/lib/apt/periodic/update-success-stamp", "/var/lib/apt/lists"} {
		if info, err := os.Stat(stamp); err == nil {
			return info.ModTime().Unix()
		}
	}
	return 0
}

func aptCacheFresh(mgr string, validSecs int) bool {
	if mgr != "apt" || validSecs <= 0 {
		return false
	}
	for _, stamp := range []string{"/var/lib/apt/periodic/update-success-stamp", "/var/lib/apt/lists"} {
		if info, err := os.Stat(stamp); err == nil {
			return time.Since(info.ModTime()) < time.Duration(validSecs)*time.Second
		}
	}
	return false
}
