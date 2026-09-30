package modules

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(iptablesModule, "iptables", "ansible.builtin.iptables")
}

var iptablesSpec = args.Spec{
	"table":               {Default: "filter", Choices: []string{"filter", "nat", "mangle", "raw", "security"}},
	"state":               {Default: "present", Choices: []string{"absent", "present"}},
	"action":              {Default: "append", Choices: []string{"append", "insert"}},
	"ip_version":          {Default: "ipv4", Choices: []string{"ipv4", "ipv6", "both"}},
	"chain":               {},
	"rule_num":            {},
	"protocol":            {},
	"wait":                {},
	"source":              {},
	"to_source":           {},
	"destination":         {},
	"to_destination":      {},
	"match":               {Type: "list", Default: []any{}},
	"tcp_flags":           {Type: "dict"},
	"jump":                {},
	"gateway":             {},
	"log_prefix":          {},
	"log_level":           {Choices: []string{"0", "1", "2", "3", "4", "5", "6", "7", "emerg", "alert", "crit", "error", "warning", "notice", "info", "debug"}},
	"goto":                {},
	"in_interface":        {},
	"out_interface":       {},
	"fragment":            {},
	"set_counters":        {},
	"source_port":         {},
	"destination_port":    {},
	"destination_ports":   {Type: "list", Default: []any{}},
	"to_ports":            {},
	"set_dscp_mark":       {},
	"set_dscp_mark_class": {},
	"comment":             {},
	"ctstate":             {Type: "list", Default: []any{}},
	"src_range":           {},
	"dst_range":           {},
	"match_set":           {},
	"match_set_flags":     {Choices: []string{"src", "dst", "src,dst", "dst,src", "src,src", "dst,dst"}},
	"limit":               {},
	"limit_burst":         {},
	"uid_owner":           {},
	"gid_owner":           {},
	"reject_with":         {},
	"icmp_type":           {},
	"syn":                 {Default: "ignore", Choices: []string{"ignore", "match", "negate"}},
	"flush":               {Type: "bool", Default: false},
	"policy":              {Choices: []string{"ACCEPT", "DROP", "QUEUE", "RETURN"}},
	"chain_management":    {Type: "bool", Default: false},
	"numeric":             {Type: "bool", Default: false},
}

var iptablesICMPTypeOptions = map[string]string{
	"ipv4": "--icmp-type",
	"ipv6": "--icmpv6-type",
	"both": "--icmp-type --icmpv6-type",
}

// iptablesParams is the module's params dict: optional strings are nil
// when unset (the Python code distinguishes None from "").
type iptablesParams struct {
	p     *args.Parsed
	str   map[string]*string
	lists map[string][]string
}

func newIptablesParams(p *args.Parsed) *iptablesParams {
	ip := &iptablesParams{p: p, str: map[string]*string{}, lists: map[string][]string{}}
	for name, def := range iptablesSpec {
		switch def.Type {
		case "list":
			ip.lists[name] = iptablesStrList(p.List(name))
		case "", "str":
			if p.Has(name) {
				s := p.Str(name)
				ip.str[name] = &s
			}
		}
	}
	return ip
}

func (ip *iptablesParams) s(name string) string {
	if v := ip.str[name]; v != nil {
		return *v
	}
	return ""
}

func (ip *iptablesParams) set(name string) bool { return ip.str[name] != nil }

func (ip *iptablesParams) truthy(name string) bool { return ip.s(name) != "" }

// iptablesAppendParam is append_param(): "!value" negates the flag.
func iptablesAppendParam(rule []string, param *string, flag string) []string {
	if param == nil {
		return rule
	}
	if strings.HasPrefix(*param, "!") {
		return append(rule, "!", flag, (*param)[1:])
	}
	return append(rule, flag, *param)
}

func iptablesAppendMatchFlag(rule []string, param, flag string, negatable bool) []string {
	if param == "match" {
		return append(rule, flag)
	}
	if negatable && param == "negate" {
		return append(rule, "!", flag)
	}
	return rule
}

func iptablesAppendCSV(rule []string, param []string, flag string) []string {
	if len(param) > 0 {
		return append(rule, flag, strings.Join(param, ","))
	}
	return rule
}

func iptablesAppendMatch(rule []string, param bool, match string, loaded map[string]bool) []string {
	if param && !loaded[match] {
		loaded[match] = true
		return append(rule, "-m", match)
	}
	return rule
}

func iptablesAppendJump(rule []string, param bool, jump string) []string {
	if param {
		return append(rule, "-j", jump)
	}
	return rule
}

