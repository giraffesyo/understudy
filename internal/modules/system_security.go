package modules

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(firewalldModule, "firewalld", "ansible.posix.firewalld")
	Register(selinuxModule, "selinux", "ansible.posix.selinux")
}

// --- firewalld (ansible.posix.firewalld) ---

var firewalldSpec = args.Spec{
	"icmp_block":           {},
	"icmp_block_inversion": {Type: "bool"},
	"service":              {},
	"protocol":             {},
	"port":                 {},
	"port_forward":         {Type: "list"},
	"rich_rule":            {},
	"zone":                 {},
	"immediate":            {Type: "bool", Default: false},
	"source":               {},
	"permanent":            {Type: "bool", Default: false},
	"state":                {Required: true, Choices: []string{"absent", "disabled", "enabled", "present"}},
	"timeout":              {Type: "int", Default: 0},
	"interface":            {},
	"forward":              {Type: "bool"},
	"masquerade":           {Type: "bool"},
	"offline":              {Type: "bool", Default: false},
	"target":               {Choices: []string{"default", "ACCEPT", "DROP", "%%REJECT%%"}},
}

var firewalldExclusive = []string{"icmp_block", "icmp_block_inversion", "service", "protocol", "port",
	"port_forward", "rich_rule", "interface", "forward", "masquerade", "source", "target"}

// fwCmd runs firewall-cmd, or firewall-offline-cmd when the daemon is
// down (the module's offline Firewall API edits the on-disk config).
type fwCmd struct {
	env     *RunEnv
	bin     string
	offline bool
}

func (f *fwCmd) run(permanent bool, a ...string) (int, string, string) {
	argv := []string{f.bin}
	if permanent && !f.offline {
		argv = append(argv, "--permanent")
	}
	return runCommand(f.env, append(argv, a...), cmdOpts{})
}

// fwError is the transaction's action_handler failure.
type fwError struct{ msg string }

func (e fwError) Error() string { return e.msg }

func (f *fwCmd) must(permanent bool, a ...string) (string, error) {
	rc, out, errOut := f.run(permanent, a...)
	if rc != 0 {
		msg := strings.TrimSpace(errOut)
		if msg == "" {
			msg = strings.TrimSpace(out)
		}
		return "", fwError{strings.TrimPrefix(msg, "Error: ")}
	}
	return out, nil
}

