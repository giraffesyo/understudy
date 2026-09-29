package modules

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hardware facts: ansible's LinuxHardware (module_utils/facts/hardware/linux.py).

func collectHardware(e *factEnv, prior map[string]any) map[string]any {
	f := map[string]any{}
	switch e.system {
	case "Linux":
	case "Darwin":
		return darwinHardware(e)
	default:
		return f
	}
	merge := func(m map[string]any) {
		for k, v := range m {
			f[k] = v
		}
	}
	merge(linuxCPUFacts(e, prior))
	merge(linuxMemoryFacts(e))
	merge(linuxDMIFacts(e))
	merge(linuxDeviceFacts(e))
	merge(linuxUptimeFacts(e))
	merge(linuxLVMFacts(e))
	merge(linuxMountFacts(e))
	return f
}

func darwinHardware(e *factEnv) map[string]any {
	f := map[string]any{}
	sysctl := e.binPath("sysctl")
	if sysctl == "" {
		return f
	}
	get := func(name string) string {
		rc, out, _ := e.run(sysctl, "-n", name)
		if rc != 0 {
			return ""
		}
		return strings.TrimSpace(out)
	}
	if v := get("machdep.cpu.brand_string"); v != "" {
		f["processor"] = v
	}
	if n, err := strconv.Atoi(get("hw.physicalcpu")); err == nil {
		f["processor_cores"] = n
	}
	if n, err := strconv.Atoi(get("hw.logicalcpu")); err == nil {
		f["processor_vcpus"] = n
	}
	if n, err := strconv.ParseInt(get("hw.memsize"), 10, 64); err == nil {
		f["memtotal_mb"] = n / 1024 / 1024
	}
	if v := get("hw.model"); v != "" {
		f["model"] = v
		f["product_name"] = v
	}
	return f
}

func linuxCPUFacts(e *factEnv, prior map[string]any) map[string]any {
	f := map[string]any{}
	xen := false
	if e.exists("/proc/xen") {
		xen = true
	} else if lines := e.fileLines("/sys/hypervisor/type"); len(lines) > 0 && strings.TrimSpace(lines[0]) == "xen" {
		xen = true
	}
	lines := e.fileLines("/proc/cpuinfo")
	if lines == nil && !e.exists("/proc/cpuinfo") {
		return f
	}
	processor := []any{}
	i := 0
	vendorOcc, modelOcc, procOcc := 0, 0, 0
	xenParavirt := false
	// Python dicts keyed by physical/core id; the initial physid/coreid is
	// the int 0, distinct from the string "0".
	const intZero = "\x00int0"
	physid, coreid := intZero, intZero
	var sockKeys, coreKeys []string
	sockets := map[string]int{}
	cores := map[string]int{}
	setSock := func(k string, v int) {
		if _, ok := sockets[k]; !ok {
			sockKeys = append(sockKeys, k)
		}
		sockets[k] = v
	}
	setCore := func(k string, v int) {
		if _, ok := cores[k]; !ok {
			coreKeys = append(coreKeys, k)
		}
		cores[k] = v
	}
	for _, line := range lines {
		key, val, _ := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if xen && key == "flags" && !strings.Contains(val, "vme") {
			xenParavirt = true
		}
		switch key {
		case "model name", "Processor", "vendor_id", "cpu", "Vendor", "processor":
			processor = append(processor, val)
			switch key {
			case "model name":
				modelOcc++
			case "vendor_id":
				vendorOcc++
			case "processor":
				procOcc++
			}
			i++
		case "physical id":
			physid = val
			if _, ok := sockets[physid]; !ok {
				setSock(physid, 1)
			}
		case "core id":
			coreid = val
			if _, ok := sockets[coreid]; !ok { // sic: ansible checks sockets
				setCore(coreid, 1)
			}
		case "cpu cores":
			n, _ := strconv.Atoi(val)
			setSock(physid, n)
		case "siblings":
			n, _ := strconv.Atoi(val)
			setCore(coreid, n)
		case "ncpus active":
			i, _ = strconv.Atoi(val)
		}
	}
	f["processor"] = processor
	if vendorOcc > 0 && vendorOcc == modelOcc {
		i = vendorOcc
	}
	arch, _ := prior["architecture"].(string)
	if strings.HasPrefix(arch, "armv") || strings.HasPrefix(arch, "aarch") || strings.HasPrefix(arch, "ppc") {
		i = procOcc
	}
	if arch == "s390x" {
		return f
	}
	if xenParavirt {
		f["processor_count"] = i
		f["processor_cores"] = i
		f["processor_threads_per_core"] = 1
		f["processor_vcpus"] = i
		f["processor_nproc"] = procOcc
	} else {
		count := i
		if len(sockets) > 0 {
			count = len(sockets)
		}
		f["processor_count"] = count
		coresPer := 1
		if len(sockKeys) > 0 && sockets[sockKeys[0]] != 0 {
			coresPer = sockets[sockKeys[0]]
		}
		f["processor_cores"] = coresPer
		tpc := 1 / coresPer
		if len(coreKeys) > 0 {
			tpc = cores[coreKeys[0]] / coresPer
		}
		f["processor_threads_per_core"] = tpc
		f["processor_vcpus"] = tpc * count * coresPer
		f["processor_nproc"] = procOcc
		if e.nproc != nil {
			if n := e.nproc(); n > 0 {
				f["processor_nproc"] = n
			}
		}
	}
	return f
}