// construct is construct_rule(): the rule spec, in the Python order.
func (ip *iptablesParams) construct() []string {
	rule := []string{}
	rule = iptablesAppendParam(rule, ip.str["protocol"], "-p")
	rule = iptablesAppendParam(rule, ip.str["source"], "-s")
	rule = iptablesAppendParam(rule, ip.str["destination"], "-d")
	match := ip.lists["match"]
	for i := range match {
		rule = iptablesAppendParam(rule, &match[i], "-m")
	}
	loaded := map[string]bool{}
	for _, m := range match {
		loaded[m] = true
	}
	if tf := ip.p.Dict("tcp_flags"); len(tf) > 0 {
		flags, hasFlags := tf["flags"]
		set, hasSet := tf["flags_set"]
		if hasFlags && hasSet {
			rule = append(rule, "--tcp-flags", strings.Join(iptablesStrList(flags), ","), strings.Join(iptablesStrList(set), ","))
		}
	}
	rule = iptablesAppendParam(rule, ip.str["jump"], "-j")
	if strings.ToLower(ip.s("jump")) == "tee" {
		rule = iptablesAppendParam(rule, ip.str["gateway"], "--gateway")
	}
	rule = iptablesAppendParam(rule, ip.str["log_prefix"], "--log-prefix")
	rule = iptablesAppendParam(rule, ip.str["log_level"], "--log-level")
	rule = iptablesAppendParam(rule, ip.str["to_destination"], "--to-destination")
	dports := ip.lists["destination_ports"]
	rule = iptablesAppendMatch(rule, len(dports) > 0, "multiport", loaded)
	rule = iptablesAppendCSV(rule, dports, "--dports")
	rule = iptablesAppendParam(rule, ip.str["to_source"], "--to-source")
	rule = iptablesAppendParam(rule, ip.str["goto"], "-g")
	rule = iptablesAppendParam(rule, ip.str["in_interface"], "-i")
	rule = iptablesAppendParam(rule, ip.str["out_interface"], "-o")
	rule = iptablesAppendParam(rule, ip.str["fragment"], "-f")
	rule = iptablesAppendParam(rule, ip.str["set_counters"], "-c")
	rule = iptablesAppendParam(rule, ip.str["source_port"], "--source-port")
	rule = iptablesAppendParam(rule, ip.str["destination_port"], "--destination-port")
	rule = iptablesAppendParam(rule, ip.str["to_ports"], "--to-ports")
	rule = iptablesAppendParam(rule, ip.str["set_dscp_mark"], "--set-dscp")
	if ip.truthy("set_dscp_mark") && strings.ToLower(ip.s("jump")) != "dscp" {
		rule = iptablesAppendJump(rule, true, "DSCP")
	}
	rule = iptablesAppendParam(rule, ip.str["set_dscp_mark_class"], "--set-dscp-class")
	if ip.truthy("set_dscp_mark_class") && strings.ToLower(ip.s("jump")) != "dscp" {
		rule = iptablesAppendJump(rule, true, "DSCP")
	}
	rule = iptablesAppendMatchFlag(rule, ip.p.Str("syn"), "--syn", true)
	ctstate := ip.lists["ctstate"]
	switch {
	case containsString(match, "conntrack"):
		rule = iptablesAppendCSV(rule, ctstate, "--ctstate")
	case containsString(match, "state"):
		rule = iptablesAppendCSV(rule, ctstate, "--state")
	case len(ctstate) > 0:
		rule = iptablesAppendMatch(rule, true, "conntrack", loaded)
		rule = iptablesAppendCSV(rule, ctstate, "--ctstate")
	}
	if containsString(match, "iprange") {
		rule = iptablesAppendParam(rule, ip.str["src_range"], "--src-range")
		rule = iptablesAppendParam(rule, ip.str["dst_range"], "--dst-range")
	} else if ip.truthy("src_range") || ip.truthy("dst_range") {
		rule = iptablesAppendMatch(rule, true, "iprange", loaded)
		rule = iptablesAppendParam(rule, ip.str["src_range"], "--src-range")
		rule = iptablesAppendParam(rule, ip.str["dst_range"], "--dst-range")
	}
	// append_match_flag(rule, 'match', match_set_flags, False) appends
	// the flags value itself.
	if containsString(match, "set") {
		rule = iptablesAppendParam(rule, ip.str["match_set"], "--match-set")
		if ip.set("match_set_flags") {
			rule = append(rule, ip.s("match_set_flags"))
		}
	} else if ip.truthy("match_set") {
		rule = iptablesAppendMatch(rule, true, "set", loaded)
		rule = iptablesAppendParam(rule, ip.str["match_set"], "--match-set")
		if ip.set("match_set_flags") {
			rule = append(rule, ip.s("match_set_flags"))
		}
	}
	rule = iptablesAppendMatch(rule, ip.truthy("limit") || ip.truthy("limit_burst"), "limit", loaded)
	rule = iptablesAppendParam(rule, ip.str["limit"], "--limit")
	rule = iptablesAppendParam(rule, ip.str["limit_burst"], "--limit-burst")
	for _, owner := range []string{"uid_owner", "gid_owner"} {
		flag := "--" + strings.ReplaceAll(owner, "_", "-")
		rule = iptablesAppendMatch(rule, ip.truthy(owner), "owner", loaded)
		rule = iptablesAppendMatchFlag(rule, ip.s(owner), flag, true)
		rule = iptablesAppendParam(rule, ip.str[owner], flag)
	}
	if !ip.set("jump") {
		rule = iptablesAppendJump(rule, ip.truthy("reject_with"), "REJECT")
		rule = iptablesAppendJump(rule, ip.truthy("set_dscp_mark_class"), "DSCP")
		rule = iptablesAppendJump(rule, ip.truthy("set_dscp_mark"), "DSCP")
	}
	rule = iptablesAppendParam(rule, ip.str["reject_with"], "--reject-with")
	rule = iptablesAppendParam(rule, ip.str["icmp_type"], iptablesICMPTypeOptions[ip.p.Str("ip_version")])
	rule = iptablesAppendMatch(rule, ip.truthy("comment"), "comment", loaded)
	rule = iptablesAppendParam(rule, ip.str["comment"], "--comment")
	return rule
}

