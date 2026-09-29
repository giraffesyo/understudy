package modules

import (
	"encoding/binary"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Network facts: ansible's LinuxNetwork (module_utils/facts/network/linux.py),
// plus the virtualization collector.

var interfaceTypes = map[string]string{"1": "ether", "32": "infiniband", "512": "ppp",
	"772": "loopback", "65534": "tunnel"}

func collectNetwork(e *factEnv, prior map[string]any) map[string]any {
	switch e.system {
	case "Linux":
		return linuxNetwork(e, prior)
	default:
		return genericNetwork()
	}
}

// ordered is an insertion-ordered string-keyed map (Python dict order).
type ordered struct {
	keys []string
	m    map[string]map[string]any
}

func (o *ordered) set(k string, v map[string]any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func linuxNetwork(e *factEnv, prior map[string]any) map[string]any {
	ip := e.binPath("ip")
	if ip == "" {
		return map[string]any{}
	}
	v4, v6 := defaultInterfaces(e, ip, prior)
	ifaces := &ordered{m: map[string]map[string]any{}}
	all4, all6 := []any{}, []any{}
	for _, p := range e.glob("/sys/class/net", "*") {
		if !e.isDir(p) {
			continue
		}
		device := p[strings.LastIndex(p, "/")+1:]
		iface := map[string]any{"device": device}
		ifaces.set(device, iface)
		macaddress := ""
		if e.exists(p + "/address") {
			macaddress = e.fileContentOr(p+"/address", "")
			if macaddress != "" && macaddress != "00:00:00:00:00:00" {
				iface["macaddress"] = macaddress
			}
		}
		if e.exists(p + "/mtu") {
			if n, err := strconv.Atoi(e.fileContentOr(p+"/mtu", "")); err == nil {
				iface["mtu"] = n
			}
		}
		if e.exists(p + "/operstate") {
			iface["active"] = e.fileContentOr(p+"/operstate", "") != "down"
		}
		if e.exists(p + "/device/driver/module") {
			rp := e.realpath(p + "/device/driver/module")
			iface["module"] = rp[strings.LastIndex(rp, "/")+1:]
		}
		if e.exists(p + "/type") {
			t, _ := e.fileContent(p + "/type")
			if name, ok := interfaceTypes[t]; ok {
				iface["type"] = name
			} else {
				iface["type"] = "unknown"
			}
		}
		if e.exists(p + "/bridge") {
			iface["type"] = "bridge"
			members := []any{}
			for _, b := range e.glob(p+"/brif", "*") {
				members = append(members, b[strings.LastIndex(b, "/")+1:])
			}
			iface["interfaces"] = members
			if e.exists(p + "/bridge/bridge_id") {
				iface["id"] = e.fileContentOr(p+"/bridge/bridge_id", "")
			}
			if e.exists(p + "/bridge/stp_state") {
				iface["stp"] = e.fileContentOr(p+"/bridge/stp_state", "") == "1"
			}
		}
		if e.exists(p + "/bonding") {
			iface["type"] = "bonding"
			iface["slaves"] = stringsToAny(strings.Fields(e.fileContentOr(p+"/bonding/slaves", "")))
			first := func(s string) string {
				if f := strings.Fields(s); len(f) > 0 {
					return f[0]
				}
				return ""
			}
			iface["mode"] = first(e.fileContentOr(p+"/bonding/mode", ""))
			iface["miimon"] = first(e.fileContentOr(p+"/bonding/miimon", ""))
			iface["lacp_rate"] = first(e.fileContentOr(p+"/bonding/lacp_rate", ""))
			if primary, ok := e.fileContent(p + "/bonding/primary"); ok {
				iface["primary"] = primary
				if e.exists(p + "/bonding/all_slaves_active") {
					iface["all_slaves_active"] = e.fileContentOr(p+"/bonding/all_slaves_active", "") == "1"
				}
			}
		}
		if e.exists(p + "/bonding_slave") {
			iface["perm_macaddress"] = e.fileContentOr(p+"/bonding_slave/perm_hwaddr", "")
		}
		if e.exists(p + "/device") {
			if t, err := e.readlink(p + "/device"); err == nil {
				iface["pciid"] = t[strings.LastIndex(t, "/")+1:]
			}
		}
		if e.exists(p + "/speed") {
			if s, ok := e.fileContent(p + "/speed"); ok {
				if n, err := strconv.Atoi(s); err == nil {
					iface["speed"] = n
				}
			}
		}
		if e.exists(p + "/flags") {
			s := strings.TrimPrefix(e.fileContentOr(p+"/flags", "0"), "0x")
			n, _ := strconv.ParseInt(s, 16, 64)
			iface["promisc"] = n&0x0100 > 0
		}

		parse := func(output string, secondary bool) {
			for _, line := range strings.Split(output, "\n") {
				if line == "" {
					continue
				}
				words := strings.Fields(line)
				if len(words) == 0 {
					continue
				}
				switch words[0] {
				case "inet":
					if len(words) < 2 {
						continue
					}
					var address, plen string
					broadcast := ""
					if a, l, ok := strings.Cut(words[1], "/"); ok {
						address, plen = a, l
						if len(words) > 3 && words[2] == "brd" {
							broadcast = words[3]
						}
					} else {
						address, plen = words[1], "32"
					}
					netmask, network := ipv4MaskNet(address, plen)
					label := words[len(words)-1]
					if label != device {
						ifaces.set(label, map[string]any{})
					}
					entry := func() map[string]any {
						return map[string]any{"address": address, "broadcast": broadcast,
							"netmask": netmask, "network": network, "prefix": plen}
					}
					target := ifaces.m[label]
					if _, has := target["ipv4"]; !secondary && !has {
						target["ipv4"] = entry()
					} else {
						sec, _ := target["ipv4_secondaries"].([]any)
						target["ipv4_secondaries"] = append(sec, entry())
					}
					if secondary {
						main := ifaces.m[device]
						sec, _ := main["ipv4_secondaries"].([]any)
						if sec == nil {
							sec = []any{}
						}
						if device != label {
							sec = append(sec, entry())
						}
						main["ipv4_secondaries"] = sec
					}
					if a, ok := v4["address"]; ok && a == address {
						v4["broadcast"] = broadcast
						v4["netmask"] = netmask
						v4["network"] = network
						v4["prefix"] = plen
						v4["macaddress"] = macaddress
						v4["mtu"] = ifaces.m[device]["mtu"]
						v4["type"] = typeOr(ifaces.m[device])
						v4["alias"] = label
					}
					if !strings.HasPrefix(address, "127.") {
						all4 = append(all4, address)
					}
				case "inet6":
					if len(words) < 4 {
						continue
					}
					var address, prefix, scope string
					if words[2] == "peer" {
						if len(words) < 6 {
							continue
						}
						address = words[1]
						_, prefix, _ = strings.Cut(words[3], "/")
						scope = words[5]
					} else {
						address, prefix, _ = strings.Cut(words[1], "/")
						scope = words[3]
					}
					main := ifaces.m[device]
					l, _ := main["ipv6"].([]any)
					main["ipv6"] = append(l, map[string]any{"address": address, "prefix": prefix, "scope": scope})
					if a, ok := v6["address"]; ok && a == address {
						v6["prefix"] = prefix
						v6["scope"] = scope
						v6["macaddress"] = macaddress
						v6["mtu"] = ifaces.m[device]["mtu"]
						v6["type"] = typeOr(ifaces.m[device])
					}
					if address != "::1" {
						all6 = append(all6, address)
					}
				}
			}
		}
		if rc, out, _ := e.run(ip, "addr", "show", "primary", "dev", device); rc == 0 {
			parse(out, false)
		} else if rc, out, _ := e.run(ip, "addr", "show", "dev", device); rc == 0 {
			parse(out, false)
		}
		if rc, out, _ := e.run(ip, "addr", "show", "secondary", "dev", device); rc == 0 {
			parse(out, true)
		}
		for k, v := range ethtoolData(e, device) {
			ifaces.m[device][k] = v
		}
	}

	f := map[string]any{}
	names := []any{}
	for _, k := range ifaces.keys {
		name := strings.ReplaceAll(k, ":", "_")
		names = append(names, name)
		f[name] = ifaces.m[k]
	}
	f["interfaces"] = names
	f["default_ipv4"] = v4
	f["default_ipv6"] = v6
	f["all_ipv4_addresses"] = all4
	f["all_ipv6_addresses"] = all6
	f["locally_reachable_ips"] = locallyReachableIPs(e, ip)
	return f
}

// locallyReachableIPs is ansible-core 2.15+'s get_locally_reachable_ips.
func locallyReachableIPs(e *factEnv, ip string) map[string]any {
	v4, v6 := []any{}, []any{}
	seen := map[string]bool{}
	for _, fam := range []string{"-4", "-6"} {
		rc, out, _ := e.run(ip, fam, "route", "show", "table", "local")
		if rc != 0 {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			words := strings.Fields(line)
			if len(words) < 2 || words[0] != "local" || seen[words[1]] {
				continue
			}
			seen[words[1]] = true
			if strings.Contains(words[1], ":") {
				v6 = append(v6, words[1])
			} else {
				v4 = append(v4, words[1])
			}
		}
	}
	return map[string]any{"ipv4": v4, "ipv6": v6}
}

func typeOr(m map[string]any) any {
	if t, ok := m["type"]; ok {
		return t
	}
	return "unknown"
}

func ipv4MaskNet(address, plen string) (string, string) {
	n, err := strconv.Atoi(plen)
	ip := net.ParseIP(address).To4()
	if err != nil || ip == nil || n < 0 || n > 32 {
		return "", ""
	}
	mask := uint32(0)
	if n > 0 {
		mask = ^uint32(0) << (32 - n)
	}
	addr := binary.BigEndian.Uint32(ip)
	var mb, nb [4]byte
	binary.BigEndian.PutUint32(mb[:], mask)
	binary.BigEndian.PutUint32(nb[:], addr&mask)
	return net.IP(mb[:]).String(), net.IP(nb[:]).String()
}

func defaultInterfaces(e *factEnv, ip string, prior map[string]any) (map[string]any, map[string]any) {
	cmds := []struct {
		family, dest string
	}{{"-4", "8.8.8.8"}, {"-6", "2404:6800:400a:800::1012"}}
	res := []map[string]any{{}, {}}
	for i, c := range cmds {
		if i == 1 {
			fam, _ := prior["os_family"].(string)
			ver, _ := prior["distribution_version"].(string)
			if fam == "RedHat" && strings.HasPrefix(ver, "4.") {
				continue
			}
		}
		_, out, _ := e.run(ip, c.family, "route", "get", c.dest)
		if out == "" {
			continue
		}
		words := strings.Fields(strings.SplitN(out, "\n", 2)[0])
		if len(words) == 0 || words[0] != c.dest {
			continue
		}
		for j := 0; j < len(words)-1; j++ {
			switch {
			case words[j] == "dev":
				res[i]["interface"] = words[j+1]
			case words[j] == "src":
				res[i]["address"] = words[j+1]
			case words[j] == "via" && words[j+1] != c.dest:
				res[i]["gateway"] = words[j+1]
			}
		}
	}
	return res[0], res[1]
}

var (
	tsRe  = regexp.MustCompile(`SOF_TIMESTAMPING_(\w+)`)
	hwtRe = regexp.MustCompile(`HWTSTAMP_FILTER_(\w+)`)
	phcRe = regexp.MustCompile(`PTP Hardware Clock: (\d+)`)
)

func ethtoolData(e *factEnv, device string) map[string]any {
	data := map[string]any{}
	ethtool := e.binPath("ethtool")
	if ethtool == "" {
		return data
	}
	if rc, out, _ := e.run(ethtool, "-k", device); rc == 0 {
		features := map[string]any{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" || strings.HasSuffix(line, ":") {
				continue
			}
			k, v, ok := strings.Cut(line, ": ")
			if !ok || v == "" {
				continue
			}
			features[strings.ReplaceAll(strings.TrimSpace(k), "-", "_")] = strings.TrimSpace(v)
		}
		data["features"] = features
	}
	if rc, out, _ := e.run(ethtool, "-T", device); rc == 0 {
		lower := func(ms [][]string) []any {
			l := []any{}
			for _, m := range ms {
				l = append(l, strings.ToLower(m[1]))
			}
			return l
		}
		data["timestamping"] = lower(tsRe.FindAllStringSubmatch(out, -1))
		data["hw_timestamp_filters"] = lower(hwtRe.FindAllStringSubmatch(out, -1))
		if m := phcRe.FindStringSubmatch(out); m != nil {
			n, _ := strconv.Atoi(m[1])
			data["phc_index"] = n
		}
	}
	return data
}

// genericNetwork is a best-effort fallback for non-Linux hosts (e.g. a
// macOS control node running local tasks): addresses from the Go runtime.
func genericNetwork() map[string]any {
	ifs, err := net.Interfaces()
	if err != nil {
		return map[string]any{}
	}
	all4, all6, names := []any{}, []any{}, []any{}
	var def4 map[string]any
	f := map[string]any{}
	for _, ifc := range ifs {
		names = append(names, ifc.Name)
		entry := map[string]any{"device": ifc.Name, "mtu": ifc.MTU, "active": ifc.Flags&net.FlagUp != 0}
		if len(ifc.HardwareAddr) > 0 {
			entry["macaddress"] = ifc.HardwareAddr.String()
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if v4 := ipn.IP.To4(); v4 != nil {
				mask, network := ipv4MaskNet(v4.String(), strconv.Itoa(ones))
				rec := map[string]any{"address": v4.String(), "netmask": mask, "network": network,
					"prefix": strconv.Itoa(ones), "broadcast": ""}
				if _, has := entry["ipv4"]; !has {
					entry["ipv4"] = rec
				}
				if !v4.IsLoopback() {
					all4 = append(all4, v4.String())
					if def4 == nil && ifc.Flags&net.FlagUp != 0 {
						def4 = map[string]any{"address": v4.String(), "interface": ifc.Name,
							"netmask": mask, "network": network, "prefix": strconv.Itoa(ones)}
					}
				}
			} else {
				l, _ := entry["ipv6"].([]any)
				entry["ipv6"] = append(l, map[string]any{"address": ipn.IP.String(), "prefix": strconv.Itoa(ones)})
				if !ipn.IP.IsLoopback() {
					all6 = append(all6, ipn.IP.String())
				}
			}
		}
		f[strings.ReplaceAll(ifc.Name, ":", "_")] = entry
	}
	if def4 == nil {
		def4 = map[string]any{}
	}
	f["interfaces"] = names
	f["all_ipv4_addresses"] = all4
	f["all_ipv6_addresses"] = all6
	f["default_ipv4"] = def4
	f["default_ipv6"] = map[string]any{}
	return f
}

// --- virtualization -------------------------------------------------------

type strSet struct {
	order []string
	has   map[string]bool
}

func (s *strSet) add(v string) {
	if s.has == nil {
		s.has = map[string]bool{}
	}
	if !s.has[v] {
		s.has[v] = true
		s.order = append(s.order, v)
	}
}

func (s *strSet) list() []any {
	l := append([]string(nil), s.order...)
	sort.Strings(l)
	return stringsToAny(l)
}

func collectVirtual(e *factEnv, _ map[string]any) map[string]any {
	if e.system != "Linux" {
		return map[string]any{"virtualization_type": "", "virtualization_role": "",
			"virtualization_tech_guest": []any{}, "virtualization_tech_host": []any{}}
	}
	vf := map[string]any{}
	found := false
	var host, guest strSet
	setGuest := func(tech, typ string) {
		guest.add(tech)
		if !found {
			vf["virtualization_type"] = typ
			vf["virtualization_role"] = "guest"
			found = true
		}
	}

	if e.exists("/proc/1/cgroup") {
		dockerRe := regexp.MustCompile(`/docker(/|-[0-9a-f]+\.scope)`)
		for _, line := range e.fileLines("/proc/1/cgroup") {
			if dockerRe.MatchString(line) {
				setGuest("docker", "docker")
			}
			if strings.Contains(line, "/lxc/") || strings.Contains(line, "/machine.slice/machine-lxc") {
				setGuest("lxc", "lxc")
			}
			if strings.Contains(line, "/system.slice/containerd.service") {
				setGuest("containerd", "containerd")
			}
		}
	}
	if e.exists("/proc/1/environ") {
		if data, ok := e.fileContent("/proc/1/environ"); ok {
			for _, line := range strings.Split(data, "\x00") {
				if strings.Contains(line, "container=lxc") {
					setGuest("lxc", "lxc")
				}
				if strings.Contains(line, "container=podman") {
					setGuest("podman", "podman")
				}
				if strings.HasPrefix(line, "container=") && len(line) > len("container=") {
					setGuest("container", "container")
				}
			}
		}
	}
	if e.exists("/proc/vz") && !e.exists("/proc/lve") {
		vf["virtualization_type"] = "openvz"
		if e.exists("/proc/bc") {
			host.add("openvz")
			if !found {
				vf["virtualization_role"] = "host"
			}
		} else {
			guest.add("openvz")
			if !found {
				vf["virtualization_role"] = "guest"
			}
		}
		found = true
	}
	systemdContainer, _ := e.fileContent("/run/systemd/container")
	if systemdContainer != "" {
		setGuest(systemdContainer, systemdContainer)
	}
	if e.exists("/.dockerenv") || e.exists("/.dockerinit") {
		setGuest("docker", "docker")
	}
	for _, t := range []string{"docker", "lxc", "podman", "openvz", "containerd"} {
		if guest.has[t] {
			guest.add("container")
			break
		}
	}
	if systemdContainer != "" {
		guest.add("container")
	}

	if e.exists("/proc/xen") {
		isHost := false
		for _, line := range e.fileLines("/proc/xen/capabilities") {
			if strings.Contains(line, "control_d") {
				isHost = true
			}
		}
		if isHost {
			host.add("xen")
			if !found {
				vf["virtualization_type"] = "xen"
				vf["virtualization_role"] = "host"
			}
		} else if !found {
			vf["virtualization_type"] = "xen"
			vf["virtualization_role"] = "guest"
		}
		found = true
	}

	if !found {
		vf["virtualization_role"] = "guest"
	}
	productName, _ := e.fileContent("/sys/devices/virtual/dmi/id/product_name")
	sysVendor, _ := e.fileContent("/sys/devices/virtual/dmi/id/sys_vendor")
	productFamily, _ := e.fileContent("/sys/devices/virtual/dmi/id/product_family")
	setType := func(tech, typ string) {
		guest.add(tech)
		if !found {
			vf["virtualization_type"] = typ
			found = true
		}
	}
	switch productName {
	case "KVM", "KVM Server", "Bochs", "AHV":
		setType("kvm", "kvm")
	}
	if sysVendor == "oVirt" {
		setType("oVirt", "oVirt")
	}
	if sysVendor == "Red Hat" {
		if productFamily == "RHV" {
			setType("RHV", "RHV")
		} else if productName == "RHEV Hypervisor" {
			setType("RHEV", "RHEV")
		}
	}
	switch productName {
	case "VMware Virtual Platform", "VMware7,1", "VMware20,1":
		setType("VMware", "VMware")
	case "OpenStack Compute", "OpenStack Nova":
		setType("openstack", "openstack")
	}
	biosVendor, _ := e.fileContent("/sys/devices/virtual/dmi/id/bios_vendor")
	switch biosVendor {
	case "Xen":
		setType("xen", "xen")
	case "innotek GmbH":
		setType("virtualbox", "virtualbox")
	case "Amazon EC2", "DigitalOcean", "Hetzner":
		setType("kvm", "kvm")
	}
	switch sysVendor {
	case "QEMU", "Amazon EC2", "DigitalOcean", "Google", "Scaleway", "Nutanix":
		setType("kvm", "kvm")
	case "KubeVirt":
		setType("KubeVirt", "KubeVirt")
	case "Microsoft Corporation":
		setType("VirtualPC", "VirtualPC")
	case "Parallels Software International Inc.":
		setType("parallels", "parallels")
	case "OpenStack Foundation":
		setType("openstack", "openstack")
	}
	if !found {
		delete(vf, "virtualization_role")
	}

	vxRe := regexp.MustCompile(`^VxID:\s+\d+`)
	for _, line := range e.fileLines("/proc/self/status") {
		if vxRe.MatchString(line) {
			if !found {
				vf["virtualization_type"] = "linux_vserver"
			}
			if regexp.MustCompile(`^VxID:\s+0`).MatchString(line) {
				host.add("linux_vserver")
				if !found {
					vf["virtualization_role"] = "host"
				}
			} else {
				guest.add("linux_vserver")
				if !found {
					vf["virtualization_role"] = "guest"
				}
			}
			found = true
		}
	}

	cpuRules := []struct {
		re        *regexp.Regexp
		tech, typ string
	}{
		{regexp.MustCompile(`^model name.*QEMU Virtual CPU`), "kvm", "kvm"},
		{regexp.MustCompile(`^vendor_id.*User Mode Linux`), "uml", "uml"},
		{regexp.MustCompile(`^model name.*UML`), "uml", "uml"},
		{regexp.MustCompile(`^machine.*CHRP IBM pSeries .emulated by qemu.`), "kvm", "kvm"},
		{regexp.MustCompile(`^vendor_id.*PowerVM Lx86`), "powervm_lx86", "powervm_lx86"},
		{regexp.MustCompile(`^vendor_id.*IBM/S390`), "PR/SM", "PR/SM"},
	}
	for _, line := range e.fileLines("/proc/cpuinfo") {
		matched := false
		for _, r := range cpuRules {
			if !r.re.MatchString(line) {
				continue
			}
			matched = true
			guest.add(r.tech)
			if !found {
				vf["virtualization_type"] = r.typ
			}
			if r.typ == "PR/SM" {
				if lscpu := e.binPath("lscpu"); lscpu != "" {
					if rc, out, _ := e.run(lscpu); rc == 0 {
						for _, l := range strings.Split(out, "\n") {
							k, v, _ := strings.Cut(l, ":")
							if strings.TrimSpace(k) == "Hypervisor" {
								tech := strings.TrimSpace(v)
								guest.add(tech)
								if !found {
									vf["virtualization_type"] = tech
								}
							}
						}
					}
				} else {
					guest.add("ibm_systemz")
					if !found {
						vf["virtualization_type"] = "ibm_systemz"
					}
				}
			}
			break
		}
		if !matched {
			continue
		}
		if !found {
			if vf["virtualization_type"] == "PR/SM" {
				vf["virtualization_role"] = "LPAR"
			} else {
				vf["virtualization_role"] = "guest"
			}
			found = true
		}
	}

	if lines := e.fileLines("/proc/modules"); lines != nil {
		mods := map[string]bool{}
		for _, l := range lines {
			name, _, _ := strings.Cut(l, " ")
			mods[name] = true
		}
		if mods["kvm"] {
			host.add("kvm")
			if !found {
				vf["virtualization_type"] = "kvm"
				vf["virtualization_role"] = "host"
				if e.isDir("/rhev/") {
					for _, c := range e.glob("/proc", "[0-9]*") {
						if comm, _ := e.fileContent(c + "/comm"); comm == "vdsm" || comm == "vdsmd" {
							vf["virtualization_type"] = "RHEV"
							break
						}
					}
				}
				found = true
			}
		}
		if mods["vboxdrv"] {
			host.add("virtualbox")
			if !found {
				vf["virtualization_type"] = "virtualbox"
				vf["virtualization_role"] = "host"
				found = true
			}
		}
		if mods["virtio"] {
			host.add("kvm")
			if !found {
				vf["virtualization_type"] = "kvm"
				vf["virtualization_role"] = "guest"
				found = true
			}
		}
	}

	if dmi := e.binPath("dmidecode"); dmi != "" {
		if rc, out, _ := e.run(dmi, "-s", "system-product-name"); rc == 0 {
			var b strings.Builder
			for _, l := range strings.Split(out, "\n") {
				if !strings.HasPrefix(l, "#") {
					b.WriteString(strings.TrimSpace(l))
				}
			}
			if strings.HasPrefix(b.String(), "VMware") {
				setGuest("VMware", "VMware")
			}
			if strings.Contains(out, "BHYVE") {
				setGuest("bhyve", "bhyve")
			}
		}
	}
	if e.exists("/dev/kvm") {
		host.add("kvm")
		if !found {
			vf["virtualization_type"] = "kvm"
			vf["virtualization_role"] = "host"
			found = true
		}
	}
	if !found {
		vf["virtualization_type"] = "NA"
		vf["virtualization_role"] = "NA"
	}
	vf["virtualization_tech_guest"] = guest.list()
	vf["virtualization_tech_host"] = host.list()
	return vf
}
