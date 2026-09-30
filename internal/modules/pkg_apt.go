package modules

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// Ports of the ansible.builtin.apt operations beyond install/remove:
// upgrade, deb, clean/autoclean/autoremove, build-dep/fixed, policy-rc.d
// and cache-update retries.

const (
	aptGetZero    = "\n0 upgraded, 0 newly installed, 0 to remove"
	aptitudeZero  = "\n0 packages upgraded, 0 newly installed, 0 to remove"
	policyRcDPath = "/usr/sbin/policy-rc.d"
)

var aptCleanChanged = map[string]string{
	"autoremove": "The following packages will be REMOVED",
	"autoclean":  "Del ",
}

// aptEnv is the apt module's run_command_environ_update.
func aptEnv(env *RunEnv) map[string]string {
	loc := bestParsableLocale(env)
	return map[string]string{
		"DEBIAN_FRONTEND": "noninteractive", "DEBIAN_PRIORITY": "critical",
		"LANG": loc, "LC_ALL": loc, "LC_MESSAGES": loc, "LC_CTYPE": loc, "LANGUAGE": loc,
	}
}

// runAptCmd runs an apt command line under the apt environment.
func runAptCmd(env *RunEnv, argv ...string) (int, string, string) {
	return runCommand(env, argv, cmdOpts{Env: aptEnv(env)})
}

// runAptLine runs a command string the way run_command splits one.
func runAptLine(env *RunEnv, line string) (int, string, string) {
	argv, err := shlexSplit(line)
	if err != nil {
		return 257, "", err.Error()
	}
	return runAptCmd(env, argv...)
}

// aptDpkgOptions is expand_dpkg_options() plus the lock timeout, as the
// module passes it on every apt-get command line.
func aptDpkgOptions(p *args.Parsed) string {
	var parts []string
	for _, opt := range strings.Split(p.Str("dpkg_options"), ",") {
		parts = append(parts, fmt.Sprintf(`-o "Dpkg::Options::=--%s"`, opt))
	}
	timeout := int64(60)
	if p.Has("lock_timeout") {
		timeout = p.Int("lock_timeout")
	}
	return fmt.Sprintf("%s -o DPkg::Lock::Timeout=%d", strings.Join(parts, " "), timeout)
}

func aptGetPath() string {
	if path, err := getBinPath("apt-get"); err == nil {
		return path
	}
	return "apt-get"
}

func flagIf(on bool, flag string) string {
	if on {
		return flag
	}
	return ""
}

// withPolicyRcD runs fn with /usr/sbin/policy-rc.d replaced by a script
// exiting policy_rc_d (PolicyRcD), restoring what was there after.
func withPolicyRcD(apt bool, p *args.Parsed, fn func() error) error {
	if !apt || !p.Has("policy_rc_d") {
		return fn()
	}
	backup := ""
	if _, err := os.Stat(policyRcDPath); err == nil {
		dir, err := os.MkdirTemp("", "ansible")
		if err != nil {
			return fmt.Errorf("Fail to move %s to %s", policyRcDPath, dir)
		}
		backup = filepath.Join(dir, "policy-rc.d")
		if err := os.Rename(policyRcDPath, backup); err != nil {
			return fmt.Errorf("Fail to move %s to %s", policyRcDPath, dir)
		}
	}
	if err := os.WriteFile(policyRcDPath, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", p.Int("policy_rc_d"))), 0o755); err != nil {
		return fmt.Errorf("Failed to create or chmod %s", policyRcDPath)
	}
	os.Chmod(policyRcDPath, 0o755)
	runErr := fn()
	if backup != "" {
		if err := os.Rename(backup, policyRcDPath); err != nil {
			return fmt.Errorf("Fail to move back %s to %s", backup, policyRcDPath)
		}
		os.Remove(filepath.Dir(backup))
	} else if err := os.Remove(policyRcDPath); err != nil {
		return fmt.Errorf("Fail to remove %s (after package manipulation)", policyRcDPath)
	}
	return runErr
}