func iptablesStrList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, pyStrValue(e))
		}
		return out
	case string:
		return strings.Split(t, ",")
	}
	return nil
}

// push is push_arguments().
func (ip *iptablesParams) push(bin, action string, makeRule bool) []string {
	cmd := []string{bin, "-t", ip.p.Str("table"), action}
	if ip.set("chain") { // run_command drops None arguments
		cmd = append(cmd, ip.s("chain"))
	}
	if action == "-I" && ip.truthy("rule_num") {
		cmd = append(cmd, ip.s("rule_num"))
	}
	if ip.truthy("wait") {
		cmd = append(cmd, "-w", ip.s("wait"))
	}
	if makeRule {
		cmd = append(cmd, ip.construct()...)
	}
	return cmd
}

// iptablesRun is run_command(): with checkRC a non-zero exit fails the
// module the way AnsibleModule reports it.
func iptablesRun(env *RunEnv, cmd []string, checkRC bool) (int, string, *agentproto.Result) {
	rc, stdout, stderr := runCommand(env, cmd, cmdOpts{})
	if rc != 0 && checkRC {
		quoted := make([]string, len(cmd))
		for i, a := range cmd {
			quoted[i] = shlexQuote(a)
		}
		return rc, stdout, &agentproto.Result{
			Failed: true,
			Msg:    strings.TrimRight(stderr, " \t\n\r\f\v"),
			RC:     agentproto.IntPtr(rc),
			Stdout: stdout,
			Stderr: stderr,
			Extra:  map[string]any{"cmd": strings.Join(quoted, " ")},
		}
	}
	return rc, stdout, nil
}

var iptablesPolicyRe = regexp.MustCompile(`\(policy ([A-Z]+)\)`)

// iptablesValidate applies the module's mutually_exclusive, required_by
// and required_if rules.
func iptablesValidate(raw map[string]any, p *args.Parsed) error {
	if err := iptablesSpec.MutuallyExclusive(raw, []string{"set_dscp_mark", "set_dscp_mark_class"}, []string{"flush", "policy"}); err != nil {
		return err
	}
	for _, k := range []string{"set_dscp_mark", "set_dscp_mark_class"} {
		if p.Has(k) && !p.Has("jump") {
			return fmt.Errorf("missing parameter(s) required by '%s': jump", k)
		}
	}
	for _, j := range []string{"TEE", "tee"} {
		if p.Has("jump") && p.Str("jump") == j && !p.Has("gateway") {
			return fmt.Errorf("jump is %s but all of the following are missing: gateway", j)
		}
	}
	if !p.Bool("flush") && !p.Has("chain") {
		return fmt.Errorf("flush is False but all of the following are missing: chain")
	}
	return nil
}

