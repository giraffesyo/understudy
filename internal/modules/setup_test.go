package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixture builds a fake target filesystem under a temp dir.
type fixture struct {
	t    *testing.T
	root string
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, root: t.TempDir()}
}

func (f *fixture) file(p, content string) *fixture {
	f.t.Helper()
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fixture) exec(p, content string) *fixture {
	f.file(p, content)
	if err := os.Chmod(filepath.Join(f.root, p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fixture) dir(p string) *fixture {
	if err := os.MkdirAll(filepath.Join(f.root, p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fixture) link(p, target string) *fixture {
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		f.t.Fatal(err)
	}
	return f
}

// env returns a Linux factEnv over the fixture with canned commands:
// cmds maps "argv joined by spaces" to stdout (rc 0); bins lists commands
// present on the fake PATH.
func (f *fixture) env(cmds map[string]string, bins ...string) *factEnv {
	e := newFactEnv(filepath.Join(f.root, "/etc/ansible/facts.d"), 5*time.Second)
	e.factPath = "/etc/ansible/facts.d"
	e.root = f.root
	e.system = "Linux"
	e.uname = func() unameInfo {
		return unameInfo{"Linux", "web01", "5.14.0-570.el9.x86_64",
			"#1 SMP PREEMPT_DYNAMIC Tue Jan 1 00:00:00 UTC 2025", "x86_64"}
	}
	e.fqdn = func(n string) string { return n + ".example.com" }
	have := map[string]bool{}
	for _, b := range bins {
		have[b] = true
	}
	e.lookPath = func(name string) string {
		if have[name] {
			return "/usr/bin/" + name
		}
		return ""
	}
	e.run = func(argv ...string) (int, string, string) {
		key := strings.Join(argv, " ")
		if out, ok := cmds[key]; ok {
			return 0, out, ""
		}
		if strings.HasPrefix(argv[0], f.root) {
			return e.execCommand(argv...)
		}
		return 1, "", "not found"
	}
	e.getenv = func(k string) string {
		return map[string]string{"USER": "deploy"}[k]
	}
	e.environ = func() []string { return []string{"HOME=/home/deploy", "PATH=/usr/bin"} }
	e.now = func() time.Time { return time.Date(2026, 9, 29, 18, 22, 6, 420030000, time.UTC) }
	e.ids = func() (int, int, int, int) { return 1000, 1000, 1000, 1000 }
	e.nproc = func() int { return 4 }
	e.statvfs = func(string) (map[string]any, bool) {
		return map[string]any{"size_total": int64(4096 * 100), "block_size": int64(4096)}, true
	}
	e.statID = func(string) (uint64, uint64, bool) { return 1, 2, true }
	e.fsType = func(string) int64 { return 0 }
	return e
}

const rockyOSRelease = `NAME="Rocky Linux"
VERSION="9.8 (Blue Onyx)"
ID="rocky"
ID_LIKE="rhel centos fedora"
VERSION_ID="9.8"
PLATFORM_ID="platform:el9"
PRETTY_NAME="Rocky Linux 9.8 (Blue Onyx)"
`

const x86CPUInfo = `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9655P 96-Core Processor
physical id	: 0
siblings	: 4
core id		: 0
cpu cores	: 2
flags		: fpu vme de

processor	: 1
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9655P 96-Core Processor
physical id	: 0
siblings	: 4
core id		: 1
cpu cores	: 2

processor	: 2
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9655P 96-Core Processor
physical id	: 0
siblings	: 4
core id		: 0
cpu cores	: 2

processor	: 3
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9655P 96-Core Processor
physical id	: 0
siblings	: 4
core id		: 1
cpu cores	: 2
`

func rockyFixture(t *testing.T) (*fixture, *factEnv) {
	f := newFixture(t)
	f.file("/etc/os-release", rockyOSRelease).
		file("/etc/rocky-release", "Rocky Linux release 9.8 (Blue Onyx)\n").
		file("/etc/redhat-release", "Rocky Linux release 9.8 (Blue Onyx)\n").
		file("/etc/system-release", "Rocky Linux release 9.8 (Blue Onyx)\n").
		file("/etc/machine-id", "981d62d5a3884107b01531cc57e15c92\n").
		file("/etc/passwd", "root:x:0:0:root:/root:/bin/bash\ndeploy:x:1000:1000:Deploy User,,,:/home/deploy:/bin/zsh\n").
		file("/etc/resolv.conf", "# generated\nsearch svc.cluster.local cluster.local\nnameserver 10.16.0.10\noptions ndots:5 rotate\n").
		file("/etc/ld.so.cache", "xx libselinux.so.1 xx").
		file("/proc/cpuinfo", x86CPUInfo).
		file("/proc/meminfo", "MemTotal:        8000000 kB\nMemFree:         4000000 kB\nBuffers:           10240 kB\nCached:          1024000 kB\nSwapCached:            0 kB\nSwapTotal:       2097152 kB\nSwapFree:        1048576 kB\n").
		file("/proc/cmdline", "BOOT_IMAGE=/vmlinuz root=UUID=abc ro quiet ds=a ds=b console=tty0 nousb\n").
		file("/proc/uptime", "3101743.52 100.00\n").
		file("/proc/loadavg", "1.17 1.18 3.48 1/100 42\n").
		file("/proc/1/comm", "systemd\n").
		file("/proc/1/cgroup", "0::/\n").
		file("/proc/1/environ", "container=oci\x00HOME=/\x00").
		file("/proc/sys/crypto/fips_enabled", "0\n").
		file("/proc/modules", "kvm 1 0 - Live 0x0\nxfs 2 0 - Live 0x0\n").
		file("/proc/mounts", "/dev/sda1 / xfs rw,relatime 0 0\nproc /proc proc rw 0 0\n/dev/sda1 /etc/hosts xfs rw,relatime 0 0\nserver:/export /mnt/nfs nfs rw 0 0\n/dev/sdb /mnt/with\\040space ext4 rw 0 0\n").
		file("/proc/self/mountinfo", "22 1 8:1 / / rw - xfs /dev/sda1 rw\n23 22 8:1 /etc/hosts /etc/hosts rw - xfs /dev/sda1 rw\n").
		dir("/run/systemd/system").
		file("/run/systemd/container", "oci\n").
		file("/usr/bin/dnf", "").file("/usr/bin/yum", "")

	dmi := "/sys/devices/virtual/dmi/id/"
	f.file(dmi+"product_name", "AS -2115HS-TNR\n").
		file(dmi+"sys_vendor", "Supermicro\n").
		file(dmi+"chassis_type", "23\n").
		file(dmi+"bios_vendor", "American Megatrends International, LLC.\n").
		file(dmi+"product_serial", "\n")

	// sda: a SCSI disk with one partition; loop0: virtual device.
	sda := "/sys/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	f.link("/sys/block/sda", "../devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda").
		file(sda+"/device/vendor", "ATA     \n").
		file(sda+"/device/model", "Samsung SSD\n").
		file(sda+"/removable", "0\n").
		file(sda+"/size", "1000215216\n").
		file(sda+"/queue/logical_block_size", "512\n").
		file(sda+"/queue/rotational", "0\n").
		file(sda+"/queue/discard_granularity", "512\n").
		file(sda+"/queue/scheduler", "none [mq-deadline] kyber\n").
		file(sda+"/sda1/start", "2048\n").
		file(sda+"/sda1/size", "2097152\n").
		dir(sda + "/holders").
		dir(sda + "/sda1/holders")
	loop := "/sys/devices/virtual/block/loop0"
	f.link("/sys/block/loop0", "../devices/virtual/block/loop0").
		file(loop+"/size", "0\n").
		file(loop+"/removable", "0\n").
		file(loop+"/queue/logical_block_size", "512\n").
		file(loop+"/queue/rotational", "0\n").
		file(loop+"/queue/scheduler", "none\n").
		file(loop+"/queue/discard_granularity", "0\n").
		dir(loop + "/holders")
	f.link("/dev/disk/by-uuid/1111-2222", "../../sda1").
		link("/dev/disk/by-id/wwn-0x5002538", "../../sda").
		link("/dev/disk/by-id/ata-Samsung_SSD", "../../sda")

	eth := "/sys/devices/virtual/net/eth0"
	f.link("/sys/class/net/eth0", "../../devices/virtual/net/eth0").
		file(eth+"/address", "2e:31:46:95:19:2f\n").
		file(eth+"/mtu", "9000\n").
		file(eth+"/operstate", "up\n").
		file(eth+"/type", "1\n").
		file(eth+"/speed", "10000\n").
		file(eth+"/flags", "0x1003\n")
	lo := "/sys/devices/virtual/net/lo"
	f.link("/sys/class/net/lo", "../../devices/virtual/net/lo").
		file(lo+"/address", "00:00:00:00:00:00\n").
		file(lo+"/mtu", "65536\n").
		file(lo+"/operstate", "unknown\n").
		file(lo+"/type", "772\n").
		file(lo+"/flags", "0x9\n")

	f.file("/etc/ansible/facts.d/citc.fact", `{"csp": "aws", "n": 3, "f": 1.5}`).
		file("/etc/ansible/facts.d/conf.fact", "[DEFAULT]\ndflt = d\n[main]\nKey = value\nmulti = a\n  b\nref = %(key)s-%%\n").
		file("/etc/ansible/facts.d/bad.fact", "neither json\nnor ini\n").
		exec("/etc/ansible/facts.d/script.fact", "#!/bin/sh\necho '{\"ran\": true}'\n").
		exec("/etc/ansible/facts.d/fails.fact", "#!/bin/sh\necho boom >&2\nexit 3\n").
		file("/etc/ansible/facts.d/.hidden.fact", "{}").
		file("/etc/ansible/facts.d/notafact.txt", "{}")

	cmds := map[string]string{
		"/usr/bin/ip -4 route get 8.8.8.8": "8.8.8.8 via 10.0.0.46 dev eth0 src 10.0.0.31 uid 0 \n    cache \n",
		"/usr/bin/ip addr show primary dev eth0": "2: eth0@if3: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 9000\n" +
			"    inet 10.0.0.31/24 brd 10.0.0.255 scope global eth0\n       valid_lft forever preferred_lft forever\n" +
			"    inet6 fe80::2c31:46ff:fe95:192f/64 scope link \n       valid_lft forever preferred_lft forever\n",
		"/usr/bin/ip addr show secondary dev eth0":                                  "    inet 10.0.0.32/24 brd 10.0.0.255 scope global secondary eth0:1\n",
		"/usr/bin/ip addr show primary dev lo":                                      "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536\n    inet 127.0.0.1/8 scope host lo\n    inet6 ::1/128 scope host \n",
		"/usr/bin/ip addr show secondary dev lo":                                    "",
		"/usr/bin/ip -4 route show table local":                                     "local 10.0.0.31 dev eth0 proto kernel scope host src 10.0.0.31\nlocal 127.0.0.0/8 dev lo proto kernel scope host src 127.0.0.1\n",
		"/usr/bin/capsh --print":                                                    "Current: cap_chown,cap_kill=ep\nBounding set =cap_chown\n",
		"/usr/bin/python3 --version":                                                "Python 3.9.25\n",
		"/usr/bin/lsblk --list --noheadings --paths --output NAME,UUID --exclude 2": "/dev/sda\n/dev/sda1 1111-2222\n",
	}
	e := f.env(cmds, "ip", "capsh", "python3", "lsblk", "findmnt", "systemctl")
	return f, e
}

func mustGather(t *testing.T, e *factEnv, subset []string, filter []string) map[string]any {
	t.Helper()
	facts, err := gatherFacts(e, subset, filter)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func expectFact(t *testing.T, facts map[string]any, key string, want any) {
	t.Helper()
	got, ok := facts[key]
	if !ok {
		t.Errorf("%s: missing", key)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", key, got, want)
	}
}

func TestSetupRockyFixture(t *testing.T) {
	_, e := rockyFixture(t)
	facts := mustGather(t, e, []string{"all"}, nil)

	// platform
	expectFact(t, facts, "ansible_system", "Linux")
	expectFact(t, facts, "ansible_kernel", "5.14.0-570.el9.x86_64")
	expectFact(t, facts, "ansible_machine", "x86_64")
	expectFact(t, facts, "ansible_architecture", "x86_64")
	expectFact(t, facts, "ansible_userspace_architecture", "x86_64")
	expectFact(t, facts, "ansible_userspace_bits", "64")
	expectFact(t, facts, "ansible_hostname", "web01")
	expectFact(t, facts, "ansible_nodename", "web01")
	expectFact(t, facts, "ansible_fqdn", "web01.example.com")
	expectFact(t, facts, "ansible_domain", "example.com")
	expectFact(t, facts, "ansible_machine_id", "981d62d5a3884107b01531cc57e15c92")
	expectFact(t, facts, "ansible_python_version", "3.9.25")

	// distribution
	expectFact(t, facts, "ansible_distribution", "Rocky")
	expectFact(t, facts, "ansible_distribution_release", "Blue Onyx")
	expectFact(t, facts, "ansible_distribution_version", "9.8")
	expectFact(t, facts, "ansible_distribution_major_version", "9")
	expectFact(t, facts, "ansible_distribution_file_path", "/etc/redhat-release")
	expectFact(t, facts, "ansible_distribution_file_variety", "RedHat")
	expectFact(t, facts, "ansible_distribution_file_parsed", true)
	expectFact(t, facts, "ansible_os_family", "RedHat")
	expectFact(t, facts, "ansible_pkg_mgr", "dnf")
	expectFact(t, facts, "ansible_service_mgr", "systemd")
	expectFact(t, facts, "ansible_lsb", map[string]any{})

	// restrictive / general
	expectFact(t, facts, "ansible_selinux", map[string]any{"status": "disabled"})
	expectFact(t, facts, "ansible_selinux_python_present", true)
	expectFact(t, facts, "ansible_apparmor", map[string]any{"status": "disabled"})
	expectFact(t, facts, "ansible_fips", false)
	expectFact(t, facts, "ansible_is_chroot", false)
	expectFact(t, facts, "ansible_system_capabilities_enforced", "True")
	expectFact(t, facts, "ansible_system_capabilities", []any{"ep"})
	expectFact(t, facts, "ansible_loadavg", map[string]any{"1m": 1.17, "5m": 1.18, "15m": 3.48})
	expectFact(t, facts, "ansible_cmdline", map[string]any{"BOOT_IMAGE": "/vmlinuz", "root": "UUID=abc",
		"ro": true, "quiet": true, "ds": "b", "console": "tty0", "nousb": true})
	expectFact(t, facts, "ansible_proc_cmdline", map[string]any{"BOOT_IMAGE": "/vmlinuz", "root": "UUID=abc",
		"ro": true, "quiet": true, "ds": []any{"a", "b"}, "console": "tty0", "nousb": true})
	expectFact(t, facts, "ansible_env", map[string]any{"HOME": "/home/deploy", "PATH": "/usr/bin"})
	expectFact(t, facts, "ansible_python", map[string]any{
		"version":        map[string]any{"major": 3, "minor": 9, "micro": 25, "releaselevel": "final", "serial": 0},
		"version_info":   []any{3, 9, 25, "final", 0},
		"executable":     "/usr/bin/python3",
		"has_sslcontext": true,
		"type":           "cpython",
	})

	// user: ints, gecos verbatim
	expectFact(t, facts, "ansible_user_id", "deploy")
	expectFact(t, facts, "ansible_user_uid", 1000)
	expectFact(t, facts, "ansible_user_gid", 1000)
	expectFact(t, facts, "ansible_user_gecos", "Deploy User,,,")
	expectFact(t, facts, "ansible_user_dir", "/home/deploy")
	expectFact(t, facts, "ansible_user_shell", "/bin/zsh")
	expectFact(t, facts, "ansible_real_user_id", 1000)
	expectFact(t, facts, "ansible_effective_group_id", 1000)

	// date_time
	dt := facts["ansible_date_time"].(map[string]any)
	for k, want := range map[string]string{"weekday": "Tuesday", "weekday_number": "2", "weeknumber": "39",
		"epoch": "1790706126", "epoch_int": "1790706126", "iso8601_micro": "2026-09-29T18:22:06.420030Z",
		"iso8601_basic": "20260929T182206420030", "iso8601_basic_short": "20260929T182206", "tz": "UTC",
		"tz_dst": "UTC", "tz_offset": "+0000"} {
		if dt[k] != want {
			t.Errorf("date_time.%s = %v, want %s", k, dt[k], want)
		}
	}

	// dns
	expectFact(t, facts, "ansible_dns", map[string]any{
		"search":      []any{"svc.cluster.local", "cluster.local"},
		"nameservers": []any{"10.16.0.10"},
		"options":     map[string]any{"ndots": "5", "rotate": true},
	})
	expectFact(t, facts, "ansible_iscsi_iqn", "")
	expectFact(t, facts, "ansible_hostnqn", "")
	expectFact(t, facts, "ansible_fibre_channel_wwn", []any{})

	// hardware: processor_count is sockets, not CPUs
	expectFact(t, facts, "ansible_processor_count", 1)
	expectFact(t, facts, "ansible_processor_cores", 2)
	expectFact(t, facts, "ansible_processor_threads_per_core", 2)
	expectFact(t, facts, "ansible_processor_vcpus", 4)
	expectFact(t, facts, "ansible_processor_nproc", 4)
	proc := facts["ansible_processor"].([]any)
	if len(proc) != 12 || proc[0] != "0" || proc[1] != "AuthenticAMD" || proc[2] != "AMD EPYC 9655P 96-Core Processor" {
		t.Errorf("processor = %v", proc)
	}
	expectFact(t, facts, "ansible_memtotal_mb", int64(7812))
	expectFact(t, facts, "ansible_memfree_mb", int64(3906))
	expectFact(t, facts, "ansible_swaptotal_mb", int64(2048))
	expectFact(t, facts, "ansible_swapfree_mb", int64(1024))
	expectFact(t, facts, "ansible_memory_mb", map[string]any{
		"real":    map[string]any{"total": int64(7812), "used": int64(3906), "free": int64(3906)},
		"nocache": map[string]any{"free": int64(3906 + 1000 + 10), "used": int64(7812 - 4916)},
		"swap":    map[string]any{"total": int64(2048), "free": int64(1024), "used": int64(1024), "cached": int64(0)},
	})
	expectFact(t, facts, "ansible_uptime_seconds", int64(3101743))
	expectFact(t, facts, "ansible_lvm", "N/A")
	expectFact(t, facts, "ansible_product_name", "AS -2115HS-TNR")
	expectFact(t, facts, "ansible_system_vendor", "Supermicro")
	expectFact(t, facts, "ansible_form_factor", "Rack Mount Chassis")
	expectFact(t, facts, "ansible_product_serial", "NA")
	expectFact(t, facts, "ansible_board_name", "NA")

	devices := facts["ansible_devices"].(map[string]any)
	sda := devices["sda"].(map[string]any)
	for k, want := range map[string]any{"virtual": 1, "vendor": "ATA", "model": "Samsung SSD",
		"sas_address": nil, "removable": "0", "support_discard": "512", "rotational": "0",
		"scheduler_mode": "mq-deadline", "sectors": "1000215216", "sectorsize": "512",
		"size": "476.94 GB", "host": "", "holders": []any{}, "wwn": "0x5002538",
		"links": map[string]any{"ids": []any{"ata-Samsung_SSD", "wwn-0x5002538"}, "uuids": []any{},
			"labels": []any{}, "masters": []any{}}} {
		if !reflect.DeepEqual(sda[k], want) {
			t.Errorf("devices.sda.%s = %#v, want %#v", k, sda[k], want)
		}
	}
	part := sda["partitions"].(map[string]any)["sda1"].(map[string]any)
	for k, want := range map[string]any{"start": "2048", "sectors": "2097152", "sectorsize": 512,
		"size": "1.00 GB", "uuid": "1111-2222", "holders": []any{}} {
		if !reflect.DeepEqual(part[k], want) {
			t.Errorf("sda1.%s = %#v, want %#v", k, part[k], want)
		}
	}
	loop0 := devices["loop0"].(map[string]any)
	if loop0["size"] != "0.00 Bytes" || loop0["scheduler_mode"] != "" || loop0["model"] != nil {
		t.Errorf("loop0 = %#v", loop0)
	}

	mounts := facts["ansible_mounts"].([]any)
	var mpoints []string
	for _, m := range mounts {
		mm := m.(map[string]any)
		mpoints = append(mpoints, mm["mount"].(string))
		switch mm["mount"] {
		case "/":
			if mm["uuid"] != "1111-2222" && mm["uuid"] != "N/A" || mm["options"] != "rw,relatime" || mm["size_total"] != int64(409600) {
				t.Errorf("/ mount = %#v", mm)
			}
		case "/etc/hosts":
			if mm["options"] != "rw,relatime,bind" {
				t.Errorf("/etc/hosts options = %v", mm["options"])
			}
		case "/mnt/nfs":
			if mm["uuid"] != "N/A" {
				t.Errorf("nfs uuid = %v", mm["uuid"])
			}
		}
	}
	if strings.Join(mpoints, ",") != "/,/etc/hosts,/mnt/nfs,/mnt/with space" {
		t.Errorf("mount points = %v", mpoints)
	}

	// network
	expectFact(t, facts, "ansible_default_ipv4", map[string]any{
		"gateway": "10.0.0.46", "interface": "eth0", "address": "10.0.0.31", "broadcast": "10.0.0.255",
		"netmask": "255.255.255.0", "network": "10.0.0.0", "prefix": "24", "macaddress": "2e:31:46:95:19:2f",
		"mtu": 9000, "type": "ether", "alias": "eth0",
	})
	expectFact(t, facts, "ansible_default_ipv6", map[string]any{})
	expectFact(t, facts, "ansible_all_ipv4_addresses", []any{"10.0.0.31", "10.0.0.32"})
	expectFact(t, facts, "ansible_all_ipv6_addresses", []any{"fe80::2c31:46ff:fe95:192f"})
	ifaces := facts["ansible_interfaces"].([]any)
	var names []string
	for _, n := range ifaces {
		names = append(names, n.(string))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "eth0,eth0_1,lo" {
		t.Errorf("interfaces = %v", names)
	}
	eth0 := facts["ansible_eth0"].(map[string]any)
	for k, want := range map[string]any{"device": "eth0", "macaddress": "2e:31:46:95:19:2f", "mtu": 9000,
		"active": true, "type": "ether", "speed": 10000, "promisc": false,
		"ipv4": map[string]any{"address": "10.0.0.31", "broadcast": "10.0.0.255", "netmask": "255.255.255.0",
			"network": "10.0.0.0", "prefix": "24"},
		"ipv6": []any{map[string]any{"address": "fe80::2c31:46ff:fe95:192f", "prefix": "64", "scope": "link"}},
		"ipv4_secondaries": []any{map[string]any{"address": "10.0.0.32", "broadcast": "10.0.0.255",
			"netmask": "255.255.255.0", "network": "10.0.0.0", "prefix": "24"}},
	} {
		if !reflect.DeepEqual(eth0[k], want) {
			t.Errorf("eth0.%s = %#v, want %#v", k, eth0[k], want)
		}
	}
	lo := facts["ansible_lo"].(map[string]any)
	if _, has := lo["macaddress"]; has || lo["type"] != "loopback" || lo["active"] != true {
		t.Errorf("lo = %#v", lo)
	}
	expectFact(t, facts, "ansible_locally_reachable_ips", map[string]any{
		"ipv4": []any{"10.0.0.31", "127.0.0.0/8"}, "ipv6": []any{}})

	// virtualization: container=oci in PID 1's environment, kvm module loaded
	expectFact(t, facts, "ansible_virtualization_type", "container")
	expectFact(t, facts, "ansible_virtualization_role", "guest")
	expectFact(t, facts, "ansible_virtualization_tech_guest", []any{"container", "oci"})
	expectFact(t, facts, "ansible_virtualization_tech_host", []any{"kvm"})

	// local facts
	local := facts["ansible_local"].(map[string]any)
	wantLocal := map[string]any{
		"citc":   map[string]any{"csp": "aws", "n": int64(3), "f": 1.5},
		"conf":   map[string]any{"main": map[string]any{"key": "value", "multi": "a\nb", "ref": "value-%", "dflt": "d"}},
		"bad":    "error loading facts as JSON or ini - please check content: /etc/ansible/facts.d/bad.fact",
		"script": map[string]any{"ran": true},
		"fails":  "Failure executing fact script (/etc/ansible/facts.d/fails.fact), rc: 3, err: boom\n",
	}
	if !reflect.DeepEqual(local, wantLocal) {
		t.Errorf("ansible_local = %#v\nwant %#v", local, wantLocal)
	}

	expectFact(t, facts, "gather_subset", []any{"all"})
	expectFact(t, facts, "module_setup", true)
}

func TestSetupFilter(t *testing.T) {
	_, e := rockyFixture(t)
	facts := mustGather(t, e, []string{"all"}, []string{"distribution*"})
	var keys []string
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "ansible_distribution,ansible_distribution_file_parsed,ansible_distribution_file_path," +
		"ansible_distribution_file_variety,ansible_distribution_major_version,ansible_distribution_release," +
		"ansible_distribution_version"
	if strings.Join(keys, ",") != want {
		t.Errorf("filtered keys = %v", keys)
	}
	facts = mustGather(t, e, []string{"!all"}, []string{"ansible_*_mb", "gather_subset"})
	keys = keys[:0]
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "gather_subset" { // hardware not gathered under !all
		t.Errorf("keys = %v", keys)
	}
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"ansible_eth[0-9]", "ansible_eth0", true},
		{"eth?", "ansible_eth0", true},
		{"*", "anything", true},
		{"ansible_[!e]*", "ansible_eth0", false},
		{"facter_*", "ansible_facter_x", false},
	} {
		if got := factFilterMatch(c.name, []string{c.pat}); got != c.want {
			t.Errorf("filter %q on %q = %v", c.pat, c.name, got)
		}
	}
}

func TestSetupSubsets(t *testing.T) {
	names := func(subset ...string) string {
		sel, err := selectCollectors(subset)
		if err != nil {
			return "ERR " + err.Error()
		}
		var out []string
		for _, c := range factCollectors {
			if sel[c.name] {
				out = append(out, c.name)
			}
		}
		return strings.Join(out, ",")
	}
	minimal := "platform,distribution,lsb,selinux,apparmor,fips,python,caps,pkg_mgr,service_mgr,cmdline,date_time,env,ssh_pub_keys,user,dns,local"
	for _, c := range []struct {
		subset []string
		want   string
	}{
		{[]string{"!all"}, minimal},
		{[]string{"min"}, minimal},
		{[]string{"!all", "!min"}, ""},
		{[]string{"!all", "!min", "hardware"}, "platform,hardware"},
		{[]string{"!all", "!min", "virtual"}, "virtual"},
		{[]string{"!all", "!min", "pkg_mgr"}, "distribution,pkg_mgr"},
		{[]string{"!all", "!min", "network"}, "platform,distribution,network"},
		{[]string{"!all", "!min", "processor_count", "user_uid"}, "platform,user,hardware"},
		{[]string{"!all", "!min", "local", "!local"}, "local"},
		{[]string{"all", "!hardware", "!network", "!virtual", "!facter", "!ohai"},
			"platform,distribution,lsb,selinux,apparmor,chroot,fips,python,caps,pkg_mgr,service_mgr,cmdline,date_time,env,loadavg,ssh_pub_keys,user,dns,fibre_channel_wwn,iscsi,nvme,local"},
	} {
		if got := names(c.subset...); got != c.want {
			t.Errorf("gather_subset=%v:\n got  %s\n want %s", c.subset, got, c.want)
		}
	}
	if got := names("bogus"); !strings.HasPrefix(got, "ERR Bad subset 'bogus' given to Ansible. gather_subset options allowed: all, all_ipv4_addresses,") {
		t.Errorf("bogus subset: %s", got)
	}
}

func TestSetupUnsupportedParameter(t *testing.T) {
	res := setupModule(&RunEnv{}, map[string]any{"bogus_opt": 1})
	if !res.Failed || res.Msg != "Unsupported parameters for (setup) module: bogus_opt. Supported parameters include: fact_path, filter, gather_subset, gather_timeout." {
		t.Errorf("got %+v", res)
	}
}

func TestSetupLocalFactsMissingDir(t *testing.T) {
	f := newFixture(t)
	e := f.env(nil)
	e.factPath = "/nonexistent"
	got := collectLocal(e, nil)
	if !reflect.DeepEqual(got, map[string]any{"local": map[string]any{}}) {
		t.Errorf("got %#v", got)
	}
}

func TestParseINI(t *testing.T) {
	for _, c := range []struct {
		in   string
		want any // map or "ERR"
	}{
		{"", map[string]any{}},
		{"[s]\na=1\nB : 2\nc =\n", map[string]any{"s": map[string]any{"a": "1", "b": "2", "c": ""}}},
		{"# comment\n; other\n[s]\nk = v1\n  v2\n\n  v3\nx=y\n", map[string]any{"s": map[string]any{"k": "v1\nv2\n\nv3", "x": "y"}}},
		{"k=v\n", "ERR"},                 // no section header
		{"[s]\n[s]\n", "ERR"},            // duplicate section
		{"[s]\na=1\nA=2\n", "ERR"},       // duplicate option (case folded)
		{"[s]\njunk line\n", "ERR"},      // parsing error
		{"[s]\na=%(missing)s\n", "IERR"}, // interpolation error
		{"[s]\na=100%\n", "IERR"},        // bare percent
		{"[s]\na=%(b)s\nb=%(c)s\nc=z\n", map[string]any{"s": map[string]any{"a": "z", "b": "z", "c": "z"}}},
	} {
		ini, err := parseINI(c.in)
		if c.want == "ERR" {
			if err == nil {
				t.Errorf("%q: expected parse error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		got, ierr := ini.toFacts()
		if c.want == "IERR" {
			if ierr == nil {
				t.Errorf("%q: expected interpolation error, got %v", c.in, got)
			}
			continue
		}
		if ierr != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %#v (%v), want %#v", c.in, got, ierr, c.want)
		}
	}
}

func distroFacts(t *testing.T, files map[string]string) map[string]any {
	t.Helper()
	f := newFixture(t)
	for p, c := range files {
		f.file(p, c)
	}
	e := f.env(nil)
	got := collectDistribution(e, map[string]any{})
	for k, v := range got {
		if !strings.HasPrefix(k, "distribution") && k != "os_family" {
			delete(got, k)
		}
		_ = v
	}
	return got
}

func TestDistributions(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  map[string]any
	}{
		{"ubuntu", map[string]string{
			"/etc/os-release":     "NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\nVERSION=\"24.04.1 LTS (Noble Numbat)\"\nVERSION_CODENAME=noble\nID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nUBUNTU_CODENAME=noble\n",
			"/etc/lsb-release":    "DISTRIB_ID=Ubuntu\nDISTRIB_RELEASE=24.04\nDISTRIB_CODENAME=noble\n",
			"/etc/debian_version": "trixie/sid\n",
		}, map[string]any{"distribution": "Ubuntu", "distribution_version": "24.04", "distribution_major_version": "24",
			"distribution_release": "noble", "distribution_file_path": "/etc/os-release", "distribution_file_variety": "Debian",
			"distribution_file_parsed": true, "os_family": "Debian"}},
		{"debian", map[string]string{
			"/etc/os-release":     "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"12\"\nVERSION=\"12 (bookworm)\"\nVERSION_CODENAME=bookworm\nID=debian\n",
			"/etc/debian_version": "12.15\n",
		}, map[string]any{"distribution": "Debian", "distribution_version": "12.15", "distribution_major_version": "12",
			"distribution_minor_version": "15", "distribution_release": "bookworm", "distribution_file_path": "/etc/os-release",
			"distribution_file_variety": "Debian", "distribution_file_parsed": true, "os_family": "Debian"}},
		{"alpine", map[string]string{
			"/etc/os-release":     "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.20.3\nPRETTY_NAME=\"Alpine Linux v3.20\"\n",
			"/etc/alpine-release": "3.20.3\n",
		}, map[string]any{"distribution": "Alpine", "distribution_version": "3.20.3", "distribution_major_version": "3",
			"distribution_release": "NA", "distribution_file_path": "/etc/alpine-release", "distribution_file_variety": "Alpine",
			"distribution_file_parsed": true, "os_family": "Alpine"}},
		{"rhel", map[string]string{
			"/etc/os-release":     "NAME=\"Red Hat Enterprise Linux\"\nVERSION=\"9.4 (Plow)\"\nID=\"rhel\"\nVERSION_ID=\"9.4\"\n",
			"/etc/redhat-release": "Red Hat Enterprise Linux release 9.4 (Plow)\n",
		}, map[string]any{"distribution": "RedHat", "distribution_version": "9.4", "distribution_major_version": "9",
			"distribution_release": "Plow", "distribution_file_path": "/etc/redhat-release", "distribution_file_variety": "RedHat",
			"distribution_file_parsed": true, "distribution_file_search_string": "Red Hat", "os_family": "RedHat"}},
		{"centos7", map[string]string{
			"/etc/os-release":     "NAME=\"CentOS Linux\"\nVERSION=\"7 (Core)\"\nID=\"centos\"\nVERSION_ID=\"7\"\n",
			"/etc/centos-release": "CentOS Linux release 7.9.2009 (Core)\n",
			"/etc/redhat-release": "CentOS Linux release 7.9.2009 (Core)\n",
		}, map[string]any{"distribution": "CentOS", "distribution_version": "7.9", "distribution_major_version": "7",
			"distribution_release": "Core", "distribution_file_path": "/etc/redhat-release", "distribution_file_variety": "RedHat",
			"distribution_file_parsed": true, "os_family": "RedHat"}},
		{"amazon2023", map[string]string{
			"/etc/os-release":     "NAME=\"Amazon Linux\"\nVERSION=\"2023\"\nID=\"amzn\"\nVERSION_ID=\"2023\"\nPLATFORM_ID=\"platform:al2023\"\n",
			"/etc/system-release": "Amazon Linux release 2023.5.20240805 (Amazon Linux)\n",
		}, map[string]any{"distribution": "Amazon", "distribution_version": "2023", "distribution_major_version": "2023",
			"distribution_minor_version": "NA", "distribution_release": "NA", "distribution_file_path": "/etc/os-release",
			"distribution_file_variety": "Amazon", "distribution_file_parsed": true, "os_family": "RedHat"}},
		{"arch", map[string]string{
			"/etc/os-release":   "NAME=\"Arch Linux\"\nID=arch\nBUILD_ID=rolling\n",
			"/etc/arch-release": "",
		}, map[string]any{"distribution": "Archlinux", "distribution_version": "NA", "distribution_major_version": "NA",
			"distribution_release": "NA", "distribution_file_path": "/etc/arch-release", "distribution_file_variety": "Archlinux",
			"os_family": "Archlinux"}},
		{"opensuse", map[string]string{
			"/etc/os-release": "NAME=\"openSUSE Leap\"\nVERSION=\"15.6\"\nID=\"opensuse-leap\"\nVERSION_ID=\"15.6\"\n",
		}, map[string]any{"distribution": "openSUSE Leap", "distribution_version": "15.6", "distribution_major_version": "15",
			"distribution_release": "6", "distribution_file_path": "/etc/os-release", "distribution_file_variety": "SUSE",
			"distribution_file_parsed": true, "os_family": "Suse"}},
	}
	for _, c := range cases {
		got := distroFacts(t, c.files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %#v\n want %#v", c.name, got, c.want)
		}
	}
}

// ansible-core 2.21: on RedHat the fact follows /usr/bin/dnf (or
// microdnf): dnf5 when it resolves to /usr/bin/dnf5; yum alone is unknown.
func TestPkgMgr(t *testing.T) {
	rh := map[string]any{"os_family": "RedHat", "distribution": "Rocky", "distribution_major_version": "9"}
	yumOnly := newFixture(t)
	yumOnly.file("/usr/bin/yum", "")
	dnf4 := newFixture(t)
	dnf4.file("/usr/bin/dnf-3", "").link("/usr/bin/dnf", "dnf-3").file("/usr/bin/yum", "")
	dnf5 := newFixture(t)
	dnf5.file("/usr/bin/dnf5", "").link("/usr/bin/dnf", "dnf5")
	deb := newFixture(t)
	deb.file("/usr/bin/apt-get", "").file("/usr/bin/yum", "")
	for _, c := range []struct {
		f     *fixture
		prior map[string]any
		want  string
	}{
		{yumOnly, rh, "unknown"},
		{dnf4, rh, "dnf"},
		{dnf5, map[string]any{"os_family": "RedHat", "distribution": "Fedora", "distribution_major_version": "42"}, "dnf5"},
		{deb, map[string]any{"os_family": "Debian", "distribution": "Ubuntu"}, "apt"},
		{yumOnly, map[string]any{"os_family": "Suse"}, "dnf"},
	} {
		if got := collectPkgMgr(c.f.env(nil), c.prior)["pkg_mgr"]; got != c.want {
			t.Errorf("%v: got %v want %s", c.prior, got, c.want)
		}
	}
}

func TestServiceMgrShellPID1(t *testing.T) {
	f := newFixture(t)
	f.file("/proc/1/comm", "bash\n").dir("/etc/init.d")
	e := f.env(nil)
	if got := collectServiceMgr(e, map[string]any{"system": "Linux"})["service_mgr"]; got != "sysvinit" {
		t.Errorf("got %v", got)
	}
	f2 := newFixture(t)
	f2.file("/proc/1/comm", "sleep\n")
	if got := collectServiceMgr(f2.env(nil), map[string]any{"system": "Linux"})["service_mgr"]; got != "sleep" {
		t.Errorf("got %v", got)
	}
}

func TestSelinuxEnabled(t *testing.T) {
	f := newFixture(t)
	f.file("/etc/ld.so.cache", "libselinux.so.1").
		file("/sys/fs/selinux/enforce", "1\n").
		file("/sys/fs/selinux/policyvers", "33\n").
		file("/etc/selinux/config", "SELINUX=enforcing\nSELINUXTYPE=targeted\n")
	got := collectSelinux(f.env(nil), nil)
	want := map[string]any{"selinux_python_present": true, "selinux": map[string]any{
		"status": "enabled", "policyvers": 33, "config_mode": "enforcing", "mode": "enforcing", "type": "targeted"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	// No libselinux (e.g. Alpine): ansible can't load its bindings.
	got = collectSelinux(newFixture(t).env(nil), nil)
	if got["selinux_python_present"] != false || got["selinux"].(map[string]any)["status"] != "Missing selinux Python library" {
		t.Errorf("got %#v", got)
	}
}

func TestLVMFacts(t *testing.T) {
	f := newFixture(t)
	e := f.env(map[string]string{
		"/usr/bin/vgs --noheadings --nosuffix --units g --separator ,": "  vg0,1,2,0,wz--n-,99.00,10.00\n",
		"/usr/bin/lvs --noheadings --nosuffix --units g --separator ,": "  root,vg0,-wi-ao----,50.00,,,,,,,,\n",
		"/usr/bin/pvs --noheadings --nosuffix --units g --separator ,": "  /dev/sda2,vg0,lvm2,a--,99.00,10.00\n",
	}, "vgs", "lvs", "pvs")
	e.ids = func() (int, int, int, int) { return 0, 0, 0, 0 }
	got := linuxLVMFacts(e)
	want := map[string]any{"lvm": map[string]any{
		"vgs": map[string]any{"vg0": map[string]any{"size_g": "99.00", "free_g": "10.00", "num_lvs": "2", "num_pvs": "1"}},
		"lvs": map[string]any{"root": map[string]any{"size_g": "50.00", "vg": "vg0"}},
		"pvs": map[string]any{"/dev/sda2": map[string]any{"size_g": "99.00", "free_g": "10.00", "vg": "vg0"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
}

func TestBytesToHuman(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{0, "0.00 Bytes"}, {512, "512.00 Bytes"}, {1024, "1.00 KB"},
		{15002931888 * 512, "6.99 TB"}, {1 << 30, "1.00 GB"},
	} {
		if got := bytesToHuman(c.in); got != c.want {
			t.Errorf("bytesToHuman(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDateTimeDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	e := newFixture(t).env(nil)
	e.now = func() time.Time { return time.Date(2026, 1, 5, 9, 0, 0, 0, loc) }
	dt := collectDateTime(e, nil)["date_time"].(map[string]any)
	if dt["tz"] != "EST" || dt["tz_dst"] != "EDT" || dt["tz_offset"] != "-0500" || dt["weeknumber"] != "01" || dt["iso8601"] != "2026-01-05T14:00:00Z" {
		t.Errorf("got %#v", dt)
	}
}

func TestReverseAddrName(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":   "1.0.0.127.in-addr.arpa",
		"::1":         "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa",
		"fe80::1%lo0": "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.e.f.ip6.arpa",
		"nonsense":    "",
	} {
		if got := reverseAddrName(addr); got != want {
			t.Errorf("reverseAddrName(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A PyPy interpreter's facts name its implementation (sys.implementation.name).
func TestSetupPythonPyPy(t *testing.T) {
	f := newFixture(t)
	e := f.env(map[string]string{
		"/usr/bin/python3 --version": "Python 3.10.14 (39dc8d3c85a7, Aug 27 2024, 14:33:33)\n[PyPy 7.3.17 with GCC 10.2.1 20210130]\n",
	}, "python3")
	facts := mustGather(t, e, []string{"!all", "python"}, nil)
	py, _ := facts["ansible_python"].(map[string]any)
	if py["type"] != "pypy" {
		t.Errorf("type = %#v", py["type"])
	}
}
