package modules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports ansible.builtin.package_facts for the apt and rpm
// managers (the ones Linux targets use). rpm matches the module's CLI
// backend exactly; apt reads dpkg's database (the module uses python-apt):
// name, version, arch, category (dpkg Section) and origin (the Release
// "Origin" of the archive the installed version came from).

func init() {
	names := []string{"package_facts", "ansible.builtin.package_facts"}
	Register(packageFactsModule, names...)
	for _, n := range names {
		specs[n] = packageFactsSpec
	}
}

var packageFactsSpec = args.Spec{
	"manager":  {Type: "list", Default: []any{"auto"}},
	"strategy": {Default: "first", Choices: []string{"first", "all"}},
}

// packageFactsManagers is PKG_MANAGER_NAMES (sorted) plus aliases.
var packageFactsManagers = []string{"apk", "apt", "openbsd_pkg", "pacman", "pkg", "pkg5", "pkg_info", "portage", "rpm", "pkg_ng", "pkgng"}

func packageFactsModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	p, err := packageFactsSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	var managers []string
	auto := false
	for _, m := range p.List("manager") {
		s := strings.ToLower(strings.TrimSpace(anyToString(m)))
		if s == "auto" {
			auto = true
			continue
		}
		managers = append(managers, s)
	}
	if auto {
		managers = append(managers, packageFactsManagers...)
	}
	var unsupported []string
	for _, m := range managers {
		if !containsString(packageFactsManagers, m) {
			unsupported = append(unsupported, m)
		}
	}
	if len(unsupported) > 0 {
		if auto {
			return agentproto.Fail("Could not auto detect a usable package manager, check warnings for details.")
		}
		return agentproto.Fail("Unsupported package managers requested: %s", strings.Join(unsupported, ", "))
	}
	var warnings []any
	packages := map[string]any{}
	found := 0
	seen := map[string]bool{}
	for _, m := range managers {
		if p.Str("strategy") == "first" && found > 0 {
			break
		}
		if m == "pkg_ng" || m == "pkgng" {
			m = "pkg"
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		var list map[string][]any
		switch m {
		case "rpm":
			if _, err := lookPath("rpm"); err != nil {
				continue
			}
			list = rpmPackages(env, &warnings)
		case "apt":
			if _, err := lookPath("dpkg-query"); err != nil {
				continue
			}
			if !anyExists("apt", "apt-get", "aptitude") {
				continue
			}
			list = aptPackages(env, &warnings)
		default:
			continue // other managers: not available on understudy's targets
		}
		if len(list) == 0 {
			warnings = append(warnings, `Found "`+m+`" but no associated packages`)
			continue
		}
		found++
		for k, v := range list {
			if cur, ok := packages[k].([]any); ok {
				packages[k] = append(cur, v...)
			} else {
				packages[k] = v
			}
		}
	}
	res := &agentproto.Result{Extra: map[string]any{}}
	if len(warnings) > 0 {
		res.Extra["warnings"] = warnings
	}
	if found == 0 {
		quoted := make([]string, len(managers))
		for i, m := range managers {
			quoted[i] = "'" + m + "'"
		}
		res.Failed = true
		res.Msg = "Could not detect a supported package manager from the following list: [" + strings.Join(quoted, ", ") +
			"], or the required Python library is not installed. Check warnings for details."
		return res
	}
	res.AnsibleFacts = map[string]any{"packages": packages}
	return res
}

func anyToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	s, _ := argString(map[string]any{"v": v}, "v")
	return s
}

func anyExists(names ...string) bool {
	for _, n := range names {
		if _, err := lookPath(n); err == nil {
			return true
		}
	}
	return false
}