func linuxMemoryFacts(e *factEnv) map[string]any {
	lines := e.fileLines("/proc/meminfo")
	if lines == nil {
		return map[string]any{}
	}
	f := map[string]any{}
	stats := map[string]int64{}
	orig := map[string]bool{"MemTotal": true, "SwapTotal": true, "MemFree": true, "SwapFree": true}
	extra := map[string]bool{"Buffers": true, "Cached": true, "SwapCached": true}
	for _, line := range lines {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || !(orig[key] || extra[key]) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		mb := n / 1024
		if orig[key] {
			f[strings.ToLower(key)+"_mb"] = mb
		}
		stats[strings.ToLower(key)] = mb
	}
	get := func(k string) any {
		if v, ok := stats[k]; ok {
			return v
		}
		return nil
	}
	has := func(ks ...string) bool {
		for _, k := range ks {
			if _, ok := stats[k]; !ok {
				return false
			}
		}
		return true
	}
	if has("memtotal", "memfree") {
		stats["real:used"] = stats["memtotal"] - stats["memfree"]
	}
	if has("cached", "memfree", "buffers") {
		stats["nocache:free"] = stats["cached"] + stats["memfree"] + stats["buffers"]
	}
	if has("memtotal", "nocache:free") {
		stats["nocache:used"] = stats["memtotal"] - stats["nocache:free"]
	}
	if has("swaptotal", "swapfree") {
		stats["swap:used"] = stats["swaptotal"] - stats["swapfree"]
	}
	f["memory_mb"] = map[string]any{
		"real":    map[string]any{"total": get("memtotal"), "used": get("real:used"), "free": get("memfree")},
		"nocache": map[string]any{"free": get("nocache:free"), "used": get("nocache:used")},
		"swap": map[string]any{"total": get("swaptotal"), "free": get("swapfree"),
			"used": get("swap:used"), "cached": get("swapcached")},
	}
	return f
}

var formFactors = []string{"Unknown", "Other", "Unknown", "Desktop",
	"Low Profile Desktop", "Pizza Box", "Mini Tower", "Tower",
	"Portable", "Laptop", "Notebook", "Hand Held", "Docking Station",
	"All In One", "Sub Notebook", "Space-saving", "Lunch Box",
	"Main Server Chassis", "Expansion Chassis", "Sub Chassis",
	"Bus Expansion Chassis", "Peripheral Chassis", "RAID Chassis",
	"Rack Mount Chassis", "Sealed-case PC", "Multi-system",
	"CompactPCI", "AdvancedTCA", "Blade", "Blade Enclosure",
	"Tablet", "Convertible", "Detachable", "IoT Gateway",
	"Embedded PC", "Mini PC", "Stick PC"}

var dmiKeys = [][3]string{
	{"bios_date", "bios_date", "bios-release-date"},
	{"bios_vendor", "bios_vendor", "bios-vendor"},
	{"bios_version", "bios_version", "bios-version"},
	{"board_asset_tag", "board_asset_tag", "baseboard-asset-tag"},
	{"board_name", "board_name", "baseboard-product-name"},
	{"board_serial", "board_serial", "baseboard-serial-number"},
	{"board_vendor", "board_vendor", "baseboard-manufacturer"},
	{"board_version", "board_version", "baseboard-version"},
	{"chassis_asset_tag", "chassis_asset_tag", "chassis-asset-tag"},
	{"chassis_serial", "chassis_serial", "chassis-serial-number"},
	{"chassis_vendor", "chassis_vendor", "chassis-manufacturer"},
	{"chassis_version", "chassis_version", "chassis-version"},
	{"form_factor", "chassis_type", "chassis-type"},
	{"product_name", "product_name", "system-product-name"},
	{"product_serial", "product_serial", "system-serial-number"},
	{"product_uuid", "product_uuid", "system-uuid"},
	{"product_version", "product_version", "system-version"},
	{"system_vendor", "sys_vendor", "system-manufacturer"},
}

