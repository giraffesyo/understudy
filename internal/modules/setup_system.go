package modules

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// System fact collectors (ansible/module_utils/facts/system/*).

func collectPlatform(e *factEnv, _ map[string]any) map[string]any {
	u := e.uname()
	f := map[string]any{
		"system":         u.sysname,
		"kernel":         u.release,
		"kernel_version": u.version,
		"machine":        u.machine,
		"python_version": pythonVersion(e),
		"nodename":       u.nodename,
	}
	fqdn := e.fqdn(u.nodename)
	f["fqdn"] = fqdn
	short, _, _ := strings.Cut(u.nodename, ".")
	f["hostname"] = short
	if _, dom, ok := strings.Cut(fqdn, "."); ok {
		f["domain"] = dom
	} else {
		f["domain"] = ""
	}
	bits := strconv.Itoa(strconv.IntSize)
	f["userspace_bits"] = bits
	machine := u.machine
	switch {
	case machine == "x86_64":
		f["architecture"] = machine
		if bits == "64" {
			f["userspace_architecture"] = "x86_64"
		} else if bits == "32" {
			f["userspace_architecture"] = "i386"
		}
	case regexp.MustCompile(`i([3456]86|86pc)`).MatchString(machine):
		f["architecture"] = "i386"
		if bits == "64" {
			f["userspace_architecture"] = "x86_64"
		} else if bits == "32" {
			f["userspace_architecture"] = "i386"
		}
	default:
		f["architecture"] = machine
	}
	mid, ok := e.fileContent("/var/lib/dbus/machine-id")
	if !ok {
		mid, ok = e.fileContent("/etc/machine-id")
	}
	if ok {
		f["machine_id"] = strings.SplitN(mid, "\n", 2)[0]
	}
	return f
}

// getFQDN mirrors Python's socket.getfqdn(): resolve the name, reverse
// resolve an address, and take the first name containing a dot.
func getFQDN(node string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, node)
	if err != nil || len(addrs) == 0 {
		return node
	}
	names, err := net.DefaultResolver.LookupAddr(ctx, addrs[0])
	if err != nil || len(names) == 0 {
		// musl's gethostbyaddr falls back to the numeric address where
		// glibc's fails, so there the address itself is the answer.
		if m, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(m) > 0 {
			return addrs[0]
		}
		return node
	}
	for _, n := range names {
		n = strings.TrimSuffix(n, ".")
		if strings.Contains(n, ".") {
			return n
		}
	}
	return strings.TrimSuffix(names[0], ".")
}

// --- python ------------------------------------------------------------

type pyInfo struct {
	exe                         string
	major, minor, micro, serial int
	level                       string
	full                        string
}

// pythonInfo probes python3 on the host: understudy runs without Python,
// so we report the interpreter Ansible would have used, if any.
func pythonInfo(e *factEnv) *pyInfo {
	if !e.pyDone {
		e.py, e.pyDone = probePython(e), true
	}
	return e.py
}

func probePython(e *factEnv) *pyInfo {
	exe := e.binPath("python3")
	if exe == "" {
		return nil
	}
	rc, out, errOut := e.run(exe, "--version")
	if rc != 0 {
		return nil
	}
	text := strings.TrimSpace(out)
	if text == "" {
		text = strings.TrimSpace(errOut) // python2 prints to stderr
	}
	m := regexp.MustCompile(`^Python (\d+)\.(\d+)\.(\d+)(?:(a|b|rc)(\d+))?`).FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	pi := &pyInfo{exe: exe, level: "final"}
	pi.major, _ = strconv.Atoi(m[1])
	pi.minor, _ = strconv.Atoi(m[2])
	pi.micro, _ = strconv.Atoi(m[3])
	switch m[4] {
	case "a":
		pi.level = "alpha"
	case "b":
		pi.level = "beta"
	case "rc":
		pi.level = "candidate"
	}
	if m[5] != "" {
		pi.serial, _ = strconv.Atoi(m[5])
	}
	pi.full = m[1] + "." + m[2] + "." + m[3] + m[4] + m[5]
	return pi
}