// aptUpdateWithRetries refreshes the cache, retrying with exponential
// backoff (update_cache_retries, update_cache_retry_max_delay).
func aptUpdateWithRetries(env *RunEnv, mgr *pkgManager, repo []string, p *args.Parsed) *agentproto.Result {
	retries := int(p.Int("update_cache_retries"))
	maxDelay := float64(p.Int("update_cache_retry_max_delay"))
	randomize := float64(rand.Intn(1000)) / 1000
	var warnings []any
	var lastErr string
	for retry := 0; retry < retries; retry++ {
		out, err := mgr.refresh(env, repo)
		if err == nil {
			return nil
		}
		lastErr = strings.TrimSpace(out)
		if lastErr == "" {
			lastErr = err.Error()
		}
		warnings = append(warnings, fmt.Sprintf("Failed to update cache after %d retries due to %s, retrying", retry+1, lastErr))
		delay := float64(int(1)<<retry) + randomize
		if delay > maxDelay {
			delay = maxDelay + randomize
		}
		time.Sleep(time.Duration(delay * float64(time.Second)))
		warnings = append(warnings, fmt.Sprintf("Sleeping for %d seconds, before attempting to refresh the cache again", int(delay+0.5)))
	}
	if lastErr == "" {
		lastErr = "unknown reason"
	}
	res := agentproto.Fail("Failed to update apt cache after %d retries: %s", retries, lastErr)
	if len(warnings) > 0 {
		res.Extra = map[string]any{"warnings": warnings}
	}
	return res
}

// aptOutput shapes a finished apt command as the module reports it.
func aptOutput(changed bool, msg *string, out, errOut string) *agentproto.Result {
	res := &agentproto.Result{Changed: changed, Stdout: out, Stderr: errOut, Extra: map[string]any{}}
	setOutputLines(res, out, errOut)
	if msg != nil {
		res.Msg = *msg
		if *msg == "" {
			res.Extra["msg"] = ""
		}
	}
	return res
}

// aptUpgrade is upgrade(): dist/full/safe(yes) through apt-get or
// aptitude.
func aptUpgrade(env *RunEnv, p *args.Parsed, mode string) *agentproto.Result {
	aptitude, aptitudeErr := getBinPath("aptitude")
	useAptGet := p.Bool("force_apt_get") || aptitudeErr != nil
	autoremove := flagIf(p.Bool("autoremove"), "--auto-remove")
	check := flagIf(env.CheckMode, "--simulate")
	var aptCmd, upgradeCommand string
	isAptGet := true
	switch {
	case mode == "dist" || mode == "full" && useAptGet:
		aptCmd, upgradeCommand = aptGetPath(), "dist-upgrade "+autoremove
	case mode == "full":
		aptCmd, upgradeCommand, isAptGet = aptitude, "full-upgrade", false
	case useAptGet:
		aptCmd, upgradeCommand = aptGetPath(), "upgrade --with-new-pkgs "+autoremove
	default:
		aptCmd, upgradeCommand, isAptGet = aptitude, "safe-upgrade", false
	}
	var warnings []any
	forceYes := ""
	if p.Bool("force") {
		forceYes = "--force-yes"
		if !isAptGet {
			forceYes = "--assume-yes --allow-untrusted"
		}
	}
	failOnAutoremove := ""
	if p.Bool("fail_on_autoremove") {
		if isAptGet {
			failOnAutoremove = "--no-remove"
		} else {
			warnings = append(warnings, "APTITUDE does not support '--no-remove', ignoring the 'fail_on_autoremove' parameter.")
		}
	}
	allowUnauth := flagIf(p.Bool("allow_unauthenticated"), "--allow-unauthenticated")
	allowDowngrade := ""
	if p.Bool("allow_downgrade") {
		if isAptGet {
			allowDowngrade = "--allow-downgrades"
		} else {
			warnings = append(warnings, "APTITUDE does not support '--allow-downgrades', ignoring the 'allow_downgrade' parameter.")
		}
	}
	cmd := fmt.Sprintf("%s -y %s %s %s %s %s %s %s", aptCmd, aptDpkgOptions(p), forceYes, failOnAutoremove,
		allowUnauth, allowDowngrade, check, upgradeCommand)
	if rel := p.Str("default_release"); rel != "" {
		cmd += fmt.Sprintf(" -t '%s'", rel)
	}
	var rc int
	var out, errOut string
	if err := withPolicyRcD(true, p, func() error {
		rc, out, errOut = runAptLine(env, cmd)
		return nil
	}); err != nil {
		return agentproto.Fail("%v", err)
	}
	var res *agentproto.Result
	switch {
	case rc != 0:
		res = &agentproto.Result{Failed: true, Msg: fmt.Sprintf("'%s %s' failed: %s", aptCmd, upgradeCommand, errOut),
			Stdout: out, RC: agentproto.IntPtr(rc)}
	case isAptGet && strings.Contains(out, aptGetZero) || !isAptGet && strings.Contains(out, aptitudeZero):
		res = aptOutput(false, &out, out, errOut)
	default:
		res = aptOutput(true, &out, out, errOut)
	}
	if len(warnings) > 0 {
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		res.Extra["warnings"] = warnings
	}
	return res
}

