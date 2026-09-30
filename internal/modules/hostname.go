package modules

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// This file ports ansible.builtin.hostname: a strategy per platform (or
// per `use`) reads the current and permanent hostnames and sets both.

func init() {
	names := []string{"hostname", "ansible.builtin.hostname"}
	Register(hostnameModule, names...)
	for _, n := range names {
		specs[n] = hostnameSpec
	}
}

var hostnameSpec = args.Spec{
	"name": {Required: true},
	"use": {Choices: []string{"alpine", "debian", "freebsd", "generic", "macos", "macosx", "darwin",
		"openbsd", "openrc", "redhat", "sles", "solaris", "systemd"}},
}

// hostnameStrats is the module's STRATS: `use` -> strategy class.
var hostnameStrats = map[string]string{
	"alpine": "Alpine", "debian": "Systemd", "freebsd": "FreeBSD", "generic": "Base",
	"macos": "Darwin", "macosx": "Darwin", "darwin": "Darwin", "openbsd": "OpenBSD",
	"openrc": "OpenRC", "redhat": "RedHat", "sles": "SLES", "solaris": "Solaris", "systemd": "Systemd",
}

// hostnameLinuxDists maps get_distribution() to the Hostname subclass's
// strategy on Linux.
var hostnameLinuxDists = map[string]string{
	"Redhat": "RedHat", "Centos": "RedHat", "Anolis": "RedHat", "Cloudlinuxserver": "RedHat",
	"Cloudlinux": "RedHat", "Alinux": "RedHat", "Scientific": "RedHat", "Oracle": "RedHat",
	"Virtuozzo": "RedHat", "Amazon": "RedHat", "Altlinux": "RedHat", "Eurolinux": "RedHat",
	"Debian": "File", "Kylin": "File", "Cumulus-linux": "File", "Kali": "File", "Parrot": "File",
	"Ubuntu": "File", "Linuxmint": "File", "Linaro": "File", "Devuan": "File", "Raspbian": "File",
	"Uos": "File", "Deepin": "File", "Neon": "File", "Void": "File", "Pop": "File",
	"Gentoo": "OpenRC", "Alpine": "Alpine",
}

// hostnameAbort carries a fail_json out of a strategy.
type hostnameAbort struct{ res *agentproto.Result }

type hostnameStrategy struct {
	env     *RunEnv
	kind    string
	name    string
	cmd     string // hostname / hostnamectl / scutil binary
	file    string
	changed bool
	unimpl  string // UnimplementedStrategy's message
}

func (s *hostnameStrategy) fail(format string, a ...any) {
	panic(hostnameAbort{agentproto.Fail(format, a...)})
}

func (s *hostnameStrategy) run(argv ...string) (int, string, string) {
	return runCommand(s.env, argv, cmdOpts{})
}

func (s *hostnameStrategy) mustRun(argv ...string) string {
	rc, out, errOut := s.run(argv...)
	if rc != 0 {
		s.fail("Command failed rc=%d, out=%s, err=%s", rc, out, errOut)
	}
	return out
}

func (s *hostnameStrategy) binPath(name string) string {
	p, err := getBinPath(name)
	if err != nil {
		s.fail("%v", err)
	}
	return p
}

// fileContent is get_file_content(path, default, strip=True).
func fileContentStripped(path string) (string, bool) {
	if !pathExists(path) || !accessOK(path, 4) {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	d := strings.TrimSpace(string(b))
	if d == "" {
		return "", false
	}
	return d, true
}

// fileLines is get_file_lines(path): the stripped content's lines.
func fileLines(path string) []string {
	d, ok := fileContentStripped(path)
	if !ok {
		return nil
	}
	return pySplitlines(d)
}

func (s *hostnameStrategy) getCurrent() string {
	switch s.kind {
	case "Unimplemented":
		s.fail("%s", s.unimpl)
	case "Systemd":
		return strings.TrimSpace(s.mustRun(s.cmd, "--transient", "status"))
	case "OpenBSD", "FreeBSD":
		return strings.TrimSpace(s.mustRun(s.cmd))
	case "Darwin":
		rc, out, errOut := s.run(s.cmd, "--get", "HostName")
		if rc != 0 && !strings.Contains(errOut, "HostName: not set") {
			s.fail("Failed to get current hostname rc=%d, out=%s, err=%s", rc, out, errOut)
		}
		return strings.TrimSpace(out)
	}
	return s.getPermanent()
}

func (s *hostnameStrategy) getPermanent() string {
	switch s.kind {
	case "Unimplemented":
		s.fail("%s", s.unimpl)
	case "Base":
		// GenericStrategy raises a bare NotImplementedError: the module
		// crashes, and with no exception message ansible-core reports
		// only the summary.
		s.fail("Task failed: Module failed.")
	case "File", "SLES", "Alpine", "OpenBSD":
		if !isFile(s.file) {
			return ""
		}
		d, _ := fileContentStripped(s.file)
		return d
	case "RedHat":
		for _, line := range fileLines(s.file) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "HOSTNAME") {
				parts := strings.Split(line, "=")
				if len(parts) != 2 {
					if len(parts) < 2 {
						s.fail("failed to read hostname: not enough values to unpack (expected 2, got %d)", len(parts))
					}
					s.fail("failed to read hostname: too many values to unpack (expected 2)")
				}
				return strings.TrimSpace(parts[1])
			}
		}
		s.fail("Unable to locate HOSTNAME entry in %s", s.file)
	case "OpenRC", "FreeBSD":
		if !isFile(s.file) {
			return ""
		}
		for _, line := range fileLines(s.file) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "hostname=") {
				v := ""
				if len(line) > 10 {
					v = line[10:]
				}
				return strings.Trim(v, `"`)
			}
		}
		return ""
	case "Systemd":
		return strings.TrimSpace(s.mustRun(s.cmd, "--static", "status"))
	case "Solaris":
		rc, out, errOut := runCommand(s.env, []string{"/bin/sh", "-c",
			"/usr/sbin/svccfg -s svc:/system/identity:node listprop -o value config/nodename"}, cmdOpts{})
		if rc != 0 {
			s.fail("Command failed rc=%d, out=%s, err=%s", rc, out, errOut)
		}
		return strings.TrimSpace(out)
	case "Darwin":
		rc, out, errOut := s.run(s.cmd, "--get", "ComputerName")
		if rc != 0 {
			s.fail("Failed to get permanent hostname rc=%d, out=%s, err=%s", rc, out, errOut)
		}
		return strings.TrimSpace(out)
	}
	return ""
}