func rpmPackages(env *RunEnv, warnings *[]any) map[string][]any {
	rc, out, stderr := runCommand(env, []string{"rpm", "-qa", "--qf", "%{NAME}|%{VERSION}|%{RELEASE}|%{EPOCH}|%{ARCH}\n"}, cmdOpts{})
	if rc != 0 {
		*warnings = append(*warnings, "Failed to retrieve packages with rpm: Unable to list packages rc="+itoa(rc)+" : "+stderr)
		return nil
	}
	none := func(v string) string {
		if v == "(none)" {
			return "None"
		}
		return v
	}
	pkgs := map[string][]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 {
			continue
		}
		d := map[string]any{"name": none(f[0]), "version": none(f[1]), "release": none(f[2]),
			"epoch": none(f[3]), "arch": none(f[4]), "source": "rpm"}
		name := d["name"].(string)
		pkgs[name] = append(pkgs[name], d)
	}
	return pkgs
}

var aptPolicyVersionRe = regexp.MustCompile(`^\s*\*\*\*\s+(\S+)`)

func aptPackages(env *RunEnv, warnings *[]any) map[string][]any {
	rc, out, stderr := runCommand(env, []string{"dpkg-query", "-W", "-f",
		"${binary:Package}|${Version}|${Architecture}|${Section}|${db:Status-Abbrev}\n"}, cmdOpts{})
	if rc != 0 {
		*warnings = append(*warnings, "Failed to retrieve packages with apt: "+strings.TrimSpace(stderr))
		return nil
	}
	type row struct{ name, version, arch, section string }
	var rows []row
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 || !strings.HasPrefix(f[4], "ii") && !strings.HasPrefix(f[4], "hi") {
			continue
		}
		rows = append(rows, row{f[0], f[1], f[2], f[3]})
		names = append(names, f[0])
	}
	origins := aptOrigins(env, names)
	pkgs := map[string][]any{}
	for _, r := range rows {
		d := map[string]any{"name": r.name, "version": r.version, "arch": r.arch,
			"category": r.section, "origin": origins[r.name], "source": "apt"}
		pkgs[r.name] = append(pkgs[r.name], d)
	}
	return pkgs
}

// aptOrigins maps each package to the Release "Origin" of the archive its
// installed version comes from ("" for locally installed versions), via
// one apt-cache policy call.
func aptOrigins(env *RunEnv, names []string) map[string]string {
	out := map[string]string{}
	if len(names) == 0 {
		return out
	}
	rc, stdout, _ := runCommand(env, append([]string{"apt-cache", "policy"}, names...), cmdOpts{Env: map[string]string{"LC_ALL": "C"}})
	if rc != 0 {
		return out
	}
	releaseOrigin := map[string]string{}
	originOf := func(url, suite string) string {
		key := url + " " + suite
		if o, ok := releaseOrigin[key]; ok {
			return o
		}
		mangled := strings.TrimSuffix(regexp.MustCompile(`^\w+://`).ReplaceAllString(url, ""), "/")
		mangled = strings.ReplaceAll(mangled, "/", "_")
		o := ""
		for _, f := range []string{"InRelease", "Release"} {
			data, err := os.ReadFile(filepath.Join("/var/lib/apt/lists", mangled+"_dists_"+strings.ReplaceAll(suite, "/", "_")+"_"+f))
			if err != nil {
				continue
			}
			if m := regexp.MustCompile(`(?m)^Origin:\s*(.*)$`).FindSubmatch(data); m != nil {
				o = strings.TrimSpace(string(m[1]))
			}
			break
		}
		releaseOrigin[key] = o
		return o
	}
	var pkg string
	inInstalled := false
	for _, line := range strings.Split(stdout, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			pkg = strings.TrimSuffix(line, ":")
			inInstalled = false
			continue
		}
		if aptPolicyVersionRe.MatchString(line) {
			inInstalled = true
			continue
		}
		t := strings.Fields(line)
		if inInstalled && len(t) >= 3 && strings.Contains(t[1], "://") {
			if _, done := out[pkg]; !done {
				suite := strings.SplitN(t[2], "/", 2)[0]
				out[pkg] = originOf(t[1], suite)
			}
			continue
		}
		if inInstalled && len(t) >= 1 && !strings.Contains(line, "://") && !strings.HasPrefix(strings.TrimSpace(line), "100 /var/lib/dpkg/status") {
			inInstalled = false
		}
	}
	return out
}