// aptCleanup is cleanup(): apt-get autoclean / autoremove.
func aptCleanup(env *RunEnv, p *args.Parsed, op string) *agentproto.Result {
	cmd := fmt.Sprintf("%s -y %s %s %s %s %s", aptGetPath(), aptDpkgOptions(p), flagIf(p.Bool("purge"), "--purge"),
		flagIf(p.Bool("force"), "--force-yes"), op, flagIf(env.CheckMode, "--simulate"))
	var rc int
	var out, errOut string
	if err := withPolicyRcD(true, p, func() error {
		rc, out, errOut = runAptLine(env, cmd)
		return nil
	}); err != nil {
		return agentproto.Fail("%v", err)
	}
	if rc != 0 {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("'apt-get %s' failed: %s", op, errOut),
			Stdout: out, Stderr: errOut, RC: agentproto.IntPtr(rc)}
	}
	return aptOutput(strings.Contains(out, aptCleanChanged[op]), nil, out, errOut)
}

// aptBuildDepOrFixed is install() for state=build-dep (apt-get build-dep)
// and state=fixed (install --fix-broken).
func aptBuildDepOrFixed(env *RunEnv, p *args.Parsed, names []string, state string) *agentproto.Result {
	var quoted []string
	for _, n := range names {
		quoted = append(quoted, "'"+n+"'")
	}
	if len(quoted) == 0 {
		return &agentproto.Result{}
	}
	packages := strings.Join(quoted, " ")
	onlyUpgrade := flagIf(p.Bool("only_upgrade"), "--only-upgrade")
	fixed := flagIf(state == "fixed", "--fix-broken")
	forceYes := flagIf(p.Bool("force"), "--force-yes")
	failOnAutoremove := flagIf(p.Bool("fail_on_autoremove"), "--no-remove")
	check := flagIf(env.CheckMode, "--simulate")
	var cmd string
	if state == "build-dep" {
		cmd = fmt.Sprintf("%s -y %s %s %s %s %s %s build-dep %s", aptGetPath(), aptDpkgOptions(p), onlyUpgrade, fixed,
			forceYes, failOnAutoremove, check, packages)
	} else {
		cmd = fmt.Sprintf("%s -y %s %s %s %s %s %s %s install %s", aptGetPath(), aptDpkgOptions(p), onlyUpgrade, fixed,
			forceYes, flagIf(p.Bool("autoremove"), "--auto-remove"), failOnAutoremove, check, packages)
	}
	if rel := p.Str("default_release"); rel != "" {
		cmd += fmt.Sprintf(" -t '%s'", rel)
	}
	if p.Has("install_recommends") {
		if p.Bool("install_recommends") {
			cmd += " -o APT::Install-Recommends=yes"
		} else {
			cmd += " -o APT::Install-Recommends=no"
		}
	}
	cmd += flagIf(p.Bool("allow_unauthenticated"), " --allow-unauthenticated")
	cmd += flagIf(p.Bool("allow_downgrade"), " --allow-downgrades")
	cmd += flagIf(p.Bool("allow_change_held_packages"), " --allow-change-held-packages")
	var rc int
	var out, errOut string
	if err := withPolicyRcD(true, p, func() error {
		rc, out, errOut = runAptLine(env, cmd)
		return nil
	}); err != nil {
		return agentproto.Fail("%v", err)
	}
	if rc != 0 {
		return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("'%s' failed: %s", cmd, errOut),
			Stdout: out, Stderr: errOut, RC: agentproto.IntPtr(rc)}
	}
	changed := true
	if state == "build-dep" {
		changed = !strings.Contains(out, aptGetZero)
	}
	return aptOutput(changed, nil, out, errOut)
}