func pythonVersion(e *factEnv) string {
	if pi := pythonInfo(e); pi != nil {
		return pi.full
	}
	return ""
}

func collectPython(e *factEnv, _ map[string]any) map[string]any {
	pi := pythonInfo(e)
	if pi == nil {
		return map[string]any{}
	}
	return map[string]any{"python": map[string]any{
		"version": map[string]any{
			"major": pi.major, "minor": pi.minor, "micro": pi.micro,
			"releaselevel": pi.level, "serial": pi.serial,
		},
		"version_info":   []any{pi.major, pi.minor, pi.micro, pi.level, pi.serial},
		"executable":     pi.exe,
		"has_sslcontext": true,
		"type":           "cpython",
	}}
}

// --- lsb ---------------------------------------------------------------

func collectLSB(e *factEnv, _ map[string]any) map[string]any {
	lsb := map[string]any{}
	if bin := e.binPath("lsb_release"); bin != "" {
		if rc, out, _ := e.run(bin, "-a"); rc == 0 {
			for _, line := range strings.Split(out, "\n") {
				if line == "" || !strings.Contains(line, ":") {
					continue
				}
				_, value, _ := strings.Cut(line, ":")
				value = strings.TrimSpace(value)
				switch {
				case strings.Contains(line, "LSB Version:"):
					lsb["release"] = value
				case strings.Contains(line, "Distributor ID:"):
					lsb["id"] = value
				case strings.Contains(line, "Description:"):
					lsb["description"] = value
				case strings.Contains(line, "Release:"):
					lsb["release"] = value
				case strings.Contains(line, "Codename:"):
					lsb["codename"] = value
				}
			}
		}
	}
	if len(lsb) == 0 && e.exists("/etc/lsb-release") {
		for _, line := range e.fileLines("/etc/lsb-release") {
			_, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch {
			case strings.Contains(line, "DISTRIB_ID"):
				lsb["id"] = value
			case strings.Contains(line, "DISTRIB_RELEASE"):
				lsb["release"] = value
			case strings.Contains(line, "DISTRIB_DESCRIPTION"):
				lsb["description"] = value
			case strings.Contains(line, "DISTRIB_CODENAME"):
				lsb["codename"] = value
			}
		}
	}
	if rel, ok := lsb["release"].(string); ok {
		lsb["major_release"] = strings.SplitN(rel, ".", 2)[0]
	}
	for k, v := range lsb {
		if s, _ := v.(string); s != "" {
			lsb[k] = strings.Trim(s, `'"\`)
		}
	}
	return map[string]any{"lsb": lsb}
}

// --- selinux / apparmor / fips / chroot / caps --------------------------

func collectSelinux(e *factEnv, _ map[string]any) map[string]any {
	se := map[string]any{}
	f := map[string]any{"selinux_python_present": true, "selinux": se}
	// ansible's selinux bindings are a ctypes wrapper over libselinux.so.1;
	// without the library it can't tell whether SELinux is enabled.
	if !hasLibselinux(e) {
		se["status"] = "Missing selinux Python library"
		f["selinux_python_present"] = false
		return f
	}
	mnt := selinuxMount(e)
	if mnt == "" {
		se["status"] = "disabled"
		return f
	}
	se["status"] = "enabled"
	if v, ok := e.fileContent(mnt + "/policyvers"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			se["policyvers"] = n
		} else {
			se["policyvers"] = "unknown"
		}
	} else {
		se["policyvers"] = "unknown"
	}
	modes := map[string]string{"1": "enforcing", "0": "permissive", "-1": "disabled"}
	cfgMode, cfgType := "", ""
	for _, line := range e.fileLines("/etc/selinux/config") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "SELINUX="); ok {
			cfgMode = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "SELINUXTYPE="); ok {
			cfgType = strings.TrimSpace(v)
		}
	}
	switch strings.ToLower(cfgMode) {
	case "enforcing", "permissive", "disabled":
		se["config_mode"] = strings.ToLower(cfgMode)
	default:
		se["config_mode"] = "unknown"
	}
	if v, ok := e.fileContent(mnt + "/enforce"); ok {
		if m, ok := modes[v]; ok {
			se["mode"] = m
		} else {
			se["mode"] = "unknown"
		}
	} else {
		se["mode"] = "unknown"
	}
	if cfgType != "" {
		se["type"] = cfgType
	} else {
		se["type"] = "unknown"
	}
	return f
}

func hasLibselinux(e *factEnv) bool {
	if data, err := os.ReadFile(e.p("/etc/ld.so.cache")); err == nil && bytes.Contains(data, []byte("libselinux.so.1")) {
		return true
	}
	dirs := []string{"/lib64", "/usr/lib64", "/lib", "/usr/lib", "/usr/local/lib"}
	for _, base := range []string{"/lib", "/usr/lib"} {
		dirs = append(dirs, e.glob(base, "*-linux-gnu*")...)
	}
	for _, d := range dirs {
		if e.exists(d + "/libselinux.so.1") {
			return true
		}
	}
	return false
}

// selinuxMount finds selinuxfs like libselinux: /sys/fs/selinux, else a
// selinuxfs entry in /proc/mounts.
func selinuxMount(e *factEnv) string {
	if e.exists("/sys/fs/selinux/enforce") {
		return "/sys/fs/selinux"
	}
	for _, line := range e.fileLines("/proc/mounts") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "selinuxfs" && e.exists(fields[1]+"/enforce") {
			return fields[1]
		}
	}
	return ""
}

func collectApparmor(e *factEnv, _ map[string]any) map[string]any {
	status := "disabled"
	if e.exists("/sys/kernel/security/apparmor") {
		status = "enabled"
	}
	return map[string]any{"apparmor": map[string]any{"status": status}}
}

func collectFips(e *factEnv, _ map[string]any) map[string]any {
	v, _ := e.fileContent("/proc/sys/crypto/fips_enabled")
	return map[string]any{"fips": v == "1"}
}

func collectChroot(e *factEnv, _ map[string]any) map[string]any {
	if e.getenv("debian_chroot") != "" {
		return map[string]any{"is_chroot": true}
	}
	rootDev, rootIno, ok := e.statID("/")
	if !ok {
		return map[string]any{"is_chroot": nil}
	}
	if dev, ino, ok := e.statID("/proc/1/root/."); ok {
		return map[string]any{"is_chroot": rootIno != ino || rootDev != dev}
	}
	fsRootIno := uint64(2)
	switch e.fsType("/") {
	case 0x9123683E: // btrfs
		fsRootIno = 256
	case 0x58465342: // xfs
		fsRootIno = 128
	}
	return map[string]any{"is_chroot": rootIno != fsRootIno}
}

func collectCaps(e *factEnv, _ map[string]any) map[string]any {
	na := map[string]any{"system_capabilities_enforced": "N/A", "system_capabilities": "N/A"}
	capsh := e.binPath("capsh")
	if capsh == "" {
		return na
	}
	rc, out, _ := e.run(capsh, "--print")
	if rc != 0 {
		return na
	}
	enforced := "NA"
	caps := []any{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Current:") {
			if strings.TrimSpace(strings.SplitN(line, ":", 3)[1]) == "=ep" {
				enforced = "False"
			} else {
				enforced = "True"
				caps = []any{}
				parts := strings.Split(line, "=")
				if len(parts) > 1 {
					for _, c := range strings.Split(parts[1], ",") {
						caps = append(caps, strings.TrimSpace(c))
					}
				}
			}
		}
	}
	return map[string]any{"system_capabilities_enforced": enforced, "system_capabilities": caps}
}

// --- pkg_mgr / service_mgr ----------------------------------------------

var pkgMgrs = []struct{ path, name string }{
	{"/usr/bin/rpm-ostree", "atomic_container"},
	{"/usr/bin/yum", "dnf"},
	{"/usr/bin/dnf-3", "dnf"},
	{"/usr/bin/dnf5", "dnf5"},
	{"/usr/bin/apt-get", "apt"},
	{"/usr/bin/zypper", "zypper"},
	{"/usr/sbin/urpmi", "urpmi"},
	{"/usr/bin/pacman", "pacman"},
	{"/bin/opkg", "opkg"},
	{"/usr/pkg/bin/pkgin", "pkgin"},
	{"/opt/local/bin/pkgin", "pkgin"},
	{"/opt/tools/bin/pkgin", "pkgin"},
	{"/opt/local/bin/port", "macports"},
	{"/usr/local/bin/brew", "homebrew"},
	{"/opt/homebrew/bin/brew", "homebrew"},
	{"/sbin/apk", "apk"},
	{"/usr/sbin/pkg", "pkgng"},
	{"/usr/sbin/swlist", "swdepot"},
	{"/usr/bin/emerge", "portage"},
	{"/usr/sbin/pkgadd", "svr4pkg"},
	{"/usr/bin/pkg", "pkg5"},
	{"/usr/bin/xbps-install", "xbps"},
	{"/usr/local/sbin/pkg", "pkgng"},
	{"/usr/bin/swupd", "swupd"},
	{"/usr/sbin/sorcery", "sorcery"},
	{"/usr/bin/installp", "installp"},
	{"/QOpenSys/pkgs/bin/yum", "yum"},
}

func collectPkgMgr(e *factEnv, prior map[string]any) map[string]any {
	name := "unknown"
	family, _ := prior["os_family"].(string)
	for _, pm := range pkgMgrs {
		// Altlinux's /usr/bin/pkg is perl-Package, not Solaris pkg5.
		if family == "Altlinux" && pm.path == "/usr/bin/pkg" {
			continue
		}
		if e.exists(pm.path) {
			name = pm.name
		}
	}
	switch family {
	case "RedHat":
		// _check_rh_versions: dnf or microdnf, dnf5 when either is it.
		name = "unknown"
		if e.exists("/run/ostree-booted") {
			name = "atomic_container"
			break
		}
		for _, bin := range []string{"/usr/bin/dnf", "/usr/bin/microdnf"} {
			if e.exists(bin) {
				name = "dnf"
				if e.realpath(bin) == "/usr/bin/dnf5" {
					name = "dnf5"
				}
				break
			}
		}
	case "Debian":
		name = "apt"
	case "Altlinux":
		if name == "apt" {
			name = "apt_rpm"
		}
	}
	// _check_apt_flavor: an apt-get that rpm owns is APT-RPM.
	if name == "apt" && e.exists("/usr/bin/rpm") {
		if rc, _, _ := e.run("/usr/bin/rpm", "-q", "--whatprovides", "/usr/bin/apt-get"); rc == 0 {
			name = "apt_rpm"
		}
	}
	return map[string]any{"pkg_mgr": name}
}

func collectServiceMgr(e *factEnv, prior map[string]any) map[string]any {
	var proc1 string
	if v, ok := e.fileContent("/proc/1/comm"); ok {
		proc1 = v
	} else if ps := e.binPath("ps"); ps != "" {
		if rc, out, _ := e.run(ps, "-p", "1", "-o", "comm"); rc == 0 {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			proc1 = strings.TrimSpace(lines[len(lines)-1])
		}
	}
	if proc1 != "" {
		if i := strings.LastIndex(proc1, "/"); i >= 0 {
			proc1 = proc1[i+1:]
		}
		proc1 = strings.TrimSpace(proc1)
	}
	if proc1 == "COMMAND" || proc1 == "init" || strings.HasSuffix(proc1, "sh") {
		proc1 = ""
	}
	name := ""
	system, _ := prior["system"].(string)
	dist, _ := prior["distribution"].(string)
	proc1Map := map[string]string{"procd": "openwrt_init", "runit-init": "runit",
		"svscan": "svc", "openrc-init": "openrc"}
	switch {
	case proc1 != "":
		if m, ok := proc1Map[proc1]; ok {
			name = m
		} else {
			name = proc1
		}
	case dist == "MacOSX" || system == "Darwin":
		name = "launchd"
	case strings.HasSuffix(system, "BSD") || system == "DragonFly":
		name = "bsdinit"
	case system == "AIX":
		name = "src"
	case system == "SunOS":
		name = "smf"
	case system == "Linux":
		switch {
		case isSystemdManaged(e):
			name = "systemd"
		case e.binPath("initctl") != "" && e.exists("/etc/init/"):
			name = "upstart"
		case e.exists("/sbin/openrc"):
			name = "openrc"
		case isSystemdManagedOffline(e):
			name = "systemd"
		case e.exists("/etc/init.d/"):
			name = "sysvinit"
		case e.exists("/etc/dinit.d/"):
			name = "dinit"
		}
	}
	if name == "" {
		name = "service"
	}
	return map[string]any{"service_mgr": name}
}

func isSystemdManaged(e *factEnv) bool {
	if e.binPath("systemctl") == "" {
		return false
	}
	for _, canary := range []string{"/run/systemd/system/", "/dev/.run/systemd/", "/dev/.systemd/"} {
		if e.exists(canary) {
			return true
		}
	}
	return false
}

func isSystemdManagedOffline(e *factEnv) bool {
	if e.binPath("systemctl") == "" || !e.isLink("/sbin/init") {
		return false
	}
	target := e.realpath("/sbin/init")
	return strings.HasSuffix(target, "/systemd") || target == "systemd"
}

// --- cmdline / date_time / env / loadavg --------------------------------

func collectCmdline(e *factEnv, _ map[string]any) map[string]any {
	data, ok := e.fileContent("/proc/cmdline")
	if !ok {
		return map[string]any{}
	}
	cmdline := map[string]any{}
	proc := map[string]any{}
	for _, piece := range shlexSplitNonPosix(data) {
		k, v, hasEq := strings.Cut(piece, "=")
		var val any = true
		if hasEq {
			val = v
		}
		cmdline[k] = val
		if old, exists := proc[k]; exists {
			if l, isList := old.([]any); isList {
				proc[k] = append(l, val)
			} else {
				proc[k] = []any{old, val}
			}
		} else {
			proc[k] = val
		}
	}
	return map[string]any{"cmdline": cmdline, "proc_cmdline": proc}
}

// shlexSplitNonPosix approximates shlex.split(s, posix=False): whitespace
// separated tokens, quoted runs kept intact including their quotes.
func shlexSplitNonPosix(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	inTok := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
			inTok = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inTok {
				out = append(out, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if inTok {
		out = append(out, cur.String())
	}
	return out
}

func collectDateTime(e *factEnv, _ map[string]any) map[string]any {
	now := e.now()
	utc := now.UTC()
	zone, _ := now.Zone()
	epoch := strconv.FormatInt(now.Unix(), 10)
	yday := now.YearDay() - 1
	wd := int(now.Weekday())
	weeknum := (yday + 7 - (wd+6)%7) / 7
	// time.tzname[1]: the DST name (or the standard name when no DST).
	jan := time.Date(now.Year(), 1, 1, 12, 0, 0, 0, now.Location())
	jul := time.Date(now.Year(), 7, 1, 12, 0, 0, 0, now.Location())
	janName, janOff := jan.Zone()
	julName, julOff := jul.Zone()
	dst := julName
	if janOff > julOff {
		dst = janName
	}
	micro := now.Nanosecond() / 1000
	return map[string]any{"date_time": map[string]any{
		"year":                now.Format("2006"),
		"month":               now.Format("01"),
		"weekday":             now.Weekday().String(),
		"weekday_number":      strconv.Itoa(wd),
		"weeknumber":          twoDigit(weeknum),
		"day":                 now.Format("02"),
		"hour":                now.Format("15"),
		"minute":              now.Format("04"),
		"second":              now.Format("05"),
		"epoch":               epoch,
		"epoch_int":           epoch,
		"date":                now.Format("2006-01-02"),
		"time":                now.Format("15:04:05"),
		"iso8601_micro":       utc.Format("2006-01-02T15:04:05.000000Z"),
		"iso8601":             utc.Format("2006-01-02T15:04:05Z"),
		"iso8601_basic":       now.Format("20060102T150405") + sixDigit(micro),
		"iso8601_basic_short": now.Format("20060102T150405"),
		"tz":                  zone,
		"tz_dst":              dst,
		"tz_offset":           now.Format("-0700"),
	}}
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func sixDigit(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 6-len(s)) + s
}

func collectEnv(e *factEnv, _ map[string]any) map[string]any {
	env := map[string]any{}
	for _, kv := range e.environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return map[string]any{"env": env}
}

func collectLoadavg(e *factEnv, _ map[string]any) map[string]any {
	data, ok := e.fileContent("/proc/loadavg")
	if !ok {
		if la, ok := sysLoadavg(); ok {
			return map[string]any{"loadavg": map[string]any{"1m": la[0], "5m": la[1], "15m": la[2]}}
		}
		return map[string]any{}
	}
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return map[string]any{}
	}
	var la [3]float64
	for i := 0; i < 3; i++ {
		la[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return map[string]any{"loadavg": map[string]any{"1m": la[0], "5m": la[1], "15m": la[2]}}
}

// --- ssh keys / user ----------------------------------------------------

func collectSSHPubKeys(e *factEnv, _ map[string]any) map[string]any {
	f := map[string]any{}
	for _, dir := range []string{"/etc/ssh", "/etc/openssh", "/etc"} {
		for _, algo := range []string{"dsa", "rsa", "ecdsa", "ed25519"} {
			name := "ssh_host_key_" + algo + "_public"
			if _, done := f[name]; done {
				return f
			}
			data, ok := e.fileContent(dir + "/ssh_host_" + algo + "_key.pub")
			if !ok {
				continue
			}
			fields := strings.Fields(data)
			if len(fields) < 2 {
				continue
			}
			f[name] = fields[1]
			f[name+"_keytype"] = fields[0]
		}
	}
	return f
}

type pwEnt struct {
	name           string
	uid, gid       int
	gecos, dir, sh string
}

func (e *factEnv) passwd() []pwEnt {
	var out []pwEnt
	for _, line := range e.fileLines("/etc/passwd") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, pwEnt{f[0], uid, gid, f[4], f[5], f[6]})
	}
	return out
}

func collectUser(e *factEnv, _ map[string]any) map[string]any {
	uid, euid, gid, _ := e.ids()
	pw := e.passwd()
	byUID := func(id int) *pwEnt {
		for i := range pw {
			if pw[i].uid == id {
				return &pw[i]
			}
		}
		return nil
	}
	// getpass.getuser(): LOGNAME, USER, LNAME, USERNAME, then the passwd name.
	name := ""
	for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := e.getenv(k); v != "" {
			name = v
			break
		}
	}
	if name == "" {
		if ent := byUID(uid); ent != nil {
			name = ent.name
		}
	}
	var ent *pwEnt
	for i := range pw {
		if pw[i].name == name {
			ent = &pw[i]
			break
		}
	}
	if ent == nil {
		ent = byUID(uid)
	}
	f := map[string]any{
		"user_id":            name,
		"real_user_id":       uid,
		"effective_user_id":  euid,
		"real_group_id":      gid,
		"effective_group_id": gid, // ansible reports os.getgid() here
	}
	if ent != nil {
		f["user_uid"] = ent.uid
		f["user_gid"] = ent.gid
		f["user_gecos"] = ent.gecos
		f["user_dir"] = ent.dir
		f["user_shell"] = ent.sh
	} else if e.system != "Linux" {
		// Non-Linux control nodes (macOS) keep accounts outside
		// /etc/passwd; fall back to the process identity.
		f["user_uid"] = uid
		f["user_gid"] = gid
		if home := e.getenv("HOME"); home != "" {
			f["user_dir"] = home
		}
		if sh := e.getenv("SHELL"); sh != "" {
			f["user_shell"] = sh
		}
	}
	return f
}

// --- dns / initiators ---------------------------------------------------

func collectDNS(e *factEnv, _ map[string]any) map[string]any {
	dns := map[string]any{}
	data, _ := e.fileContent("/etc/resolv.conf")
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.TrimSpace(line) == "" {
			continue
		}
		tokens := strings.Fields(line)
		if len(tokens) == 0 {
			continue
		}
		switch tokens[0] {
		case "nameserver":
			ns, _ := dns["nameservers"].([]any)
			if ns == nil {
				ns = []any{}
			}
			for _, t := range tokens[1:] {
				ns = append(ns, t)
			}
			dns["nameservers"] = ns
		case "domain":
			if len(tokens) > 1 {
				dns["domain"] = tokens[1]
			}
		case "search":
			l := []any{}
			for _, t := range tokens[1:] {
				l = append(l, t)
			}
			dns["search"] = l
		case "sortlist":
			l := []any{}
			for _, t := range tokens[1:] {
				l = append(l, t)
			}
			dns["sortlist"] = l
		case "options":
			opts := map[string]any{}
			for _, o := range tokens[1:] {
				k, v, hasVal := strings.Cut(o, ":")
				if hasVal && v != "" {
					opts[k] = v
				} else {
					opts[k] = true
				}
			}
			dns["options"] = opts
		}
	}
	return map[string]any{"dns": dns}
}

func collectFcWwn(e *factEnv, _ map[string]any) map[string]any {
	wwn := []any{}
	if e.system == "Linux" {
		for _, f := range e.glob("/sys/class/fc_host", "*") {
			for _, line := range e.fileLines(f + "/port_name") {
				line = strings.TrimRight(line, " \t\r\n")
				if len(line) >= 2 {
					wwn = append(wwn, line[2:])
				} else {
					wwn = append(wwn, "")
				}
			}
		}
	}
	return map[string]any{"fibre_channel_wwn": wwn}
}

func collectIscsi(e *factEnv, _ map[string]any) map[string]any {
	iqn := ""
	if e.system == "Linux" || e.system == "SunOS" {
		data, _ := e.fileContent("/etc/iscsi/initiatorname.iscsi")
		for _, line := range strings.Split(data, "\n") {
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.TrimSpace(line) == "" {
				continue
			}
			if v, ok := strings.CutPrefix(line, "InitiatorName="); ok {
				iqn = v
				break
			}
		}
	}
	return map[string]any{"iscsi_iqn": iqn}
}

func collectNvme(e *factEnv, _ map[string]any) map[string]any {
	nqn := ""
	if e.system == "Linux" {
		data, _ := e.fileContent("/etc/nvme/hostnqn")
		for _, line := range strings.Split(data, "\n") {
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.TrimSpace(line) == "" {
				continue
			}
			if strings.HasPrefix(line, "nqn.") {
				nqn = line
				break
			}
		}
	}
	return map[string]any{"hostnqn": nqn}
}

// --- facter / ohai --------------------------------------------------------

func collectFacter(e *factEnv, _ map[string]any) map[string]any {
	bin := e.binPath("cfacter")
	if bin == "" {
		bin = e.binPath("facter")
	}
	if bin == "" && e.exists("/opt/puppetlabs/bin/facter") {
		bin = "/opt/puppetlabs/bin/facter"
	}
	if bin == "" {
		return map[string]any{}
	}
	rc, out, _ := e.run(bin, "--puppet", "--json")
	if rc != 0 {
		rc, out, _ = e.run(bin, "--json")
	}
	if rc != 0 {
		return map[string]any{}
	}
	if m, ok := decodeJSONValue([]byte(out)).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func collectOhai(e *factEnv, _ map[string]any) map[string]any {
	bin := e.binPath("ohai")
	if bin == "" {
		return map[string]any{}
	}
	rc, out, _ := e.run(bin)
	if rc != 0 {
		return map[string]any{}
	}
	if m, ok := decodeJSONValue([]byte(out)).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
