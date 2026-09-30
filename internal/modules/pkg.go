package modules

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

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
	"update_cache":       {Type: "bool", Default: false, Aliases: []string{"update-cache", "expire-cache"}},
	"cache_valid_time":   {Type: "int"},
	"install_recommends": {Type: "bool"},
	"autoremove":         {Type: "bool", Default: false},
	"dpkg_options":       {Default: "force-confdef,force-confold"},

	// dnf/yum options (Ansible's dnf and yum modules).
	"enablerepo":        {Type: "list"},
	"disablerepo":       {Type: "list"},
	"use_backend":       {Choices: []string{"auto", "yum", "yum4", "dnf", "dnf4", "dnf5"}},
	"disable_gpg_check": {Type: "bool", Default: false},
	"exclude":           {Type: "list"},
	"skip_broken":       {Type: "bool", Default: false},
	"allowerasing":      {Type: "bool", Default: false},
	"nobest":            {Type: "bool"},
	"conf_file":         {},
	"releasever":        {},
	"installroot":       {},
	"install_weak_deps": {Type: "bool", Default: true},
	"disable_excludes":  {},
	"security":          {Type: "bool", Default: false},
	"bugfix":            {Type: "bool", Default: false},
	"download_only":     {Type: "bool", Default: false},
	"lock_timeout":      {Type: "int"},
	"validate_certs":    {Type: "bool", Default: true},
	"sslverify":         {Type: "bool", Default: true},
}

// rpmOnly/aptOnly options are rejected on the other family rather than
// silently ignored.
var (
	rpmOnlyOpts = []string{"enablerepo", "disablerepo", "use_backend", "disable_gpg_check", "exclude",
		"skip_broken", "allowerasing", "nobest", "conf_file", "releasever", "installroot",
		"disable_excludes", "security", "bugfix", "download_only"}
	aptOnlyOpts = []string{"cache_valid_time", "install_recommends", "dpkg_options"}
)

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
	"apk": {
		name: "apk",
		installed: func(env *RunEnv, pkg string) bool {
			_, err := runOut(env, "apk", "info", "-e", pkg)
			return err == nil
		},
		install: func(env *RunEnv, pkgs []string, latest bool, _ []string) (string, error) {
			argv := []string{"add"}
			if latest {
				argv = append(argv, "--upgrade")
			}
			return runOut(env, "apk", append(argv, pkgs...)...)
		},
		remove: func(env *RunEnv, pkgs []string, _ []string) (string, error) {
			return runOut(env, "apk", append([]string{"del"}, pkgs...)...)
		},
		refresh: func(env *RunEnv, _ []string) (string, error) {
			return runOut(env, "apk", "update")
		},
	},
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
		// yum on a dnf-only system (RHEL 8+) is dnf, as in Ansible's yum
		// action; use_backend picks explicitly.
		if mgr.name == "yum" || mgr.name == "dnf" {
			switch p.Str("use_backend") {
			case "dnf", "dnf4", "yum4":
				mgr = pkgManagers["dnf"]
			case "dnf5":
				mgr = rpmManager("dnf5")
			case "yum":
				mgr = pkgManagers["yum"]
			}
			if _, err := exec.LookPath(pkgBinary(mgr.name)); err != nil && mgr.name == "yum" {
				mgr = pkgManagers["dnf"]
			}
		}
		if _, err := exec.LookPath(pkgBinary(mgr.name)); err != nil {
			return agentproto.Fail("package manager %q is not available on this host", mgr.name)
		}
		opts, err := pkgOptions(mgr.name, p, rawArgs)
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

		wantUpdate := p.Bool("update_cache") || (mgr.name == "apt" && p.Int("cache_valid_time") > 0)
		if wantUpdate && mgr.name == "apt" {
			// ansible.builtin.apt: refresh unless the cache is younger than
			// cache_valid_time; with nothing else to do, changed reports
			// whether the cache was refreshed.
			before := aptCacheMtime()
			updated := false
			if !aptCacheFresh(mgr.name, int(p.Int("cache_valid_time"))) {
				if !env.CheckMode {
					if out, err := mgr.refresh(env, opts.repo); err != nil {
						return agentproto.Fail("cache update failed: %v: %s", err, tail(out))
					}
				}
				after := aptCacheMtime()
				updated = env.CheckMode || after != before
				before = after
			}
			res.Extra["cache_updated"] = updated
			res.Extra["cache_update_time"] = before
			if len(names) == 0 {
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
			out, err := mgr.install(env, targets, state == "latest", opts.install)
			if err != nil {
				return agentproto.Fail("package install failed: %v: %s", err, tail(out))
			}
			// The transaction decides: names that only resolve through dnf
			// (groups, modules) can look missing and still be "Nothing to do".
			res.Changed = pkgOutputShowsChange(mgr.name, out)
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
			out, err := mgr.remove(env, present, opts.remove)
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

type pkgOpts struct {
	repo, install, remove []string
}

// pkgOptions maps module options onto the manager's CLI flags, rejecting
// options that belong to the other package family.
func pkgOptions(mgr string, p *args.Parsed, raw map[string]any) (pkgOpts, error) {
	var o pkgOpts
	rpm := mgr == "dnf" || mgr == "yum" || mgr == "dnf5"
	reject := aptOnlyOpts
	if !rpm {
		reject = rpmOnlyOpts
	}
	for _, k := range reject {
		if _, set := raw[k]; set {
			return o, fmt.Errorf("the %q option is not supported by %s", k, mgr)
		}
	}
	if rpm {
		for _, r := range p.List("enablerepo") {
			o.repo = append(o.repo, "--enablerepo="+fmt.Sprint(r))
		}
		for _, r := range p.List("disablerepo") {
			o.repo = append(o.repo, "--disablerepo="+fmt.Sprint(r))
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
