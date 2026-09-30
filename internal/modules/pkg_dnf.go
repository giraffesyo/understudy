package modules

import (
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// The dnf (dnf4, through its dnf script) and dnf5 modules' ensure():
// the transaction is resolved and run by the dnf CLI, and the result
// takes the modules' shape: msg, rc, and results listing the transaction
// as "Installed: <nevra>" / "Removed: <nevra>" ("Nothing to do" when
// there is none), failures for specs that match no package.

// rpmNEVRAFormat renders an installed package as hawkey/libdnf5 print a
// NEVRA: the epoch only when it is set.
const rpmNEVRAFormat = `%{NAME}-%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}.%{ARCH}\n`

// rpmInstalledSet lists the installed packages' NEVRAs (gpg-pubkey
// pseudo-packages aside).
func rpmInstalledSet(env *RunEnv) map[string]bool {
	set := map[string]bool{}
	rc, out, _ := runCommand(env, []string{"rpm", "-qa", "--qf", rpmNEVRAFormat}, cmdOpts{})
	if rc != 0 {
		return set
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "gpg-pubkey-") {
			set[line] = true
		}
	}
	return set
}

// listifyCommaSep is YumDnf.listify_comma_sep_strings_in_list.
func listifyCommaSep(list []string) []string {
	var keep, extra []string
	for _, e := range list {
		if strings.Contains(e, ",") {
			for _, part := range strings.Split(e, ",") {
				extra = append(extra, strings.TrimSpace(part))
			}
			continue
		}
		keep = append(keep, e)
	}
	out := append(keep, extra...)
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// dnfResult builds the module's exit_json/fail_json.
type dnfResult struct {
	backend  string
	changed  bool
	msg      string
	results  []any
	failures []any
	failed   bool
}

func (r *dnfResult) result() *agentproto.Result {
	if r.results == nil {
		r.results = []any{}
	}
	res := &agentproto.Result{Changed: r.changed, Failed: r.failed, Msg: r.msg, Extra: map[string]any{}}
	rc := 0
	if r.failed {
		rc = 1
		res.Extra["failures"] = r.failures
		if r.failures == nil {
			res.Extra["failures"] = []any{}
		}
		if r.backend == "dnf" {
			res.Extra["results"] = r.results
		}
	} else {
		res.Extra["results"] = r.results
	}
	res.Extra["rc"] = rc
	if r.msg == "" {
		// exit_json(msg='') still reports the key.
		res.Extra["msg"] = ""
	}
	return res
}

// dnfNoMatch finds the specs the CLI could not match.
func dnfNoMatch(out string) []string {
	var specs []string
	for _, line := range strings.Split(out, "\n") {
		if spec, ok := strings.CutPrefix(strings.TrimSpace(line), "No match for argument: "); ok {
			specs = append(specs, spec)
		}
	}
	return specs
}

// dnfTransactionTable parses the transaction summary table dnf prints
// before it asks (or with --assumeno): installed and removed NEVRAs.
func dnfTransactionTable(out string) (installs, removes []string) {
	section := ""
	var pending string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if section != "" && pending == "" {
				section = ""
			}
			continue
		}
		if !strings.HasPrefix(line, " ") {
			low := strings.ToLower(strings.TrimSuffix(trimmed, ":"))
			switch {
			case strings.HasPrefix(low, "installing"), strings.HasPrefix(low, "upgrading"),
				strings.HasPrefix(low, "downgrading"), strings.HasPrefix(low, "reinstalling"):
				section = "install"
				if strings.HasPrefix(low, "upgrading") || strings.HasPrefix(low, "downgrading") || strings.HasPrefix(low, "reinstalling") {
					section = "replace"
				}
			case strings.HasPrefix(low, "removing"):
				section = "remove"
			default:
				section = ""
			}
			continue
		}
		if section == "" {
			continue
		}
		f := strings.Fields(trimmed)
		if f[0] == "replacing" {
			// obsoleted: "replacing  name.arch  evr"
			if len(f) >= 3 {
				if i := strings.LastIndexByte(f[1], '.'); i > 0 {
					removes = append(removes, f[1][:i]+"-"+f[2]+"."+f[1][i+1:])
				}
			}
			continue
		}
		if len(f) == 1 {
			pending = f[0] // a long name wraps onto its own line
			continue
		}
		if pending != "" {
			f = append([]string{pending}, f...)
			pending = ""
		}
		if len(f) < 3 {
			continue
		}
		nevra := f[0] + "-" + f[2] + "." + f[1]
		switch section {
		case "install":
			installs = append(installs, nevra)
		case "replace":
			installs = append(installs, nevra)
			removes = append(removes, "\x00"+f[0]+"."+f[1]) // resolved by the caller
		case "remove":
			removes = append(removes, nevra)
		}
	}
	return installs, removes
}