// aptInstallDeb is install_deb(): each deb file (downloaded first when a
// URL) is installed with dpkg -i unless the same version is installed.
// Dependencies are left to dpkg: the Python module resolves them
// through python-apt first.
func aptInstallDeb(env *RunEnv, p *args.Parsed, debs string) *agentproto.Result {
	dpkg, err := getBinPath("dpkg")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	var files []string
	for _, deb := range strings.Split(debs, ",") {
		if strings.Contains(deb, "://") {
			local, err := fetchDeb(deb)
			if err != nil {
				return agentproto.Fail("%v", err)
			}
			deb = local
		}
		field := func(name string) (string, *agentproto.Result) {
			cmd := fmt.Sprintf("%s --field %s %s", dpkg, deb, name)
			rc, out, errOut := runAptLine(env, cmd)
			if rc != 0 {
				r := aptOutput(false, nil, out, errOut)
				r.Failed, r.Msg = true, cmd+" failed"
				return "", r
			}
			return strings.Trim(out, "\n"), nil
		}
		name, fail := field("Package")
		if fail != nil {
			return fail
		}
		version, fail := field("Version")
		if fail != nil {
			return fail
		}
		rc, installed, _ := runAptCmd(env, "dpkg-query", "-W", "-f=${Status} ${Version}", name)
		if rc == 0 && strings.HasPrefix(installed, "install ok installed ") &&
			strings.TrimPrefix(installed, "install ok installed ") == version {
			continue
		}
		files = append(files, deb)
	}
	if len(files) == 0 {
		r := aptOutput(false, nil, "", "")
		r.Extra["diff"] = ""
		return r
	}
	// Missing dependencies go through apt first (DebPackage.missing_deps),
	// then are marked automatically installed.
	var deps []string
	var depOut, depErr string
	for _, f := range files {
		local := f
		if !strings.HasPrefix(local, "/") && !strings.HasPrefix(local, ".") {
			local = "./" + local
		}
		pkgName := ""
		if rc, out, _ := runAptCmd(env, dpkg, "--field", f, "Package"); rc == 0 {
			pkgName = strings.TrimSpace(out)
		}
		_, out, _ := runAptCmd(env, aptGetPath(), "install", "--simulate", "-q", local)
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "Inst" && fields[1] != pkgName &&
				!containsStr(deps, fields[1]) {
				deps = append(deps, fields[1])
			}
		}
	}
	if len(deps) > 0 && !env.CheckMode {
		argv := []string{aptGetPath(), "-y"}
		argv = append(argv, strings.Fields(strings.ReplaceAll(aptDpkgOptions(p), `"`, ""))...)
		if p.Has("install_recommends") {
			argv = append(argv, "-o", "APT::Install-Recommends="+map[bool]string{true: "yes", false: "no"}[p.Bool("install_recommends")])
		}
		argv = append(argv, "install")
		argv = append(argv, deps...)
		rc, out, errOut := runAptCmd(env, argv...)
		if rc != 0 {
			return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("'%s' failed: %s", strings.Join(argv, " "), errOut),
				Stdout: out, Stderr: errOut, RC: agentproto.IntPtr(rc)}
		}
		runAptCmd(env, append([]string{"apt-mark", "auto"}, deps...)...)
		depOut, depErr = out, errOut
	}
	var opts []string
	for _, x := range strings.Split(p.Str("dpkg_options"), ",") {
		opts = append(opts, "--"+x)
	}
	options := strings.Join(opts, " ")
	if env.CheckMode {
		options += " --simulate"
	}
	if p.Bool("force") {
		options += " --force-all"
	}
	cmd := fmt.Sprintf("dpkg %s -i %s", options, strings.Join(files, " "))
	var rc int
	var out, errOut string
	if err := withPolicyRcD(true, p, func() error {
		rc, out, errOut = runAptLine(env, cmd)
		return nil
	}); err != nil {
		return agentproto.Fail("%v", err)
	}
	out, errOut = depOut+out, depErr+errOut
	if rc != 0 {
		r := aptOutput(false, nil, out, errOut)
		r.Failed, r.Msg = true, cmd+" failed"
		return r
	}
	return aptOutput(true, nil, out, errOut)
}

