package modules

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// apkModule ports community.general.apk.

var apkSpec = args.Spec{
	"state":        {Default: "present", Choices: []string{"present", "installed", "absent", "removed", "latest"}},
	"name":         {Type: "list"},
	"no_cache":     {Type: "bool", Default: false},
	"repository":   {Type: "list"},
	"update_cache": {Type: "bool", Default: false},
	"upgrade":      {Type: "bool", Default: false},
	"available":    {Type: "bool", Default: false},
	"world":        {Default: "/etc/apk/world"},
}

var apkPackageLine = regexp.MustCompile(`^\(\s*\d+/\d+\)\s+\S+\s+(\S+)`)

func apkParsePackages(stdout string) []any {
	packages := []any{}
	for _, l := range strings.Split(stdout, "\n") {
		if m := apkPackageLine.FindStringSubmatch(l); m != nil {
			packages = append(packages, m[1])
		}
	}
	return packages
}

type apkRun struct {
	env *RunEnv
	cmd []string // APK_PATH: apk plus global options
}

func (a *apkRun) run(argv ...string) (int, string, string) {
	return runCommand(a.env, append(append([]string{}, a.cmd...), argv...), cmdOpts{Env: map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}})
}

func apkResult(failed, changed bool, msg, stdout, stderr string, packages []any) *agentproto.Result {
	res := &agentproto.Result{Failed: failed, Changed: changed, Msg: msg, Stdout: stdout, Stderr: stderr,
		Extra: map[string]any{}}
	setOutputLines(res, stdout, stderr)
	if packages != nil {
		res.Extra["packages"] = packages
	}
	return res
}

func apkModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := apkSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if err := apkSpec.MutuallyExclusive(rawArgs, []string{"name", "upgrade"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	bin, err := getBinPath("apk")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	var names []string
	for _, n := range p.List("name") {
		s := pyStrValue(n)
		if strings.TrimSpace(s) == "" {
			return agentproto.Fail("Package name(s) cannot be empty or whitespace-only")
		}
		names = append(names, s)
	}
	a := &apkRun{env: env, cmd: []string{bin}}
	if p.Bool("no_cache") {
		a.cmd = append(a.cmd, "--no-cache")
	}
	for _, r := range p.List("repository") {
		a.cmd = append(a.cmd, "--repository", pyStrValue(r), "--repositories-file", "/dev/null")
	}
	state := p.Str("state")
	switch state {
	case "installed":
		state = "present"
	case "removed":
		state = "absent"
	}

	if p.Bool("update_cache") {
		rc, stdout, stderr := a.run("update")
		if rc != 0 {
			return apkResult(true, false, "could not update package db", stdout, stderr, nil)
		}
		if len(names) == 0 && !p.Bool("upgrade") {
			return apkResult(false, true, "updated repository indexes", stdout, stderr, nil)
		}
	}
	if p.Bool("upgrade") {
		argv := []string{"upgrade"}
		if env.CheckMode {
			argv = append(argv, "--simulate")
		}
		if p.Bool("available") {
			argv = append(argv, "--available")
		}
		rc, stdout, stderr := a.run(argv...)
		pkgs := apkParsePackages(stdout)
		if rc != 0 {
			return apkResult(true, false, "failed to upgrade packages", stdout, stderr, pkgs)
		}
		if len(pkgs) > 0 {
			return apkResult(false, true, "upgraded packages", stdout, stderr, pkgs)
		}
		return apkResult(false, false, "packages already upgraded", stdout, stderr, pkgs)
	}
	if state == "absent" {
		return a.remove(names)
	}
	return a.install(names, state, p.Str("world"))
}

func (a *apkRun) queryPackage(name string) bool {
	rc, _, _ := a.run("-v", "info", "--installed", name)
	return rc == 0
}

func (a *apkRun) queryLatest(name string) bool {
	_, stdout, _ := a.run("version", name)
	re := regexp.MustCompile(`(` + regexp.QuoteMeta(name) + `)-[\d\.\w]+-[\d\w]+\s+(.)\s+[\d\.\w]+-[\d\w]+\s+`)
	m := re.FindStringSubmatch(stdout)
	return !(m != nil && m[2] == "<")
}

func (a *apkRun) queryVirtual(name string) bool {
	_, stdout, _ := a.run("-v", "info", "--description", name)
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `: virtual meta package`).MatchString(stdout)
}

func (a *apkRun) dependencies(name string) []string {
	_, stdout, _ := a.run("-v", "info", "--depends", name)
	deps := strings.Fields(stdout)
	if len(deps) > 1 {
		return deps[1:]
	}
	return nil
}

func apkQueryToplevel(name, world string) (bool, error) {
	data, err := os.ReadFile(world)
	if err != nil {
		return false, err
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `([@=<>~].+)?$`)
	for _, p := range strings.Fields(string(data)) {
		if re.MatchString(p) {
			return true, nil
		}
	}
	return false, nil
}

func (a *apkRun) install(names []string, state, world string) *agentproto.Result {
	var toInstall, toUpgrade []string
	for _, name := range names {
		if a.queryVirtual(name) {
			for _, dep := range a.dependencies(name) {
				if state == "latest" && !a.queryLatest(dep) {
					toUpgrade = append(toUpgrade, dep)
				}
			}
			continue
		}
		top, err := apkQueryToplevel(name, world)
		if err != nil {
			return agentproto.Fail("%s", pyStrOSError(err, world))
		}
		if !top {
			toInstall = append(toInstall, name)
		} else if state == "latest" && !a.queryLatest(name) {
			toUpgrade = append(toUpgrade, name)
		}
	}
	if len(toInstall) == 0 && len(toUpgrade) == 0 {
		return &agentproto.Result{Msg: "package(s) already installed"}
	}
	packages := append(toInstall, toUpgrade...)
	argv := []string{"add"}
	if len(toUpgrade) > 0 {
		argv = append(argv, "--upgrade")
	}
	if a.env.CheckMode {
		argv = append(argv, "--simulate")
	}
	rc, stdout, stderr := a.run(append(argv, packages...)...)
	pkgs := apkParsePackages(stdout)
	if rc != 0 {
		return apkResult(true, false, fmt.Sprintf("failed to install %s", pyValueRepr(packages)), stdout, stderr, pkgs)
	}
	return apkResult(false, true, fmt.Sprintf("installed %s package(s)", pyValueRepr(packages)), stdout, stderr, pkgs)
}

func (a *apkRun) remove(names []string) *agentproto.Result {
	var installed []string
	for _, name := range names {
		if a.queryPackage(name) {
			installed = append(installed, name)
		}
	}
	if len(installed) == 0 {
		return &agentproto.Result{Msg: "package(s) already removed"}
	}
	argv := []string{"del", "--purge"}
	if a.env.CheckMode {
		argv = append(argv, "--simulate")
	}
	rc, stdout, stderr := a.run(append(argv, installed...)...)
	pkgs := apkParsePackages(stdout)
	for _, name := range installed {
		if a.queryPackage(name) {
			rc = 1
			break
		}
	}
	if rc != 0 {
		return apkResult(true, false, fmt.Sprintf("failed to remove %s package(s)", pyValueRepr(installed)), stdout, stderr, pkgs)
	}
	return apkResult(false, true, fmt.Sprintf("removed %s package(s)", pyValueRepr(installed)), stdout, stderr, pkgs)
}