func (s *hostnameStrategy) setCurrent(name string) {
	switch s.kind {
	case "Systemd":
		if len([]rune(name)) > 64 {
			s.fail("name cannot be longer than 64 characters on systemd servers, try a shorter name")
		}
		s.mustRun(s.cmd, "--transient", "set-hostname", name)
	case "OpenBSD", "FreeBSD":
		s.mustRun(s.cmd, name)
	case "Solaris":
		s.mustRun(s.cmd, "-t", name)
	case "Alpine":
		s.mustRun(s.binPath("hostname"), "-F", s.file)
	}
}

func writeHostnameFile(s *hostnameStrategy, content string) {
	if err := os.WriteFile(s.file, []byte(content), 0o666); err != nil {
		s.fail("failed to update hostname: %s", pyStrOSError(err, s.file))
	}
}

func (s *hostnameStrategy) setPermanent(name string) {
	switch s.kind {
	case "File", "SLES", "Alpine", "OpenBSD":
		writeHostnameFile(s, name+"\n")
	case "RedHat":
		var content string
		if b, err := os.ReadFile(s.file); err == nil {
			content = string(b)
		}
		var lines []string
		found := false
		for _, line := range pySplitlinesKeep(content) {
			if strings.HasPrefix(strings.TrimSpace(line), "HOSTNAME") {
				lines = append(lines, "HOSTNAME="+name+"\n")
				found = true
			} else {
				lines = append(lines, line)
			}
		}
		if !found {
			lines = append(lines, "HOSTNAME="+name+"\n")
		}
		writeHostnameFile(s, strings.Join(lines, ""))
	case "OpenRC", "FreeBSD":
		var lines []string
		if s.kind == "FreeBSD" && !isFile(s.file) {
			lines = []string{`hostname="` + name + `"`}
		} else {
			for _, l := range fileLines(s.file) {
				lines = append(lines, strings.TrimSpace(l))
			}
			for i, l := range lines {
				if strings.HasPrefix(l, "hostname=") {
					lines[i] = `hostname="` + name + `"`
					break
				}
			}
		}
		writeHostnameFile(s, strings.Join(lines, "\n")+"\n")
	case "Systemd":
		if len([]rune(name)) > 64 {
			s.fail("name cannot be longer than 64 characters on systemd servers, try a shorter name")
		}
		s.mustRun(s.cmd, "--pretty", "--static", "set-hostname", name)
	case "Solaris":
		s.mustRun(s.cmd, name)
	case "Darwin":
		for _, typ := range []string{"HostName", "ComputerName", "LocalHostName"} {
			v := name
			if typ == "LocalHostName" {
				v = scrubDarwinHostname(name)
			}
			rc, out, errOut := s.run(s.cmd, "--set", typ, v)
			if rc != 0 {
				s.fail("Failed to set %s to '%s': %s %s", typ, name, out, errOut)
			}
		}
	}
}

// scrubDarwinHostname is DarwinStrategy._scrub_hostname.
func scrubDarwinHostname(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case strings.ContainsRune(".'", r):
		case strings.ContainsRune("\"~`!@#$%^&*(){}[]/=?+\\|-_ ", r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "")
	}
	return strings.TrimRight(out, "-")
}

func (s *hostnameStrategy) updateCurrent() {
	if s.kind == "Darwin" {
		return
	}
	if cur := s.getCurrent(); cur != s.name {
		if !s.env.CheckMode {
			s.setCurrent(s.name)
		}
		s.changed = true
	}
}