// iptablesModule ports ansible.builtin.iptables.
func iptablesModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := iptablesSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if err := iptablesValidate(rawArgs, p); err != nil {
		return agentproto.Fail("%v", err)
	}
	ip := newIptablesParams(p)

	var chain, wait any
	if ip.set("chain") {
		chain = ip.s("chain")
	}
	if ip.set("wait") {
		wait = ip.s("wait")
	}
	rule := strings.Join(ip.construct(), " ")
	extra := map[string]any{
		"ip_version":       p.Str("ip_version"),
		"table":            p.Str("table"),
		"chain":            chain,
		"flush":            p.Bool("flush"),
		"rule":             rule,
		"state":            p.Str("state"),
		"chain_management": p.Bool("chain_management"),
		"wait":             wait,
	}

	versions := []string{p.Str("ip_version")}
	if versions[0] == "both" {
		versions = []string{"ipv4", "ipv6"}
	}
	var paths []string
	for _, v := range versions {
		name := "iptables"
		if v != "ipv4" {
			name = "ip6tables"
		}
		path, err := getBinPath(name)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		paths = append(paths, path)
	}

	changedAny := false
	origWait := ip.str["wait"]
	for _, path := range paths {
		if ip.truthy("log_prefix") || ip.truthy("log_level") {
			if !ip.set("jump") {
				j := "LOG"
				ip.str["jump"] = &j
			} else if ip.s("jump") != "LOG" {
				return &agentproto.Result{Failed: true, Msg: "Logging options can only be used with the LOG jump target."}
			}
		}

		// -w support depends on the iptables version.
		_, out, fail := iptablesRun(env, []string{path, "--version"}, true)
		if fail != nil {
			return fail
		}
		version := ""
		if parts := strings.SplitN(out, "v", 3); len(parts) > 1 {
			version = strings.TrimRight(parts[1], "\n")
		}
		ip.str["wait"] = origWait
		if !looseVersionLess(version, "1.4.20") {
			if looseVersionLess(version, "1.6.0") {
				empty := ""
				ip.str["wait"] = &empty
			}
		} else {
			ip.str["wait"] = nil
		}

		switch {
		case p.Bool("flush"):
			changedAny = true
			if !env.CheckMode {
				if _, _, fail := iptablesRun(env, ip.push(path, "-F", false), true); fail != nil {
					return fail
				}
			}
		case ip.truthy("policy"):
			cmd := ip.push(path, "-L", false)
			if p.Bool("numeric") {
				cmd = append(cmd, "--numeric")
			}
			_, out, fail := iptablesRun(env, cmd, true)
			if fail != nil {
				return fail
			}
			header := strings.SplitN(out, "\n", 2)[0]
			m := iptablesPolicyRe.FindStringSubmatch(header)
			if m == nil {
				return &agentproto.Result{Failed: true, Msg: "Can't detect current policy"}
			}
			changed := m[1] != ip.s("policy")
			changedAny = changedAny || changed
			if changed && !env.CheckMode {
				if _, _, fail := iptablesRun(env, append(ip.push(path, "-P", false), ip.s("policy")), true); fail != nil {
					return fail
				}
			}
		case rule == "":
			cmd := ip.push(path, "-L", false)
			if p.Bool("numeric") {
				cmd = append(cmd, "--numeric")
			}
			rc, _, _ := iptablesRun(env, cmd, false)
			present := rc == 0
			if p.Str("state") == "absent" {
				changedAny = changedAny || present
				if present && p.Bool("chain_management") && !env.CheckMode {
					if _, _, fail := iptablesRun(env, ip.push(path, "-X", false), true); fail != nil {
						return fail
					}
				}
			} else {
				changedAny = changedAny || !present
				if !present && p.Bool("chain_management") && !env.CheckMode {
					if _, _, fail := iptablesRun(env, ip.push(path, "-N", false), true); fail != nil {
						return fail
					}
				}
			}
		default:
			rc, _, _ := iptablesRun(env, ip.push(path, "-C", true), false)
			present := rc == 0
			want := p.Str("state") == "present"
			if present == want {
				continue
			}
			changedAny = true
			if env.CheckMode {
				continue
			}
			op := "-D"
			if want {
				op = "-A"
				if p.Str("action") == "insert" {
					op = "-I"
				}
			}
			if _, _, fail := iptablesRun(env, ip.push(path, op, true), true); fail != nil {
				return fail
			}
		}
	}
	return &agentproto.Result{Changed: changedAny, Extra: extra}
}