// fetchDeb downloads a deb URL into a temporary directory (fetch_file).
func fetchDeb(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("Failure downloading %s, %s", url, urlErrorMsg(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("Failure downloading %s, %s", url, httpReason(resp))
	}
	dir, err := os.MkdirTemp("", "ansible-deb")
	if err != nil {
		return "", err
	}
	name := url[strings.LastIndexByte(url, '/')+1:]
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	local := filepath.Join(dir, name)
	f, err := os.Create(local)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("Failure downloading %s, %s", url, err)
	}
	return local, nil
}

// setOutputLines records stdout/stderr (empty ones included) with the
// *_lines the action derives from them (str.splitlines).
func setOutputLines(res *agentproto.Result, out, errOut string) {
	for _, kv := range [][2]string{{"stdout", out}, {"stderr", errOut}} {
		if kv[1] == "" {
			res.Extra[kv[0]] = ""
		}
		lines := []any{}
		for _, l := range pyStrSplitlines(kv[1]) {
			lines = append(lines, l)
		}
		res.Extra[kv[0]+"_lines"] = lines
	}
}

// pyStrSplitlines is str.splitlines(): \n, \r and \r\n (and the other
// Unicode line boundaries) end lines.
func pyStrSplitlines(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(rs[start:i]))
			if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// pyBool is str() of a Python bool.
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func withArg(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for key, val := range m {
		out[key] = val
	}
	out[k] = v
	return out
}