func (f *fwCmd) query(permanent bool, a ...string) (bool, error) {
	rc, out, errOut := f.run(permanent, a...)
	switch rc {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	msg := strings.TrimSpace(errOut)
	if msg == "" {
		msg = strings.TrimSpace(out)
	}
	return false, fwError{strings.TrimPrefix(msg, "Error: ")}
}

// fwTransaction is one FirewallTransaction subclass: get/set for the
// immediate (runtime) and permanent configurations.
type fwTransaction struct {
	enabledValues, disabledValues []string
	enabledMsg, disabledMsg       string
	getImmediate, getPermanent    func() (bool, error)
	setImmediate, setPermanent    func(enable bool) error
}

// run is FirewallTransaction.run(); exit reports check mode's early
// exit_json(changed=True).
func (t *fwTransaction) run(desired string, check, permanent, immediate bool) (changed bool, msgs []string, exit bool, err error) {
	want := 0
	switch {
	case containsStr(t.enabledValues, desired):
		want = 1
	case containsStr(t.disabledValues, desired):
		want = -1
	}
	step := func(isEnabled bool, set func(bool) error) error {
		if want == 1 && !isEnabled || want == -1 && isEnabled {
			if err := set(want == 1); err != nil {
				return err
			}
			changed = true
		}
		return nil
	}
	tail := func() {
		if changed && want == 1 && t.enabledMsg != "" {
			msgs = append(msgs, t.enabledMsg)
		}
		if changed && want == -1 && t.disabledMsg != "" {
			msgs = append(msgs, t.disabledMsg)
		}
	}
	switch {
	case immediate && permanent:
		p, err := t.getPermanent()
		if err != nil {
			return false, msgs, false, err
		}
		i, err := t.getImmediate()
		if err != nil {
			return false, msgs, false, err
		}
		msgs = append(msgs, "Permanent and Non-Permanent(immediate) operation")
		if want == 1 && (!p || !i) || want == -1 && (p || i) {
			if check {
				return true, msgs, true, nil
			}
		}
		if err := step(p, t.setPermanent); err != nil {
			return changed, msgs, false, err
		}
		if err := step(i, t.setImmediate); err != nil {
			return changed, msgs, false, err
		}
		tail()
	case permanent:
		p, err := t.getPermanent()
		if err != nil {
			return false, msgs, false, err
		}
		msgs = append(msgs, "Permanent operation")
		if want == 1 && !p || want == -1 && p {
			if check {
				return true, msgs, true, nil
			}
		}
		if err := step(p, t.setPermanent); err != nil {
			return changed, msgs, false, err
		}
		tail()
	case immediate:
		i, err := t.getImmediate()
		if err != nil {
			return false, msgs, false, err
		}
		msgs = append(msgs, "Non-permanent operation")
		if want == 1 && !i || want == -1 && i {
			if check {
				return true, msgs, true, nil
			}
		}
		if err := step(i, t.setImmediate); err != nil {
			return changed, msgs, false, err
		}
		tail()
	}
	return changed, msgs, false, nil
}

// firewalldModule ports ansible.posix.firewalld on top of firewall-cmd /
// firewall-offline-cmd.
func firewalldModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := firewalldSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if err := firewalldSpec.MutuallyExclusive(rawArgs, firewalldExclusive); err != nil {
		return agentproto.Fail("%v", err)
	}
	for _, rb := range [][2]string{{"interface", "zone"}, {"target", "zone"}, {"source", "permanent"}} {
		if p.Has(rb[0]) && !p.Has(rb[1]) {
			if _, given := rawArgs[rb[1]]; !given {
				return agentproto.Fail("missing parameter(s) required by '%s': %s", rb[0], rb[1])
			}
		}
	}
	fw := &fwCmd{env: env}
	if b, err := getBinPath("firewall-cmd"); err == nil {
		fw.bin = b
	} else {
		return agentproto.Fail("%s", missingRequiredLib(env, "firewall", "", "")+". Version 0.2.11 or newer required (0.3.9 or newer for offline operations)")
	}
	if rc, _, _ := runCommand(env, []string{fw.bin, "--state"}, cmdOpts{}); rc != 0 {
		// Firewalld is not running: permanent-only operations on disk.
		b, err := getBinPath("firewall-offline-cmd")
		if err != nil {
			return agentproto.Fail("firewalld service must be running, or try with offline=true")
		}
		fw.bin, fw.offline = b, true
	}

	permanent := p.Bool("permanent")
	immediate := p.Bool("immediate")
	state := p.Str("state")
	timeout := p.Int("timeout")
	if p.Bool("offline") {
		if !permanent {
			return agentproto.Fail("offline cannot be enabled unless permanent changes are allowed")
		}
		if fw.offline {
			immediate = false
		}
	}
	if !permanent && !immediate {
		immediate = true
	}
	if immediate && fw.offline {
		return agentproto.Fail("firewall is not currently running, unable to perform immediate actions without a running firewall daemon")
	}

	var port, portProto string
	if p.Has("port") {
		v := strings.TrimSpace(p.Str("port"))
		if parts := strings.Split(v, "/"); len(parts) == 2 {
			port, portProto = parts[0], parts[1]
		} else if len(parts) > 2 {
			return agentproto.Fail("too many values to unpack (expected 2)")
		}
		if portProto == "" {
			return agentproto.Fail("improper port format (missing protocol?)")
		}
	}
	var pf map[string]any
	pfToaddr := ""
	if p.Has("port_forward") {
		l := p.List("port_forward")
		if len(l) > 1 {
			return agentproto.Fail("Only one port forward supported at a time")
		}
		if len(l) == 1 {
			pf, _ = l[0].(map[string]any)
			if pf == nil {
				pf = map[string]any{}
			}
			for _, k := range []string{"port", "proto", "toport"} {
				if _, ok := pf[k]; !ok {
					switch k {
					case "port":
						return agentproto.Fail("port must be specified for port forward")
					case "proto":
						return agentproto.Fail("proto udp/tcp must be specified for port forward")
					default:
						return agentproto.Fail("toport must be specified for port forward")
					}
				}
			}
			if v, ok := pf["toaddr"]; ok {
				pfToaddr = pyStrValue(v)
			}
		}
	}
	modification := false
	for _, k := range firewalldExclusive {
		switch k {
		case "icmp_block_inversion", "forward", "masquerade":
			if p.Has(k) && p.Bool(k) {
				modification = true
			}
		case "port":
			if port != "" {
				modification = true
			}
		case "port_forward":
			if pf != nil {
				modification = true
			}
		default:
			if p.Str(k) != "" {
				modification = true
			}
		}
	}
	if modification && (state == "absent" || state == "present") && !p.Has("target") {
		return agentproto.Fail("absent and present state can only be used in zone level operations")
	}

	zone := p.Str("zone")
	if zone == "" {
		out, err := fw.must(false, "--get-default-zone")
		if err != nil {
			return agentproto.Fail("ERROR: Exception caught: %s", err)
		}
		zone = strings.TrimSpace(out)
	}
	z := "--zone=" + zone
	withTimeout := func(a string) []string {
		if timeout > 0 {
			return []string{z, a, "--timeout=" + strconv.FormatInt(timeout, 10)}
		}
		return []string{z, a}
	}
	// simple builds a transaction over --query/--add/--remove-<kind>[=value].
	simple := func(kind, value string, timed bool) *fwTransaction {
		arg := func(verb string) string {
			if value == "" {
				return "--" + verb + "-" + kind
			}
			return "--" + verb + "-" + kind + "=" + value
		}
		set := func(perm bool) func(bool) error {
			return func(enable bool) error {
				var a []string
				switch {
				case enable && timed && !perm:
					a = withTimeout(arg("add"))
				case enable:
					a = []string{z, arg("add")}
				default:
					a = []string{z, arg("remove")}
				}
				_, err := fw.must(perm, a...)
				return err
			}
		}
		return &fwTransaction{
			enabledValues: []string{"enabled"}, disabledValues: []string{"disabled"},
			getImmediate: func() (bool, error) { return fw.query(false, z, arg("query")) },
			getPermanent: func() (bool, error) { return fw.query(true, z, arg("query")) },
			setImmediate: set(false), setPermanent: set(true),
		}
	}
	boolState := func(flag bool) string {
		if (state == "enabled") == flag {
			return "enabled"
		}
		return "disabled"
	}

	type step struct {
		tx      *fwTransaction
		desired string
		after   string // message appended when changed
	}
	var steps []step
	if p.Has("icmp_block") {
		v := p.Str("icmp_block")
		steps = append(steps, step{simple("icmp-block", v, true), state, fmt.Sprintf("Changed icmp-block %s to %s", v, state)})
	}
	if p.Has("icmp_block_inversion") {
		steps = append(steps, step{simple("icmp-block-inversion", "", false), boolState(p.Bool("icmp_block_inversion")),
			fmt.Sprintf("Changed icmp-block-inversion %s to %s", pyValueRepr(p.Bool("icmp_block_inversion")), state)})
	}
	if p.Has("service") {
		v := p.Str("service")
		steps = append(steps, step{simple("service", v, true), state, fmt.Sprintf("Changed service %s to %s", v, state)})
	}
	if p.Has("protocol") {
		v := p.Str("protocol")
		steps = append(steps, step{simple("protocol", v, true), state, fmt.Sprintf("Changed protocol %s to %s", v, state)})
	}
	if p.Has("source") {
		v := p.Str("source")
		tx := simple("source", v, false)
		tx.enabledMsg = fmt.Sprintf("Added %s to zone %s", v, zone)
		tx.disabledMsg = fmt.Sprintf("Removed %s from zone %s", v, zone)
		steps = append(steps, step{tx, state, ""})
	}
	if port != "" {
		steps = append(steps, step{simple("port", port+"/"+portProto, true), state, fmt.Sprintf("Changed port %s/%s to %s", port, portProto, state)})
	}
	if pf != nil {
		spec := fmt.Sprintf("port=%s:proto=%s:toport=%s", pyStrValue(pf["port"]), pyStrValue(pf["proto"]), pyStrValue(pf["toport"]))
		if pfToaddr != "" {
			spec += ":toaddr=" + pfToaddr
		}
		steps = append(steps, step{simple("forward-port", spec, true), state, fmt.Sprintf("Changed port_forward port=%s:proto=%s:toport=%s:toaddr=%s to %s",
			pyStrValue(pf["port"]), pyStrValue(pf["proto"]), pyStrValue(pf["toport"]), pfToaddr, state)})
	}
	if p.Has("rich_rule") {
		v := p.Str("rich_rule")
		steps = append(steps, step{simple("rich-rule", v, true), state, fmt.Sprintf("Changed rich_rule %s to %s", v, state)})
	}
	if p.Has("interface") {
		v := p.Str("interface")
		tx := simple("interface", v, false)
		tx.setImmediate = func(enable bool) error {
			a := []string{z, "--remove-interface=" + v}
			if enable {
				a = []string{z, "--change-interface=" + v}
			}
			_, err := fw.must(false, a...)
			return err
		}
		tx.setPermanent = func(enable bool) error {
			a := []string{z, "--remove-interface=" + v}
			if enable {
				a = []string{z, "--change-interface=" + v}
			}
			_, err := fw.must(true, a...)
			return err
		}
		tx.enabledMsg = fmt.Sprintf("Changed %s to zone %s", v, zone)
		tx.disabledMsg = fmt.Sprintf("Removed %s from zone %s", v, zone)
		steps = append(steps, step{tx, state, ""})
	}
	if p.Has("forward") {
		tx := simple("forward", "", false)
		tx.enabledMsg = "Added forward to zone " + zone
		tx.disabledMsg = "Removed forward from zone " + zone
		steps = append(steps, step{tx, boolState(p.Bool("forward")), ""})
	}
	if p.Has("masquerade") {
		tx := simple("masquerade", "", false)
		tx.enabledMsg = "Added masquerade to zone " + zone
		tx.disabledMsg = "Removed masquerade from zone " + zone
		steps = append(steps, step{tx, boolState(p.Bool("masquerade")), ""})
	}
	notPermanent := "Zone operations must be permanent. " +
		"Make sure you didn't set the 'permanent' flag to 'false' or the 'immediate' flag to 'true'."
	zoneOnly := func() (bool, error) { return false, fwError{"\x00" + notPermanent} }
	zoneOnlySet := func(bool) error { return fwError{"\x00" + notPermanent} }
	if p.Has("target") {
		target := p.Str("target")
		steps = append(steps, step{&fwTransaction{
			enabledValues: []string{"present", "enabled"}, disabledValues: []string{"absent", "disabled"},
			enabledMsg:   fmt.Sprintf("Set zone %s target to %s", zone, target),
			disabledMsg:  fmt.Sprintf("Reset zone %s target to default", zone),
			getImmediate: zoneOnly, setImmediate: zoneOnlySet,
			getPermanent: func() (bool, error) {
				out, err := fw.must(true, z, "--get-target")
				return strings.TrimSpace(out) == target, err
			},
			setPermanent: func(enable bool) error {
				t := "default"
				if enable {
					t = target
				}
				_, err := fw.must(true, z, "--set-target="+t)
				return err
			},
		}, state, ""})
	}
	if !modification && (state == "absent" || state == "present") {
		steps = append(steps, step{&fwTransaction{
			enabledValues: []string{"present"}, disabledValues: []string{"absent"},
			enabledMsg: "Added zone " + zone, disabledMsg: "Removed zone " + zone,
			getImmediate: zoneOnly, setImmediate: zoneOnlySet,
			getPermanent: func() (bool, error) {
				out, err := fw.must(true, "--get-zones")
				return containsStr(strings.Fields(out), zone), err
			},
			setPermanent: func(enable bool) error {
				a := "--delete-zone=" + zone
				if enable {
					a = "--new-zone=" + zone
				}
				_, err := fw.must(true, a)
				return err
			},
		}, state, fmt.Sprintf("Changed zone %s to %s", zone, state)})
	}

	changed := false
	var msgs []string
	for _, s := range steps {
		c, m, exit, err := s.tx.run(s.desired, env.CheckMode, permanent, immediate)
		msgs = append(msgs, m...)
		if exit {
			return &agentproto.Result{Changed: true}
		}
		if err != nil {
			e := err.Error()
			if strings.HasPrefix(e, "\x00") {
				return agentproto.Fail("%s", e[1:])
			}
			if strings.Contains(e, "INVALID_SERVICE") {
				msgs = append(msgs, "Services are defined by port/tcp relationship and named as they are in /etc/services (on most systems)")
			}
			if len(msgs) > 0 {
				return agentproto.Fail("ERROR: Exception caught: %s %s", e, strings.Join(msgs, ", "))
			}
			return agentproto.Fail("ERROR: Exception caught: %s", e)
		}
		changed = c
		if c && s.after != "" {
			msgs = append(msgs, s.after)
		}
	}
	if fw.offline {
		msgs = append(msgs, "(offline operation: only on-disk configs were altered)")
	}
	return &agentproto.Result{Changed: changed, Msg: strings.Join(msgs, ", "),
		Extra: map[string]any{"msg": strings.Join(msgs, ", ")}}
}

