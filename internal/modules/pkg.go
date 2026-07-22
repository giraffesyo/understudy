package modules

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(mkPkg(""), "package", "ansible.builtin.package")
	Register(mkPkg("apt"), "apt", "ansible.builtin.apt")
	Register(mkPkg("dnf"), "dnf", "ansible.builtin.dnf")
	Register(mkPkg("yum"), "yum", "ansible.builtin.yum")
	Register(mkPkg("apk"), "apk", "community.general.apk")
}

var pkgSpec = args.Spec{
	"name":               {Type: "list", Aliases: []string{"pkg", "package"}},
	"state":              {Default: "present", Choices: []string{"present", "installed", "absent", "removed", "latest"}},
	"update_cache":       {Type: "bool", Default: false, Aliases: []string{"update-cache"}},
	"cache_valid_time":   {Type: "int"},
	"install_recommends": {Type: "bool"},
	"autoremove":         {Type: "bool", Default: false},
}

// pkgManager abstracts one package manager's query and mutate commands.
type pkgManager struct {
	name      string
	installed func(env *RunEnv, pkg string) bool
	install   func(env *RunEnv, pkgs []string, latest bool) (string, error)
	remove    func(env *RunEnv, pkgs []string) (string, error)
	refresh   func(env *RunEnv) (string, error)
}

var pkgManagers = map[string]*pkgManager{
	"apt": {
		name: "apt",
		installed: func(env *RunEnv, pkg string) bool {
			out, err := runOut(env, "dpkg-query", "-W", "-f=${Status}", pkg)
			return err == nil && strings.Contains(out, "install ok installed")
		},
		install: func(env *RunEnv, pkgs []string, latest bool) (string, error) {
			argv := append([]string{"install", "-y"}, pkgs...)
			return runAptGet(env, argv...)
		},
		remove: func(env *RunEnv, pkgs []string) (string, error) {
			argv := append([]string{"remove", "-y"}, pkgs...)
			return runAptGet(env, argv...)
		},
		refresh: func(env *RunEnv) (string, error) {
			return runAptGet(env, "update")
		},
	},
	"dnf": rpmManager("dnf"),
	"yum": rpmManager("yum"),
	"apk": {
		name: "apk",
		installed: func(env *RunEnv, pkg string) bool {
			_, err := runOut(env, "apk", "info", "-e", pkg)
			return err == nil
		},
		install: func(env *RunEnv, pkgs []string, latest bool) (string, error) {
			argv := []string{"add"}
			if latest {
				argv = append(argv, "--upgrade")
			}
			return runOut(env, "apk", append(argv, pkgs...)...)
		},
		remove: func(env *RunEnv, pkgs []string) (string, error) {
			return runOut(env, "apk", append([]string{"del"}, pkgs...)...)
		},
		refresh: func(env *RunEnv) (string, error) {
			return runOut(env, "apk", "update")
		},
	},
}

func rpmManager(cmd string) *pkgManager {
	return &pkgManager{
		name: cmd,
		installed: func(env *RunEnv, pkg string) bool {
			_, err := runOut(env, "rpm", "-q", pkg)
			return err == nil
		},
		install: func(env *RunEnv, pkgs []string, latest bool) (string, error) {
			return runOut(env, cmd, append([]string{"install", "-y"}, pkgs...)...)
		},
		remove: func(env *RunEnv, pkgs []string) (string, error) {
			return runOut(env, cmd, append([]string{"remove", "-y"}, pkgs...)...)
		},
		refresh: func(env *RunEnv) (string, error) {
			return runOut(env, cmd, "makecache")
		},
	}
}

// mkPkg builds a package module bound to one manager, or auto-detecting
// when mgrName is "" (the generic `package` module).
func mkPkg(mgrName string) ModuleFunc {
	return func(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
		p, err := pkgSpec.Parse(rawArgs)
		if err != nil {
			return agentproto.Fail("%v", err)
		}

		mgr := pkgManagers[mgrName]
		if mgr == nil {
			mgr = detectPkgManager()
			if mgr == nil {
				return agentproto.Fail("no supported package manager found (apt, dnf, yum, apk)")
			}
		}
		if _, err := exec.LookPath(pkgBinary(mgr.name)); err != nil {
			return agentproto.Fail("package manager %q is not available on this host", mgr.name)
		}

		var names []string
		for _, n := range p.List("name") {
			s, ok := n.(string)
			if !ok || s == "" {
				return agentproto.Fail("package names must be strings")
			}
			names = append(names, s)
		}

		state := p.Str("state")
		switch state {
		case "installed":
			state = "present"
		case "removed":
			state = "absent"
		}

		res := &agentproto.Result{Extra: map[string]any{}}

		if p.Bool("update_cache") {
			if !env.CheckMode {
				if out, err := mgr.refresh(env); err != nil {
					return agentproto.Fail("cache update failed: %v: %s", err, tail(out))
				}
			}
			res.Extra["cache_updated"] = true
			if len(names) == 0 {
				return res
			}
		}
		if len(names) == 0 {
			return agentproto.Fail("'name' is required (or update_cache alone)")
		}

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
			if len(targets) == 0 {
				return res
			}
			if env.CheckMode {
				res.Changed = len(missing) > 0 || state == "latest"
				res.Extra["would_install"] = targets
				return res
			}
			out, err := mgr.install(env, targets, state == "latest")
			if err != nil {
				return agentproto.Fail("package install failed: %v: %s", err, tail(out))
			}
			res.Changed = len(missing) > 0 || pkgOutputShowsChange(mgr.name, out)
			res.Stdout = tail(out)
		case "absent":
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
			out, err := mgr.remove(env, present)
			if err != nil {
				return agentproto.Fail("package removal failed: %v: %s", err, tail(out))
			}
			res.Changed = true
			res.Stdout = tail(out)
		}
		return res
	}
}

func detectPkgManager() *pkgManager {
	for _, name := range []string{"apt", "dnf", "yum", "apk"} {
		if _, err := exec.LookPath(pkgBinary(name)); err == nil {
			return pkgManagers[name]
		}
	}
	return nil
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
	cmd := exec.Command(path, argv...)
	applyEnv(cmd, env)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

// runAptGet wraps apt-get with the noninteractive frontend.
func runAptGet(env *RunEnv, argv ...string) (string, error) {
	cmd := exec.Command("apt-get", argv...)
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