// dnfList is the dnf module's list= query: installed, updates,
// available, repos, or a package spec.
func dnfList(env *RunEnv, mgr string, repo []string, what string) *agentproto.Result {
	bin := pkgBinary(mgr)
	results := []any{}
	if what == "repos" || what == "repositories" {
		rc, out, errOut := runCommand(env, append(append([]string{bin, "-q"}, repo...), "repolist", "--enabled"), cmdOpts{})
		if rc != 0 {
			return &agentproto.Result{Failed: true, Msg: strings.TrimSpace(errOut), RC: agentproto.IntPtr(1), Extra: map[string]any{"results": []any{}}}
		}
		var ids []string
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] == "repo" && len(f) > 1 && f[1] == "id" {
				continue
			}
			ids = append(ids, f[0])
		}
		// base.repos.iter_enabled() follows the repo files (sorted) and
		// their section order; repolist sorts by id.
		for _, id := range repoFileOrder(ids) {
			results = append(results, map[string]any{"repoid": id, "state": "enabled"})
		}
		return &agentproto.Result{Extra: map[string]any{"msg": "", "results": results}}
	}
	// Name first: repoquery sorts its output lines, and the sack
	// iterates by name.
	const qf = "%{name}\t%{epoch}\t%{version}\t%{release}\t%{arch}\t%{repoid}\n"
	var queries [][]string
	switch what {
	case "installed":
		queries = [][]string{{"--installed"}}
	case "updates", "upgrades":
		queries = [][]string{{"--upgrades"}}
	case "available":
		queries = [][]string{{"--available"}}
	default:
		queries = [][]string{{"--installed", what}, {"--available", what}}
	}
	for _, q := range queries {
		argv := append(append([]string{bin, "-q"}, repo...), "repoquery", "--qf", qf)
		rc, out, errOut := runCommand(env, append(argv, q...), cmdOpts{})
		if rc != 0 {
			return &agentproto.Result{Failed: true, Msg: strings.TrimSpace(errOut), RC: agentproto.IntPtr(1), Extra: map[string]any{"results": []any{}}}
		}
		installed := q[0] == "--installed"
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\t")
			if len(f) != 6 {
				continue
			}
			f[0], f[1] = f[1], f[0]
			state, repoid := "available", f[5]
			if installed {
				state, repoid = "installed", "@System"
			}
			envra := fmt.Sprintf("%s:%s-%s-%s.%s", f[0], f[1], f[2], f[3], f[4])
			results = append(results, map[string]any{
				"name": f[1], "arch": f[4], "epoch": f[0], "release": f[3], "version": f[2],
				"repo": repoid, "envra": envra, "nevra": envra, "yumstate": state,
			})
		}
	}
	return &agentproto.Result{Extra: map[string]any{"msg": "", "results": results}}
}

// aptRunResult runs an apt-get install/remove and reports it as the apt
// module does: full stdout/stderr, and on failure "'<cmd>' failed".
func aptRunResult(env *RunEnv, p *args.Parsed, res *agentproto.Result, argv []string, install bool) *agentproto.Result {
	var rc int
	var out, errOut string
	if err := withPolicyRcD(true, p, func() error {
		rc, out, errOut = runAptCmd(env, argv...)
		return nil
	}); err != nil {
		return agentproto.Fail("%v", err)
	}
	if rc != 0 {
		msg := fmt.Sprintf("'%s' failed: %s", strings.Join(argv, " "), errOut)
		if !install {
			msg = fmt.Sprintf("'apt-get remove %s' failed: %s", strings.Join(argv[len(argv)-1:], " "), errOut)
		}
		fail := &agentproto.Result{Failed: true, Msg: msg, Stdout: out, Stderr: errOut, RC: agentproto.IntPtr(rc), Extra: res.Extra}
		return fail
	}
	r := aptOutput(true, nil, out, errOut)
	for k, v := range res.Extra {
		r.Extra[k] = v
	}
	if install {
		r.Changed = pkgOutputShowsChange("apt", out)
	}
	return r
}

// repoFileOrder orders repo ids as they appear in /etc/yum.repos.d/*.repo
// (ids not found there keep their order, last).
func repoFileOrder(ids []string) []string {
	files, _ := filepath.Glob("/etc/yum.repos.d/*.repo")
	// by base name without the extension: rocky.repo before
	// rocky-extras.repo
	sort.Slice(files, func(i, j int) bool {
		return strings.TrimSuffix(files[i], ".repo") < strings.TrimSuffix(files[j], ".repo")
	})
	var ordered []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				id := line[1 : len(line)-1]
				if containsStr(ids, id) && !containsStr(ordered, id) {
					ordered = append(ordered, id)
				}
			}
		}
	}
	for _, id := range ids {
		if !containsStr(ordered, id) {
			ordered = append(ordered, id)
		}
	}
	return ordered
}