// --- selinux (ansible.posix.selinux) ---

var selinuxSpec = args.Spec{
	"policy":              {},
	"state":               {Required: true, Choices: []string{"enforcing", "permissive", "disabled"}},
	"configfile":          {Default: "/etc/selinux/config", Aliases: []string{"conf", "file"}},
	"update_kernel_param": {Type: "bool", Default: false},
}

const selinuxFS = "/sys/fs/selinux"

// selinuxConfigValue is get_config_state/get_config_policy: the value of
// the first KEY= line, or "" (None) when absent.
func selinuxConfigValue(lines []string, key string) (string, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			return strings.TrimSpace(strings.Split(l, "=")[1]), true
		}
	}
	return "", false
}

// getFileLines is facts.utils.get_file_lines(strip=False).
func getFileLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	return pySplitLines(string(data))
}

func selinuxSetConfig(key, value, configfile string) error {
	re := regexp.MustCompile(`^` + key + `=.*`)
	var b bytes.Buffer
	found := false
	for _, line := range getFileLines(configfile) {
		if re.MatchString(line) {
			found = true
		}
		b.WriteString(re.ReplaceAllLiteralString(line, key+"="+value) + "\n")
	}
	if !found {
		b.WriteString(key + "=" + value + "\n")
	}
	return fsutil.AtomicWrite(configfile, &b, 0o644)
}