// runDnf is the dnf/dnf5 module's run(): list, update_cache alone, or
// ensure().
func runDnf(env *RunEnv, mgr *pkgManager, p *args.Parsed, raw map[string]any, names []string, opts pkgOpts, backend string) *agentproto.Result {
	r := &dnfResult{backend: backend}
	names = listifyCommaSep(names)
	for _, n := range names {
		if strings.Contains(n, " ") && !strings.ContainsAny(n, "@><=") {
			return agentproto.Fail("It appears that a space separated string of packages was passed in " +
				"as an argument. To operate on several packages, pass a comma separated " +
				"string of packages or a list of packages.")
		}
	}
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	autoremove := p.Bool("autoremove")
	state := p.Str("state")
	if state == "" {
		state = "present"
		if autoremove {
			state = "absent"
		}
	}
	if autoremove && state != "absent" {
		return &agentproto.Result{Failed: true, Msg: "Autoremove should be used alone or with state=absent",
			Extra: map[string]any{"results": []any{}}}
	}
	if p.Bool("update_cache") && len(names) == 0 && !p.Has("list") {
		if !env.CheckMode {
			if out, err := mgr.refresh(env, opts.repo); err != nil {
				return &agentproto.Result{Failed: true, Msg: tail(out), Extra: map[string]any{"results": []any{}, "rc": 1}}
			}
		}
		return &agentproto.Result{Msg: "Cache updated", Extra: map[string]any{"results": []any{}, "rc": 0}}
	}
	if p.Has("list") {
		return dnfList(env, mgr.name, opts.repo, p.Str("list"))
	}
	if p.Bool("update_cache") && !env.CheckMode {
		if out, err := mgr.refresh(env, opts.repo); err != nil {
			return &agentproto.Result{Failed: true, Msg: tail(out), Extra: map[string]any{"results": []any{}, "rc": 1}}
		}
	}
	bin := pkgBinary(mgr.name)

	var installs, upgrades, removes []string
	switch state {
	case "present", "installed", "latest":
		if len(names) == 1 && names[0] == "*" && state == "latest" {
			upgrades = []string{}
			break
		}
		for _, n := range names {
			installed := !strings.HasPrefix(n, "@") && mgr.installed(env, n)
			switch {
			case !installed && state == "latest" && p.Bool("update_only"):
				r.results = append(r.results, "Packages providing "+n+" not installed due to update_only specified")
			case !installed:
				installs = append(installs, n)
			case state == "latest":
				upgrades = append(upgrades, n)
			}
		}
	case "absent", "removed":
		for _, n := range names {
			if strings.HasPrefix(n, "@") || mgr.installed(env, n) {
				removes = append(removes, n)
			} else if backend == "dnf" {
				r.results = append(r.results, "No match for argument: "+n)
			}
		}
		if autoremove {
			removes = append(removes, "\x00autoremove")
		}
	}
	type step struct{ argv []string }
	var steps []step
	if len(installs) > 0 {
		steps = append(steps, step{append(append([]string{bin, "install"}, opts.install...), installs...)})
	}
	if upgrades != nil {
		steps = append(steps, step{append(append([]string{bin, "upgrade"}, opts.install...), upgrades...)})
	}
	var rm []string
	auto := false
	for _, n := range removes {
		if n == "\x00autoremove" {
			auto = true
		} else {
			rm = append(rm, n)
		}
	}
	if len(rm) > 0 {
		steps = append(steps, step{append(append([]string{bin, "remove"}, opts.remove...), rm...)})
	} else if auto {
		steps = append(steps, step{append([]string{bin, "autoremove"}, opts.remove...)})
	}
	if len(steps) == 0 {
		r.msg = "Nothing to do"
		return r.result()
	}

	var inst, rem []string
	var failures []any
	if env.CheckMode {
		for _, s := range steps {
			_, out, errOut := runCommand(env, append(append([]string(nil), s.argv...), "--assumeno"), cmdOpts{})
			for _, spec := range dnfNoMatch(out + "\n" + errOut) {
				failures = append(failures, "No package "+spec+" available.")
			}
			i, rmv := dnfTransactionTable(out)
			inst = append(inst, i...)
			rem = append(rem, resolveReplaced(env, rmv)...)
		}
	} else {
		before := rpmInstalledSet(env)
		for _, s := range steps {
			rc, out, errOut := runCommand(env, append(append([]string(nil), s.argv...), "-y"), cmdOpts{})
			if rc == 0 {
				continue
			}
			for _, spec := range dnfNoMatch(out + "\n" + errOut) {
				failures = append(failures, "No package "+spec+" available.")
			}
			if len(failures) == 0 {
				msg := strings.TrimSpace(errOut)
				if msg == "" {
					msg = strings.TrimSpace(out)
				}
				return (&dnfResult{backend: backend, failed: true, msg: "Unknown Error occurred: " + msg}).result()
			}
		}
		after := rpmInstalledSet(env)
		for n := range after {
			if !before[n] {
				inst = append(inst, n)
			}
		}
		for n := range before {
			if !after[n] {
				rem = append(rem, n)
			}
		}
	}
	requested := append(append(append([]string(nil), installs...), upgrades...), rm...)
	inst = dnfResultOrder(inst, requested)
	rem = dnfResultOrder(rem, requested)
	for _, n := range inst {
		r.results = append(r.results, "Installed: "+n)
	}
	for _, n := range rem {
		r.results = append(r.results, "Removed: "+n)
	}
	if len(failures) > 0 {
		r.failed, r.failures, r.msg = true, failures, "Failed to install some of the specified packages"
		return r.result()
	}
	switch {
	case len(inst)+len(rem) == 0:
		r.msg = "Nothing to do"
	case env.CheckMode:
		r.changed = true
		r.msg = "Check mode: No changes made, but would have if not in check mode"
	default:
		r.changed = true
	}
	return r.result()
}