func (s *hostnameStrategy) updatePermanent() {
	if s.kind == "Darwin" {
		var all []string
		for _, typ := range []string{"HostName", "ComputerName", "LocalHostName"} {
			_, out, _ := s.run(s.cmd, "--get", typ)
			all = append(all, strings.TrimSpace(out))
		}
		want := []string{s.name, s.name, scrubDarwinHostname(s.name)}
		if strings.Join(all, "\x00") != strings.Join(want, "\x00") {
			if !s.env.CheckMode {
				s.setPermanent(s.name)
			}
			s.changed = true
		}
		return
	}
	if perm := s.getPermanent(); perm != s.name {
		if !s.env.CheckMode {
			s.setPermanent(s.name)
		}
		s.changed = true
	}
}

func (s *hostnameStrategy) updateBoth() bool {
	if s.kind == "Unimplemented" {
		s.fail("%s", s.unimpl)
	}
	if s.kind == "Systemd" {
		// The permanent name first, to avoid NetworkManager complaints.
		s.updatePermanent()
		s.updateCurrent()
	} else {
		s.updateCurrent()
		s.updatePermanent()
	}
	return s.changed
}

// pyDistribution is module_utils' get_distribution(): distro.id()
// capitalized, with Amazon/Redhat spellings ("" off Linux).
func pyDistribution() string {
	if platformSystem() != "Linux" {
		return ""
	}
	id := loadDistro(newFactEnv("", 10*time.Second)).id()
	switch {
	case id == "":
		return "OtherLinux"
	case id == "amzn":
		return "Amazon"
	case id == "rhel":
		return "Redhat"
	}
	r := []rune(strings.ToLower(id))
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

// hostnameSystemdManaged is ServiceMgrFactCollector.is_systemd_managed.
func hostnameSystemdManaged() bool {
	if _, err := getBinPath("systemctl"); err != nil {
		return false
	}
	for _, c := range []string{"/run/systemd/system/", "/dev/.run/systemd/", "/dev/.systemd/"} {
		if pathExists(c) {
			return true
		}
	}
	return false
}

func newHostnameStrategy(env *RunEnv, name, use string) *hostnameStrategy {
	s := &hostnameStrategy{env: env, name: name}
	system := platformSystem()
	switch {
	case use != "":
		s.kind = hostnameStrats[use]
	case system == "Linux" && hostnameSystemdManaged():
		s.kind = "Systemd"
	default:
		dist := pyDistribution()
		s.kind = "Unimplemented"
		switch system {
		case "Linux":
			if k, ok := hostnameLinuxDists[dist]; ok {
				s.kind = k
			} else if dist == "Sles" {
				v := loadDistro(newFactEnv("", 10*time.Second)).version(false)
				if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 10 && f <= 12 {
					s.kind = "SLES"
				}
			}
		case "Darwin":
			s.kind = "Darwin"
		case "FreeBSD", "NetBSD":
			s.kind = "FreeBSD"
		case "OpenBSD":
			s.kind = "OpenBSD"
		case "SunOS":
			s.kind = "Solaris"
		}
		if s.kind == "Unimplemented" {
			plat := system
			if dist != "" {
				plat += " (" + dist + ")"
			}
			s.unimpl = "hostname module cannot be used on platform " + plat
		}
	}
	switch s.kind {
	case "File", "Alpine":
		s.file = "/etc/hostname"
	case "SLES":
		s.file = "/etc/HOSTNAME"
	case "RedHat":
		s.file = "/etc/sysconfig/network"
	case "OpenRC":
		s.file = "/etc/conf.d/hostname"
	case "OpenBSD":
		s.file = "/etc/myname"
		s.cmd = s.binPath("hostname")
	case "FreeBSD":
		s.file = "/etc/rc.conf.d/hostname"
		s.cmd = s.binPath("hostname")
	case "Solaris":
		s.cmd = s.binPath("hostname")
	case "Systemd":
		s.cmd = s.binPath("hostnamectl")
	case "Darwin":
		s.cmd = s.binPath("scutil")
	}
	return s
}

func hostnameModule(env *RunEnv, raw map[string]any) (res *agentproto.Result) {
	p, err := hostnameSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			a, ok := r.(hostnameAbort)
			if !ok {
				panic(r)
			}
			res = a.res
		}
	}()
	name := p.Str("name")
	s := newHostnameStrategy(env, name, p.Str("use"))
	current := s.getCurrent()
	permanent := s.getPermanent()
	changed := s.updateBoth()
	before := permanent
	if name != current {
		before = current
	}
	host, _ := os.Hostname()
	fqdn := getFQDN(host)
	domain := ""
	if i := strings.IndexByte(fqdn, '.'); i >= 0 {
		domain = fqdn[i+1:]
	}
	res = &agentproto.Result{Changed: changed, Extra: map[string]any{"name": name},
		AnsibleFacts: map[string]any{
			"ansible_hostname": strings.SplitN(name, ".", 2)[0],
			"ansible_nodename": name,
			"ansible_fqdn":     fqdn,
			"ansible_domain":   domain,
		}}
	if changed {
		res.Diff = map[string]any{"after": "hostname = " + name + "\n", "before": "hostname = " + before + "\n"}
	}
	return res
}