// selinuxModule ports ansible.posix.selinux, reading and setting the
// runtime state through selinuxfs as libselinux does.
func selinuxModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := selinuxSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if _, err := os.Stat("/etc/selinux"); err != nil {
		return agentproto.Fail("%s", missingRequiredLib(env, "libselinux-python", "", ""))
	}
	configfile := p.Str("configfile")
	policy, hasPolicy := p.Str("policy"), p.Str("policy") != ""
	state := p.Str("state")
	updateKernel := p.Bool("update_kernel_param")
	check := env.CheckMode
	var msgs, warnings []any
	changed := false

	// is_selinux_enabled / security_getenforce / selinux_getpolicytype.
	enforceData, enforceErr := os.ReadFile(selinuxFS + "/enforce")
	runtimeEnabled := enforceErr == nil
	runtimeState := "disabled"
	if runtimeEnabled {
		if strings.TrimSpace(string(enforceData)) == "1" {
			runtimeState = "enforcing"
		} else {
			runtimeState = "permissive"
		}
	}
	runtimePolicy, ok := selinuxConfigValue(getFileLines("/etc/selinux/config"), "SELINUXTYPE")
	if !ok {
		runtimePolicy = "targeted"
	}

	if info, err := os.Stat(configfile); err != nil || !info.Mode().IsRegular() {
		r := agentproto.Fail("Unable to find file %s", configfile)
		r.Extra = map[string]any{"details": "Please install SELinux-policy package, " +
			"if this package is not installed previously."}
		return r
	}
	configPolicy, hasConfigPolicy := selinuxConfigValue(getFileLines(configfile), "SELINUXTYPE")
	configState, hasConfigState := selinuxConfigValue(getFileLines(configfile), "SELINUX")
	pyNone := func(s string, ok bool) string {
		if !ok {
			return "None"
		}
		return s
	}

	var grubby string
	var kernelEnabled *bool
	if updateKernel {
		b, err := getBinPath("grubby")
		if err != nil {
			r := agentproto.Fail("'grubby' command not found on host")
			r.Extra = map[string]any{"details": "In order to update the kernel command line" +
				"enabled/disabled setting, the grubby package" +
				"needs to be present on the system."}
			return r
		}
		grubby = b
		rc, out, _ := runCommand(env, []string{grubby, "--info=ALL"}, cmdOpts{})
		if rc != 0 {
			return agentproto.Fail("unable to run grubby")
		}
		allEnabled, allDisabled := true, true
		argsRe := regexp.MustCompile(`^args="(.*)"$`)
		for _, line := range strings.Split(out, "\n") {
			m := argsRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if containsStr(strings.Split(m[1], " "), "selinux=0") {
				allEnabled = false
			} else {
				allDisabled = false
			}
		}
		if allDisabled != allEnabled {
			kernelEnabled = &allEnabled
		}
	}

	policyNone := false
	if state != "disabled" {
		if !hasPolicy {
			return agentproto.Fail("Policy is required if state is not 'disabled'")
		}
	} else if !hasPolicy {
		policy, policyNone = configPolicy, !hasConfigPolicy
	}

	if policyNone || policy != runtimePolicy {
		if check {
			return &agentproto.Result{Changed: true}
		}
		msgs = append(msgs, fmt.Sprintf("Running SELinux policy changed from '%s' to '%s'", runtimePolicy, pyNone(policy, !policyNone)))
		changed = true
	}
	if policyNone != !hasConfigPolicy || policy != configPolicy {
		if check {
			return &agentproto.Result{Changed: true}
		}
		if _, err := os.Stat("/etc/selinux/" + pyNone(policy, !policyNone) + "/policy"); err != nil {
			return agentproto.Fail("Policy %s does not exist in /etc/selinux/", pyNone(policy, !policyNone))
		}
		if err := selinuxSetConfig("SELINUXTYPE", policy, configfile); err != nil {
			return agentproto.Fail("%v", err)
		}
		msgs = append(msgs, fmt.Sprintf("SELinux policy configuration in '%s' changed from '%s' to '%s'",
			configfile, pyNone(configPolicy, hasConfigPolicy), pyNone(policy, !policyNone)))
		changed = true
	}

	rebootRequired := false
	setenforce := func(v string) error {
		return os.WriteFile(selinuxFS+"/enforce", []byte(v), 0o644)
	}
	if state != runtimeState {
		if runtimeEnabled {
			if state == "disabled" {
				if runtimeState != "permissive" {
					if !check {
						if err := setenforce("0"); err != nil {
							return agentproto.Fail("%s", pyStrOSError(err, selinuxFS+"/enforce"))
						}
					}
					warnings = append(warnings, fmt.Sprintf("SELinux state temporarily changed from '%s' to 'permissive'. State change will take effect next reboot.", runtimeState))
					changed = true
				} else {
					warnings = append(warnings, "SELinux state change will take effect next reboot")
				}
				rebootRequired = true
			} else {
				if !check {
					v := "0"
					if state == "enforcing" {
						v = "1"
					}
					if err := setenforce(v); err != nil {
						return agentproto.Fail("%s", pyStrOSError(err, selinuxFS+"/enforce"))
					}
				}
				msgs = append(msgs, fmt.Sprintf("SELinux state changed from '%s' to '%s'", runtimeState, state))
				changed = true
			}
		} else {
			warnings = append(warnings, fmt.Sprintf("Reboot is required to set SELinux state to '%s'", state))
			rebootRequired = true
		}
	}
	if !hasConfigState || state != configState {
		if !check {
			if err := selinuxSetConfig("SELINUX", state, configfile); err != nil {
				return agentproto.Fail("%v", err)
			}
		}
		msgs = append(msgs, fmt.Sprintf("Config SELinux state changed from '%s' to '%s'", pyNone(configState, hasConfigState), state))
		changed = true
	}
	requested := state == "enforcing" || state == "permissive"
	if updateKernel && (kernelEnabled == nil || *kernelEnabled != requested) {
		if !check {
			op := "--args"
			if requested {
				op = "--remove-args"
			}
			if rc, _, _ := runCommand(env, []string{grubby, "--update-kernel=ALL", op, "selinux=0"}, cmdOpts{}); rc != 0 {
				if requested {
					return agentproto.Fail("unable to remove selinux=0 from kernel config")
				}
				return agentproto.Fail("unable to add selinux=0 to kernel config")
			}
		}
		from, to := "enabled", "disabled"
		if requested {
			from, to = "disabled", "enabled"
		}
		if kernelEnabled == nil {
			from = "<inconsistent>"
		}
		msgs = append(msgs, fmt.Sprintf("Kernel SELinux state changed from '%s' to '%s'", from, to))
		changed = true
	}

	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.(string)
	}
	var pol any = policy
	if policyNone {
		pol = nil
	}
	res := &agentproto.Result{Changed: changed, Msg: strings.Join(parts, ", "), Extra: map[string]any{
		"msg": strings.Join(parts, ", "), "configfile": configfile, "policy": pol, "state": state, "reboot_required": rebootRequired}}
	if len(warnings) > 0 {
		res.Extra["warnings"] = warnings
	}
	return res
}