// resolveReplaced turns the table's replaced packages ("\x00name.arch")
// into the installed NEVRAs they replace.
func resolveReplaced(env *RunEnv, list []string) []string {
	var out []string
	for _, n := range list {
		if !strings.HasPrefix(n, "\x00") {
			out = append(out, n)
			continue
		}
		rc, q, _ := runCommand(env, []string{"rpm", "-q", "--qf", rpmNEVRAFormat, n[1:]}, cmdOpts{})
		if rc != 0 {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(q), "\n") {
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// nevraName is the name part of a NEVRA (before version and release).
func nevraName(nevra string) string {
	s := nevra
	for i := 0; i < 2; i++ {
		if j := strings.LastIndexByte(s, '-'); j > 0 {
			s = s[:j]
		}
	}
	return s
}

// dnfResultOrder orders a transaction's packages the way the module's
// results usually list them: the requested packages in request order,
// then what came with them (dependencies), by name. (dnf4 iterates a
// set, so its order is not otherwise defined.)
func dnfResultOrder(list, requested []string) []string {
	sort.Strings(list)
	var out, rest []string
	used := map[string]bool{}
	for _, spec := range requested {
		for _, n := range list {
			name := nevraName(n)
			if !used[n] && (spec == name || strings.HasPrefix(spec, name+"-") || strings.HasPrefix(spec, name+".")) {
				out = append(out, n)
				used[n] = true
			}
		}
	}
	for _, n := range list {
		if !used[n] {
			rest = append(rest, n)
		}
	}
	return append(out, rest...)
}
