package modules

import (
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(iptablesModule, "iptables", "ansible.builtin.iptables")
}

var iptablesSpec = args.Spec{
	"chain":            {Required: true},
	"table":            {Default: "filter"},
	"protocol":         {},
	"source":           {},
	"destination":      {},
	"source_port":      {},
	"destination_port": {},
	"in_interface":     {},
	"out_interface":    {},
	"jump":             {},
	"ctstate":          {Type: "list"},
	"state":            {Default: "present", Choices: []string{"present", "absent"}},
	"ip_version":       {Default: "ipv4", Choices: []string{"ipv4", "ipv6"}},
	"comment":          {},
}

// iptablesModule manages a single rule, using -C (check) for idempotence.
func iptablesModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := iptablesSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	bin := "iptables"
	if p.Str("ip_version") == "ipv6" {
		bin = "ip6tables"
	}

	rule := buildIptablesRule(p)
	res := &agentproto.Result{Extra: map[string]any{"chain": p.Str("chain")}}

	// -C returns rc 0 if the rule exists.
	checkArgv := append([]string{"-t", p.Str("table"), "-C"}, rule...)
	_, checkErr := runOut(env, bin, checkArgv...)
	exists := checkErr == nil

	want := p.Str("state") == "present"
	if exists == want {
		return res
	}
	res.Changed = true
	if env.CheckMode {
		return res
	}
	op := "-A"
	if !want {
		op = "-D"
	}
	argv := append([]string{"-t", p.Str("table"), op}, rule...)
	if out, err := runOut(env, bin, argv...); err != nil {
		return agentproto.Fail("%s %s failed: %v: %s", bin, op, err, tail(out))
	}
	return res
}

// buildIptablesRule assembles the rule spec starting with the chain name.
func buildIptablesRule(p *args.Parsed) []string {
	rule := []string{p.Str("chain")}
	add := func(flag, val string) {
		if val != "" {
			rule = append(rule, flag, val)
		}
	}
	add("-p", p.Str("protocol"))
	add("-s", p.Str("source"))
	add("-d", p.Str("destination"))
	add("--sport", p.Str("source_port"))
	add("--dport", p.Str("destination_port"))
	add("-i", p.Str("in_interface"))
	add("-o", p.Str("out_interface"))
	if states := stringList(p.List("ctstate")); len(states) > 0 {
		rule = append(rule, "-m", "conntrack", "--ctstate", strings.Join(states, ","))
	}
	add("-j", p.Str("jump"))
	if c := p.Str("comment"); c != "" {
		rule = append(rule, "-m", "comment", "--comment", c)
	}
	return rule
}