func linuxDMIFacts(e *factEnv) map[string]any {
	f := map[string]any{}
	if e.exists("/sys/devices/virtual/dmi/id/product_name") {
		for _, k := range dmiKeys {
			data, ok := e.fileContent("/sys/devices/virtual/dmi/id/" + k[1])
			if !ok {
				f[k[0]] = "NA"
				continue
			}
			if k[0] == "form_factor" {
				n, err := strconv.Atoi(data)
				if err == nil && n >= 0 && n < len(formFactors) {
					f[k[0]] = formFactors[n]
				} else {
					f[k[0]] = "unknown (" + data + ")"
				}
				continue
			}
			f[k[0]] = data
		}
		return f
	}
	dmi := e.binPath("dmidecode")
	for _, k := range dmiKeys {
		if dmi == "" {
			f[k[0]] = "NA"
			continue
		}
		rc, out, _ := e.run(dmi, "-s", k[2])
		if rc != 0 {
			f[k[0]] = "NA"
			continue
		}
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if !strings.HasPrefix(line, "#") {
				b.WriteString(line)
			}
		}
		f[k[0]] = b.String()
	}
	return f
}

// bytesToHuman is ansible's bytes_to_human (module_utils/common/text/formatters.py).
func bytesToHuman(size float64) string {
	ranges := []struct {
		suffix string
		limit  float64
	}{
		{"Y", 1 << 80}, {"Z", 1 << 70}, {"E", 1 << 60}, {"P", 1 << 50},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	suffix, limit := "", 1.0
	for _, r := range ranges {
		suffix, limit = r.suffix, r.limit
		if size >= r.limit {
			break
		}
	}
	if limit != 1 {
		suffix += "B"
	} else {
		suffix = "Bytes"
	}
	return fmt.Sprintf("%.2f %s", size/limit, suffix)
}

func (e *factEnv) deviceLinks(dir string) map[string]any {
	out := map[string]any{}
	if !e.exists(dir) {
		return out
	}
	names, err := e.listDir(dir)
	if err != nil {
		return out
	}
	sets := map[string][]string{}
	for _, n := range names {
		target, err := e.readlink(dir + "/" + n)
		if err != nil {
			continue
		}
		base := target[strings.LastIndex(target, "/")+1:]
		sets[base] = append(sets[base], n)
	}
	for k, v := range sets {
		sort.Strings(v)
		out[k] = stringsToAny(dedupSorted(v))
	}
	return out
}

func dedupSorted(v []string) []string {
	out := v[:0]
	for i, s := range v {
		if i == 0 || s != v[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func stringsToAny(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func (e *factEnv) deviceOwners() map[string]any {
	sets := map[string][]string{}
	for _, blk := range e.glob("/sys/block", "*") {
		for _, sl := range e.glob(blk+"/slaves", "*") {
			parts := strings.Split(sl, "/")
			if len(parts) < 6 {
				continue
			}
			sets[parts[5]] = append(sets[parts[5]], parts[3])
		}
	}
	out := map[string]any{}
	for k, v := range sets {
		sort.Strings(v)
		out[k] = stringsToAny(dedupSorted(v))
	}
	return out
}

func (e *factEnv) holders(d map[string]any, sysdir string) {
	holders := []any{}
	if e.isDir(sysdir + "/holders") {
		names, _ := e.listDir(sysdir + "/holders")
		for _, folder := range names {
			if !strings.HasPrefix(folder, "dm-") {
				continue
			}
			if name, ok := e.fileContent(sysdir + "/holders/" + folder + "/dm/name"); ok {
				holders = append(holders, name)
			} else {
				holders = append(holders, folder)
			}
		}
	}
	d["holders"] = holders
}

func (e *factEnv) partitionUUID(part string) any {
	uuids, err := e.listDir("/dev/disk/by-uuid")
	if err != nil {
		return nil
	}
	for _, u := range uuids {
		if e.realpath("/dev/disk/by-uuid/"+u) == "/dev/"+part {
			return u
		}
	}
	return nil
}

func linuxDeviceFacts(e *factEnv) map[string]any {
	f := map[string]any{}
	devices := map[string]any{}
	f["devices"] = devices
	pcidata := ""
	if lspci := e.binPath("lspci"); lspci != "" {
		_, pcidata, _ = e.run(lspci, "-D")
	}
	blocks, err := e.listDir("/sys/block")
	if err != nil {
		return f
	}
	wwns := map[string]string{}
	if ids, err := e.listDir("/dev/disk/by-id"); err == nil {
		for _, n := range ids {
			if !strings.HasPrefix(n, "wwn-") {
				continue
			}
			t, err := e.readlink("/dev/disk/by-id/" + n)
			if err != nil {
				continue
			}
			wwns[t[strings.LastIndex(t, "/")+1:]] = n[4:]
		}
	}
	links := map[string]map[string]any{
		"ids":     e.deviceLinks("/dev/disk/by-id"),
		"uuids":   e.deviceLinks("/dev/disk/by-uuid"),
		"labels":  e.deviceLinks("/dev/disk/by-label"),
		"masters": e.deviceOwners(),
	}
	linksAny := map[string]any{}
	for k, v := range links {
		linksAny[k] = v
	}
	f["device_links"] = linksAny
	linksFor := func(name string) map[string]any {
		m := map[string]any{}
		for t, vals := range links {
			if v, ok := vals[name]; ok {
				m[t] = v
			} else {
				m[t] = []any{}
			}
		}
		return m
	}
	sgInq := e.binPath("sg_inq")
	schedRe := regexp.MustCompile(`^.*?(\[(.*)\])`)
	pciRe := regexp.MustCompile(`^.+/([a-f0-9]{4}:[a-f0-9]{2}:[0|1][a-f0-9]\.[0-7])/`)
	for _, block := range blocks {
		virtual := 1
		noLinks := false
		target, err := e.readlink("/sys/block/" + block)
		if err != nil {
			if !e.exists("/sys/block/" + block) {
				continue
			}
			target = block
			noLinks = true
		}
		sysdir := "/sys/block/" + target
		if strings.HasPrefix(target, "/") {
			sysdir = target
		}
		sysdir = cleanAbs(sysdir)
		if noLinks {
			names, _ := e.listDir(sysdir)
			for _, n := range names {
				if strings.Contains(n, "device") {
					virtual = 0
					break
				}
			}
		}
		d := map[string]any{"virtual": virtual, "links": linksFor(block)}
		diskname := sysdir[strings.LastIndex(sysdir, "/")+1:]
		for _, key := range []string{"vendor", "model", "sas_address", "sas_device_handle"} {
			d[key] = e.fileContentAny(sysdir + "/device/" + key)
		}
		if sgInq != "" {
			if serial := sgInqSerial(e, sgInq, block); serial != "" {
				d["serial"] = serial
			}
		} else if serial, ok := e.fileContent("/sys/block/" + block + "/device/serial"); ok {
			d["serial"] = serial
		}
		d["removable"] = e.fileContentAny(sysdir + "/removable")
		d["support_discard"] = e.fileContentAny(sysdir + "/queue/discard_granularity")
		if w, ok := wwns[diskname]; ok {
			d["wwn"] = w
		}
		partitions := map[string]any{}
		partRe := regexp.MustCompile("(" + regexp.QuoteMeta(diskname) + `[p]?\d+)`)
		names, _ := e.listDir(sysdir)
		for _, folder := range names {
			m := partRe.FindStringSubmatch(folder)
			if m == nil {
				continue
			}
			partname := m[1]
			psys := sysdir + "/" + partname
			part := map[string]any{"links": linksFor(partname)}
			start := any(0)
			if v, ok := e.fileContent(psys + "/start"); ok {
				start = v
			}
			part["start"] = start
			sectors := any(0)
			if v, ok := e.fileContent(psys + "/size"); ok {
				sectors = v
			}
			part["sectors"] = sectors
			var ss any = e.fileContentAny(psys + "/queue/logical_block_size")
			if ss == nil {
				ss = e.fileContentOrAny(psys+"/queue/hw_sector_size", 512)
			}
			part["sectorsize"] = ss
			part["size"] = bytesToHuman(anyToFloat(sectors) * 512.0)
			part["uuid"] = e.partitionUUID(partname)
			e.holders(part, psys)
			partitions[partname] = part
		}
		d["partitions"] = partitions
		d["rotational"] = e.fileContentAny(sysdir + "/queue/rotational")
		d["scheduler_mode"] = ""
		if sched, ok := e.fileContent(sysdir + "/queue/scheduler"); ok {
			if m := schedRe.FindStringSubmatch(sched); m != nil {
				d["scheduler_mode"] = m[2]
			}
		}
		sectors := any(0)
		if v, ok := e.fileContent(sysdir + "/size"); ok {
			sectors = v
		}
		d["sectors"] = sectors
		var ss any = e.fileContentAny(sysdir + "/queue/logical_block_size")
		if ss == nil {
			ss = e.fileContentOrAny(sysdir+"/queue/hw_sector_size", 512)
		}
		d["sectorsize"] = ss
		d["size"] = bytesToHuman(anyToFloat(sectors) * 512.0)
		d["host"] = ""
		if m := pciRe.FindStringSubmatch(sysdir); m != nil && pcidata != "" {
			hostRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(m[1]) + `\s(.*)$`)
			if hm := hostRe.FindStringSubmatch(pcidata); hm != nil {
				d["host"] = hm[1]
			}
		}
		e.holders(d, sysdir)
		devices[diskname] = d
	}
	return f
}

func (e *factEnv) fileContentOrAny(abs string, def any) any {
	if s, ok := e.fileContent(abs); ok {
		return s
	}
	return def
}

func anyToFloat(v any) float64 {
	switch t := v.(type) {
	case int:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// cleanAbs resolves ".." in a sysfs link target like
// "/sys/block/../devices/virtual/block/loop0".
func cleanAbs(p string) string {
	parts := strings.Split(p, "/")
	var out []string
	for _, s := range parts {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return "/" + strings.Join(out, "/")
}

func sgInqSerial(e *factEnv, sgInq, block string) string {
	rc, out, _ := e.run(sgInq, "/dev/"+block)
	if rc != 0 {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Unit serial number") {
			_, v, _ := strings.Cut(line, ":")
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func linuxUptimeFacts(e *factEnv) map[string]any {
	data, ok := e.fileContent("/proc/uptime")
	if !ok {
		return map[string]any{}
	}
	s, _, _ := strings.Cut(data, " ")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return map[string]any{}
	}
	return map[string]any{"uptime_seconds": int64(v)}
}

func linuxLVMFacts(e *factEnv) map[string]any {
	uid, _, _, _ := e.ids()
	vgsPath := e.binPath("vgs")
	if uid != 0 || vgsPath == "" {
		return map[string]any{"lvm": "N/A"}
	}
	opts := []string{"--noheadings", "--nosuffix", "--units", "g", "--separator", ","}
	lines := func(bin string) []string {
		_, out, _ := e.run(append([]string{bin}, opts...)...)
		return strings.Split(strings.TrimRight(out, "\n"), "\n")
	}
	vgs := map[string]any{}
	for _, l := range lines(vgsPath) {
		items := strings.Split(strings.TrimSpace(l), ",")
		if len(items) < 3 || l == "" {
			continue
		}
		vgs[items[0]] = map[string]any{"size_g": items[len(items)-2], "free_g": items[len(items)-1],
			"num_lvs": items[2], "num_pvs": items[1]}
	}
	lvs := map[string]any{}
	if p := e.binPath("lvs"); p != "" {
		for _, l := range lines(p) {
			items := strings.Split(strings.TrimSpace(l), ",")
			if len(items) < 4 {
				continue
			}
			lvs[items[0]] = map[string]any{"size_g": items[3], "vg": items[1]}
		}
	}
	pvs := map[string]any{}
	if p := e.binPath("pvs"); p != "" {
		for _, l := range lines(p) {
			items := strings.Split(strings.TrimSpace(l), ",")
			if len(items) < 6 {
				continue
			}
			pvs[findMapperDevice(e, items[0])] = map[string]any{"size_g": items[4], "free_g": items[5], "vg": items[1]}
		}
	}
	return map[string]any{"lvm": map[string]any{"lvs": lvs, "vgs": vgs, "pvs": pvs}}
}

func findMapperDevice(e *factEnv, dev string) string {
	if !strings.HasPrefix(dev, "/dev/dm-") {
		return dev
	}
	dmsetup := e.binPath("dmsetup")
	if dmsetup == "" {
		return dev
	}
	rc, out, _ := e.run(dmsetup, "info", "-C", "--noheadings", "-o", "name", dev)
	if rc == 0 {
		return "/dev/mapper/" + strings.TrimRight(out, " \t\r\n")
	}
	return dev
}

var octalEscape = regexp.MustCompile(`\\[0-7]{3}`)

func replaceOctalEscapes(s string) string {
	return octalEscape.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseUint(m[1:], 8, 8)
		return string(rune(n))
	})
}

// bindMounts mirrors _find_bind_mounts: findmnt shows SOURCE[/root] for
// mounts whose root inside the filesystem is not "/", which ansible treats
// as a bind mount. We read the same data from /proc/self/mountinfo.
func (e *factEnv) bindMounts() map[string]bool {
	out := map[string]bool{}
	if e.binPath("findmnt") == "" {
		return out
	}
	for _, line := range e.fileLines("/proc/self/mountinfo") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		root, target := replaceOctalEscapes(fields[3]), replaceOctalEscapes(fields[4])
		sep := 6
		for sep < len(fields) && fields[sep] != "-" {
			sep++
		}
		if sep+2 >= len(fields) {
			continue
		}
		source := fields[sep+2]
		if root != "/" && source != "" {
			out[target] = true
		}
	}
	return out
}

func (e *factEnv) lsblkUUIDs() map[string]string {
	uuids := map[string]string{}
	lsblk := e.binPath("lsblk")
	if lsblk == "" {
		return uuids
	}
	rc, out, _ := e.run(lsblk, "--list", "--noheadings", "--paths", "--output", "NAME,UUID", "--exclude", "2")
	if rc != 0 {
		return uuids
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := strings.TrimSpace(line[:strings.LastIndex(line, fields[len(fields)-1])])
		uuid := fields[len(fields)-1]
		if _, seen := uuids[dev]; seen {
			continue
		}
		uuids[dev] = uuid
	}
	return uuids
}

func (e *factEnv) udevadmUUID(device string) string {
	udevadm := e.binPath("udevadm")
	if udevadm == "" {
		return ""
	}
	rc, out, _ := e.run(udevadm, "info", "--query", "property", "--name", device)
	if rc != 0 {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "ID_FS_UUID="); ok {
			return v
		}
	}
	return ""
}

func linuxMountFacts(e *factEnv) map[string]any {
	binds := e.bindMounts()
	uuids := e.lsblkUUIDs()
	mtab := "/etc/mtab"
	if !e.exists(mtab) {
		mtab = "/proc/mounts"
	}
	data, _ := e.fileContent(mtab)
	type pending struct {
		info          map[string]any
		mount, device string
	}
	var order []string
	byMount := map[string]*pending{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		for i := range fields {
			fields[i] = replaceOctalEscapes(fields[i])
		}
		device, mount, fstype, options := fields[0], fields[1], fields[2], fields[3]
		if (!strings.HasPrefix(device, "/") && !strings.HasPrefix(device, "\\") && !strings.Contains(device, ":/")) || fstype == "none" {
			continue
		}
		info := map[string]any{"mount": mount, "device": device, "fstype": fstype, "options": options}
		if binds[mount] && !strings.Contains(options, "bind") {
			info["options"] = options + ",bind"
		}
		if _, seen := byMount[mount]; !seen {
			order = append(order, mount)
		}
		byMount[mount] = &pending{info: info, mount: mount, device: device}
	}

	// statvfs each mount concurrently, bounded by gather_timeout.
	type result struct {
		size map[string]any
		uuid string
	}
	results := make([]chan result, len(order))
	for i, m := range order {
		ch := make(chan result, 1)
		results[i] = ch
		p := byMount[m]
		go func() {
			var r result
			if e.statvfs != nil {
				if sz, ok := e.statvfs(e.p(p.mount)); ok {
					r.size = sz
				}
			}
			if u, ok := uuids[p.device]; ok {
				r.uuid = u
			} else {
				r.uuid = e.udevadmUUID(p.device)
			}
			ch <- r
		}()
	}
	deadline := time.NewTimer(e.cmdTimeout())
	defer deadline.Stop()
	expired := false
	mounts := []any{}
	for i, m := range order {
		info := byMount[m].info
		var r result
		got := false
		if !expired {
			select {
			case r = <-results[i]:
				got = true
			case <-deadline.C:
				expired = true
			}
		}
		if !got && expired {
			select {
			case r = <-results[i]:
				got = true
			default:
			}
		}
		if got {
			for k, v := range r.size {
				info[k] = v
			}
			if r.uuid != "" {
				info["uuid"] = r.uuid
			} else {
				info["uuid"] = "N/A"
			}
		} else {
			e.warn("Timeout exceeded when getting mount info for " + m)
			info["note"] = "Could not get extra information due to timeout"
		}
		mounts = append(mounts, info)
	}
	return map[string]any{"mounts": mounts}
}
