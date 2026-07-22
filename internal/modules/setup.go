package modules

import (
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// titleCase capitalizes the first letter (strings.Title is deprecated).
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type ifaceInfo struct {
	name  string
	addrs []string // ipv4 only
}

func netInterfaces() ([]ifaceInfo, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifaceInfo
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		info := ifaceInfo{name: ifc.Name}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					info.addrs = append(info.addrs, v4.String())
				}
			}
		}
		if len(info.addrs) > 0 {
			out = append(out, info)
		}
	}
	return out, nil
}

func init() {
	Register(setupModule, "setup", "gather_facts", "ansible.builtin.setup")
}

// setupModule gathers ansible_facts. Pure Go: /etc/os-release, /proc, and
// stdlib probes — no Python, no shell-outs except PATH lookups. Fields
// unavailable on the current OS are simply omitted.
func setupModule(env *RunEnv, args map[string]any) *agentproto.Result {
	facts := map[string]any{}

	facts["ansible_system"] = titleCase(runtime.GOOS)
	facts["ansible_architecture"] = normalizeArch(runtime.GOARCH)
	facts["ansible_processor_vcpus"] = runtime.NumCPU()
	facts["ansible_processor_count"] = runtime.NumCPU()

	if hostname, err := os.Hostname(); err == nil {
		facts["ansible_fqdn"] = hostname
		short, _, _ := strings.Cut(hostname, ".")
		facts["ansible_hostname"] = short
		facts["ansible_nodename"] = hostname
	}

	osRelease(facts)
	memInfo(facts)
	networkFacts(facts)
	userFacts(facts)
	dateTimeFacts(facts)
	serviceMgrFact(facts)
	pkgMgrFact(facts)

	// Python stubs: defined-but-empty so playbook conditionals referencing
	// them get falsy values instead of undefined-variable errors.
	facts["ansible_python_version"] = ""

	envMap := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}
	facts["ansible_env"] = envMap

	return &agentproto.Result{AnsibleFacts: facts}
}

func normalizeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return goarch
}

// osRelease parses /etc/os-release into distribution facts, with the
// ID_LIKE mapping table for os_family.
func osRelease(facts map[string]any) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		if runtime.GOOS == "darwin" {
			facts["ansible_os_family"] = "Darwin"
			facts["ansible_distribution"] = "MacOSX"
			facts["ansible_system"] = "Darwin"
		}
		return
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		kv[k] = strings.Trim(v, `"`)
	}
	dist := kv["ID"]
	facts["ansible_distribution"] = distTitle(dist)
	if v := kv["VERSION_ID"]; v != "" {
		facts["ansible_distribution_version"] = v
		if major, _, ok := strings.Cut(v, "."); ok {
			facts["ansible_distribution_major_version"] = major
		} else {
			facts["ansible_distribution_major_version"] = v
		}
	}
	if v := kv["VERSION_CODENAME"]; v != "" {
		facts["ansible_distribution_release"] = v
	}
	facts["ansible_os_family"] = osFamily(dist, kv["ID_LIKE"])
}

func distTitle(id string) string {
	switch id {
	case "ubuntu":
		return "Ubuntu"
	case "debian":
		return "Debian"
	case "centos":
		return "CentOS"
	case "rhel":
		return "RedHat"
	case "fedora":
		return "Fedora"
	case "rocky":
		return "Rocky"
	case "almalinux":
		return "AlmaLinux"
	case "alpine":
		return "Alpine"
	case "opensuse", "opensuse-leap":
		return "openSUSE Leap"
	case "arch":
		return "Archlinux"
	case "amzn":
		return "Amazon"
	}
	return titleCase(id)
}

func osFamily(id, idLike string) string {
	families := map[string]string{
		"debian": "Debian", "ubuntu": "Debian", "linuxmint": "Debian",
		"rhel": "RedHat", "centos": "RedHat", "fedora": "RedHat",
		"rocky": "RedHat", "almalinux": "RedHat", "amzn": "RedHat",
		"oracle": "RedHat",
		"alpine": "Alpine",
		"suse":   "Suse", "opensuse": "Suse", "opensuse-leap": "Suse", "sles": "Suse",
		"arch": "Archlinux",
	}
	if fam, ok := families[id]; ok {
		return fam
	}
	for _, like := range strings.Fields(idLike) {
		if fam, ok := families[like]; ok {
			return fam
		}
	}
	return titleCase(id)
}

func memInfo(facts map[string]any) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(v)
			if len(fields) >= 1 {
				if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					facts["ansible_memtotal_mb"] = kb / 1024
				}
			}
			return
		}
	}
}

// networkFacts collects ipv4 addresses and the default interface (from
// /proc/net/route on linux; first non-loopback interface elsewhere).
func networkFacts(facts map[string]any) {
	ifaces, err := netInterfaces()
	if err != nil {
		return
	}
	var all []any
	var defaultV4 map[string]any
	defaultIface := linuxDefaultRouteIface()
	for _, ifc := range ifaces {
		for _, addr := range ifc.addrs {
			all = append(all, addr)
			if defaultV4 == nil && (ifc.name == defaultIface ||
				(defaultIface == "" && !strings.HasPrefix(addr, "127."))) {
				defaultV4 = map[string]any{
					"address":   addr,
					"interface": ifc.name,
				}
			}
		}
	}
	facts["ansible_all_ipv4_addresses"] = all
	if defaultV4 != nil {
		facts["ansible_default_ipv4"] = defaultV4
	} else {
		facts["ansible_default_ipv4"] = map[string]any{}
	}
}

// linuxDefaultRouteIface finds the interface of the 0.0.0.0/0 route.
func linuxDefaultRouteIface() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

func userFacts(facts map[string]any) {
	if u, err := user.Current(); err == nil {
		facts["ansible_user_id"] = u.Username
		facts["ansible_user_dir"] = u.HomeDir
		facts["ansible_user_uid"] = u.Uid
		facts["ansible_user_gid"] = u.Gid
	}
	facts["ansible_effective_user_id"] = os.Geteuid()
}

func dateTimeFacts(facts map[string]any) {
	now := time.Now()
	zone, _ := now.Zone()
	facts["ansible_date_time"] = map[string]any{
		"date":    now.Format("2006-01-02"),
		"time":    now.Format("15:04:05"),
		"iso8601": now.UTC().Format("2006-01-02T15:04:05Z"),
		"epoch":   strconv.FormatInt(now.Unix(), 10),
		"year":    now.Format("2006"),
		"month":   now.Format("01"),
		"day":     now.Format("02"),
		"hour":    now.Format("15"),
		"minute":  now.Format("04"),
		"second":  now.Format("05"),
		"weekday": now.Weekday().String(),
		"tz":      zone,
	}
}

func serviceMgrFact(facts map[string]any) {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		facts["ansible_service_mgr"] = "systemd"
		return
	}
	if runtime.GOOS == "darwin" {
		facts["ansible_service_mgr"] = "launchd"
		return
	}
	facts["ansible_service_mgr"] = "unknown"
}

func pkgMgrFact(facts map[string]any) {
	for _, mgr := range []string{"apt-get", "dnf", "yum", "apk", "brew", "pacman", "zypper"} {
		if _, err := exec.LookPath(mgr); err == nil {
			name := mgr
			if mgr == "apt-get" {
				name = "apt"
			}
			if mgr == "brew" {
				name = "homebrew"
			}
			facts["ansible_pkg_mgr"] = name
			return
		}
	}
	facts["ansible_pkg_mgr"] = "unknown"
}
