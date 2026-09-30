package modules

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(partedModule, "parted", "community.general.parted")
	Register(filesystemModule, "filesystem", "community.general.filesystem")
}

var langC = map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}

// runCheckRC is run_command(check_rc=True): a non-zero exit fails the
// module with cmd, rc, stdout, stderr and the stripped stderr as msg.
func runCheckRC(env *RunEnv, argv []string, envUpdate map[string]string) (string, string, *agentproto.Result) {
	rc, out, errOut := runCommand(env, argv, cmdOpts{Env: envUpdate})
	if rc != 0 {
		quoted := make([]string, len(argv))
		for i, a := range argv {
			quoted[i] = shlexQuote(a)
		}
		r := agentproto.Fail("%s", strings.TrimRight(errOut, " \t\r\n\f\v"))
		r.Extra = map[string]any{"cmd": strings.Join(quoted, " "), "rc": int64(rc), "stdout": out, "stderr": errOut}
		return out, errOut, r
	}
	return out, errOut, nil
}

// --- parted (community.general.parted) ---

var (
	partedUnitsSI  = []string{"B", "KB", "MB", "GB", "TB"}
	partedUnitsIEC = []string{"KiB", "MiB", "GiB", "TiB"}
	partedUnits    = append(append(append([]string{}, partedUnitsSI...), partedUnitsIEC...), "s", "%", "cyl", "chs", "compact")
)

var partedSpec = args.Spec{
	"device":             {Required: true},
	"align":              {Default: "optimal", Choices: []string{"cylinder", "minimal", "none", "optimal", "undefined"}},
	"number":             {Type: "int"},
	"unit":               {Default: "KiB", Choices: partedUnits},
	"label":              {Default: "msdos", Choices: []string{"aix", "amiga", "bsd", "dvh", "gpt", "loop", "mac", "msdos", "pc98", "sun"}},
	"part_type":          {Default: "primary", Choices: []string{"extended", "logical", "primary"}},
	"part_start":         {Default: "0%"},
	"part_end":           {Default: "100%"},
	"fs_type":            {},
	"name":               {},
	"flags":              {Type: "list"},
	"state":              {Default: "info", Choices: []string{"absent", "info", "present"}},
	"resize":             {Type: "bool", Default: false},
	"unit_preserve_case": {Type: "bool", Default: false},
}

func canonicalUnit(unit string) string {
	for _, u := range partedUnits {
		if strings.EqualFold(u, unit) {
			return u
		}
	}
	return unit
}

var (
	partedSizeRe = regexp.MustCompile(`^(-?[\d.]+) *([\w%]+)?$`)
	partedCHSRe  = regexp.MustCompile(`^(\d+),(\d+),(\d+)$`)
)

// parseUnit is parse_unit(): the size (float64, or a CHS map) and unit.
func parseUnit(s, unit string) (any, string, bool) {
	if m := partedSizeRe.FindStringSubmatch(s); m != nil {
		if m[2] != "" {
			unit = m[2]
		}
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return nil, unit, false
		}
		return f, unit, true
	}
	if m := partedCHSRe.FindStringSubmatch(s); m != nil {
		c, _ := strconv.ParseInt(m[1], 10, 64)
		h, _ := strconv.ParseInt(m[2], 10, 64)
		sec, _ := strconv.ParseInt(m[3], 10, 64)
		return map[string]any{"cylinder": c, "head": h, "sector": sec}, "chs", true
	}
	return nil, unit, false
}

type parted struct {
	env      *RunEnv
	bin      string
	unit     string
	preserve bool
	version  [3]int
}

func (pt *parted) unitOut(u string) string {
	if pt.preserve {
		return canonicalUnit(u)
	}
	return strings.ToLower(u)
}

func (pt *parted) parseErr(s string) *agentproto.Result {
	return agentproto.Fail("Error interpreting parted size output: '%s'", s)
}

// parsePartitionInfo is parse_partition_info().
func (pt *parted) parsePartitionInfo(out string) (map[string]any, []any, *agentproto.Result) {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return nil, nil, agentproto.Fail("Error while parsing parted output: %s", out)
	}
	gp := strings.Split(strings.TrimRight(lines[1], ";"), ":")
	for len(gp) < 7 {
		gp = append(gp, "")
	}
	size, unit, ok := parseUnit(gp[1], pt.unit)
	if !ok {
		return nil, nil, pt.parseErr(gp[1])
	}
	lb, _ := strconv.ParseInt(gp[3], 10, 64)
	pb, _ := strconv.ParseInt(gp[4], 10, 64)
	generic := map[string]any{"dev": gp[0], "size": size, "unit": pt.unitOut(unit), "table": gp[5],
		"model": gp[6], "logical_block": lb, "physical_block": pb}
	if unit == "cyl" || unit == "chs" {
		chs := strings.Split(strings.TrimRight(lines[2], ";"), ":")
		for len(chs) < 4 {
			chs = append(chs, "")
		}
		cylSize, cylUnit, ok := parseUnit(chs[3], "")
		if !ok {
			return nil, nil, pt.parseErr(chs[3])
		}
		c, _ := strconv.ParseInt(chs[0], 10, 64)
		h, _ := strconv.ParseInt(chs[1], 10, 64)
		s, _ := strconv.ParseInt(chs[2], 10, 64)
		generic["chs_info"] = map[string]any{"cylinders": c, "heads": h, "sectors": s, "cyl_size": cylSize, "cyl_size_unit": pt.unitOut(cylUnit)}
		lines = lines[1:]
	}
	parts := []any{}
	for _, line := range lines[2:] {
		pp := strings.Split(strings.TrimRight(line, ";"), ":")
		for len(pp) < 7 {
			pp = append(pp, "")
		}
		var size any = ""
		fstype, name, flags := pp[3], pp[4], pp[5]
		if unit != "chs" {
			s, _, ok := parseUnit(pp[3], "")
			if !ok {
				return nil, nil, pt.parseErr(pp[3])
			}
			size, fstype, name, flags = s, pp[4], pp[5], pp[6]
		}
		begin, _, ok1 := parseUnit(pp[1], "")
		end, _, ok2 := parseUnit(pp[2], "")
		if !ok1 {
			return nil, nil, pt.parseErr(pp[1])
		}
		if !ok2 {
			return nil, nil, pt.parseErr(pp[2])
		}
		num, _ := strconv.ParseInt(pp[0], 10, 64)
		fl := []any{}
		for _, f := range strings.Split(flags, ", ") {
			if f != "" {
				fl = append(fl, strings.TrimSpace(f))
			}
		}
		parts = append(parts, map[string]any{"num": num, "begin": begin, "end": end, "size": size,
			"fstype": fstype, "name": name, "flags": fl, "unit": pt.unitOut(unit)})
	}
	return generic, parts, nil
}

// formatDiskSize is format_disk_size().
func formatDiskSize(sizeBytes int64, unit string) (float64, string) {
	unit = strings.ToLower(unit)
	if sizeBytes == 0 {
		return 0, "b"
	}
	if unit == "" || unit == "compact" || unit == "cyl" || unit == "chs" {
		index := int(math.Max(0, math.Trunc((math.Log10(float64(sizeBytes))-1.0)/3.0)))
		unit = "b"
		if index < len(partedUnitsSI) {
			unit = partedUnitsSI[index]
		}
	}
	multiplier := 1.0
	for i, u := range partedUnitsSI {
		if u == unit {
			multiplier = math.Pow(1000, float64(i))
		}
	}
	for i, u := range partedUnitsIEC {
		if u == unit {
			multiplier = math.Pow(1024, float64(i))
		}
	}
	output := math.Floor(float64(sizeBytes)/multiplier) * (1 + 1e-16)
	var w float64
	switch {
	case output < 10:
		w = output + 0.005
	case output < 100:
		w = output + 0.05
	default:
		w = output + 0.5
	}
	precision := 0
	if w < 10 {
		precision = 2
	} else if w < 100 {
		precision = 1
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(output, 'f', precision, 64), 64)
	return r, unit
}

// convertToBytes is convert_to_bytes().
func convertToBytes(size float64, unit string) int64 {
	multiplier := 1.0
	switch {
	case indexOf(partedUnitsSI, unit) >= 0:
		multiplier = math.Pow(1000, float64(indexOf(partedUnitsSI, unit)))
	case indexOf(partedUnitsIEC, unit) >= 0:
		multiplier = math.Pow(1024, float64(indexOf(partedUnitsIEC, unit)+1))
	case unit == "" || unit == "compact" || unit == "cyl" || unit == "chs":
		multiplier = math.Pow(1000, 2)
	}
	return int64(size * multiplier)
}

func indexOf(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}

func readRecord(path, def string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return def
	}
	return strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
}

func (pt *parted) getVersion() *agentproto.Result {
	rc, out, errOut := runCommand(pt.env, []string{pt.bin, "--version"}, cmdOpts{Env: langC})
	fail := func(rcv int, withErr bool) *agentproto.Result {
		r := agentproto.Fail("Failed to get parted version.")
		r.Extra = map[string]any{"rc": int64(rcv), "out": out}
		if withErr {
			r.Extra["err"] = errOut
		}
		return r
	}
	if rc != 0 {
		return fail(rc, true)
	}
	var first string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			first = strings.TrimSpace(l)
			break
		}
	}
	m := regexp.MustCompile(`^parted.+\s(\d+)\.(\d+)(?:\.(\d+))?`).FindStringSubmatch(first)
	if m == nil {
		return fail(0, false)
	}
	pt.version[0], _ = strconv.Atoi(m[1])
	pt.version[1], _ = strconv.Atoi(m[2])
	if m[3] != "" {
		pt.version[2], _ = strconv.Atoi(m[3])
	}
	return nil
}

// deviceInfo is get_device_info().
func (pt *parted) deviceInfo(device string) (map[string]any, []any, *agentproto.Result) {
	if f := pt.getVersion(); f != nil {
		return nil, nil, f
	}
	if !(pt.version[0] == 3 && pt.version[1] >= 1 || pt.version[0] > 3) {
		rc, out, _ := runCommand(pt.env, []string{pt.bin, "-s", "-m", device, "print"}, cmdOpts{Env: langC})
		if rc != 0 && strings.Contains(strings.ToLower(out), "unrecognised disk label") {
			// get_unlabeled_device_info
			base := "/sys/block/" + filepath.Base(device)
			lb, _ := strconv.ParseInt(readRecord(base+"/queue/logical_block_size", "0"), 10, 64)
			pb, _ := strconv.ParseInt(readRecord(base+"/queue/physical_block_size", "0"), 10, 64)
			sz, _ := strconv.ParseInt(readRecord(base+"/size", "0"), 10, 64)
			size, unit := formatDiskSize(sz*lb, pt.unit)
			if pt.preserve {
				unit = canonicalUnit(unit)
			}
			return map[string]any{"dev": device, "table": "unknown", "size": size, "unit": unit,
				"logical_block": lb, "physical_block": pb,
				"model": readRecord(base+"/device/vendor", "Unknown") + " " + readRecord(base+"/device/model", "model")}, []any{}, nil
		}
	}
	argv := []string{pt.bin, "-s", "-m", device, "--", "unit", pt.unit, "print"}
	rc, out, errOut := runCommand(pt.env, argv, cmdOpts{Env: langC})
	if rc != 0 && !strings.Contains(errOut, "unrecognised disk label") {
		r := agentproto.Fail("Error while getting device information with parted script: '%s'", strings.Join(argv, " "))
		r.Extra = map[string]any{"rc": int64(rc), "out": out, "err": errOut}
		return nil, nil, r
	}
	return pt.parsePartitionInfo(out)
}

// run is parted(): run a script unless in check mode.
func (pt *parted) run(script []string, device, align string) *agentproto.Result {
	if f := pt.getVersion(); f != nil {
		return f
	}
	alignOpt := []string{"-a", align}
	if align == "undefined" {
		alignOpt = nil
	}
	scriptOpt := []string{"-s"}
	v := pt.version
	if v[0] > 3 || v[0] == 3 && (v[1] > 4 || v[1] == 4 && v[2] >= 64) {
		scriptOpt = []string{"-s", "-f"}
	}
	if len(script) == 0 || pt.env.CheckMode {
		return nil
	}
	argv := append(append(append(append([]string{pt.bin}, scriptOpt...), "-m"), alignOpt...), device, "--")
	argv = append(argv, script...)
	rc, out, errOut := runCommand(pt.env, argv, cmdOpts{Env: langC})
	if rc != 0 {
		r := agentproto.Fail("Error while running parted script: %s", strings.TrimSpace(strings.Join(argv, " ")))
		r.Extra = map[string]any{"rc": int64(rc), "out": out, "err": errOut}
		return r
	}
	return nil
}

func partByNum(parts []any, num int64) map[string]any {
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok && m["num"] == num {
			return m
		}
	}
	return nil
}

func checkSizeFormat(s string) bool {
	_, unit, ok := parseUnit(s, "")
	return ok && indexOf(partedUnits, unit) >= 0
}

// partedModule ports community.general.parted.
func partedModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := partedSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	state := p.Str("state")
	if (state == "present" || state == "absent") && !p.Has("number") {
		return agentproto.Fail("state is %s but all of the following are missing: number", state)
	}
	bin, err := getBinPath("parted")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	pt := &parted{env: env, bin: bin, unit: p.Str("unit"), preserve: p.Bool("unit_preserve_case")}
	device, align := p.Str("device"), p.Str("align")
	number := p.Int("number")
	if p.Has("number") && number < 1 {
		return agentproto.Fail("The partition number must be greater then 0.")
	}
	for _, k := range []string{"part_start", "part_end"} {
		if !checkSizeFormat(p.Str(k)) {
			size, unit, _ := parseUnit(p.Str(k), "")
			r := agentproto.Fail("The argument '%s' doesn't respect required format.The size unit is case sensitive.", k)
			r.Extra = map[string]any{"err": []any{size, unit}}
			return r
		}
	}
	generic, parts, fail := pt.deviceInfo(device)
	if fail != nil {
		return fail
	}
	unit := pt.unit
	changed := false
	outputScript := []any{}
	addOut := func(s []string) {
		for _, x := range s {
			outputScript = append(outputScript, x)
		}
	}
	var script []string
	switch state {
	case "present":
		label := p.Str("label")
		mklabel := generic["table"] != label
		if mklabel {
			script = append(script, "mklabel", label)
		}
		if p.Str("part_type") != "" && (mklabel || partByNum(parts, number) == nil) {
			script = append(script, "mkpart", p.Str("part_type"))
			if p.Has("fs_type") {
				script = append(script, p.Str("fs_type"))
			}
			script = append(script, p.Str("part_start"), p.Str("part_end"))
		}
		if unit != "" && len(script) > 0 {
			script = append([]string{"unit", unit}, script...)
		}
		if part := partByNum(parts, number); p.Bool("resize") && part != nil {
			endF, _ := part["end"].(float64)
			current := convertToBytes(endF, unit)
			size, parsedUnit, _ := parseUnit(p.Str("part_end"), unit)
			sf, _ := size.(float64)
			if parsedUnit == "%" {
				ds, _ := generic["size"].(float64)
				sf = float64(int64(float64(int64(ds)) * sf / 100))
				parsedUnit = unit
			}
			if current != convertToBytes(sf, parsedUnit) {
				script = append(script, "resizepart", strconv.FormatInt(number, 10), p.Str("part_end"))
			}
		}
		if len(script) > 0 {
			addOut(script)
			if f := pt.run(script, device, align); f != nil {
				return f
			}
			changed = true
			script = nil
			if !env.CheckMode {
				if _, parts, fail = pt.deviceInfo(device); fail != nil {
					return fail
				}
			}
		}
		if partByNum(parts, number) != nil || env.CheckMode {
			var partition map[string]any
			if changed && env.CheckMode {
				partition = map[string]any{"flags": []any{}}
			} else {
				partition = partByNum(parts, number)
			}
			if partition == nil {
				return agentproto.Fail("list index out of range")
			}
			num := strconv.FormatInt(number, 10)
			if p.Has("name") && partition["name"] != p.Str("name") {
				script = append(script, "name", num, `"`+p.Str("name")+`"`)
			}
			var flags []string
			for _, f := range p.List("flags") {
				flags = append(flags, pyStrValue(f))
			}
			if len(flags) > 0 {
				if containsStr(flags, "esp") && !containsStr(flags, "boot") {
					flags = append(flags, "boot")
				}
				typeRe := regexp.MustCompile(`^type=[0-9a-fA-F]+$`)
				var current []string
				cf, _ := partition["flags"].([]any)
				for _, f := range cf {
					if s, _ := f.(string); !typeRe.MatchString(s) {
						current = append(current, s)
					}
				}
				for _, f := range setMinus(flags, current) {
					script = append(script, "set", num, f, "on")
				}
				for _, f := range setMinus(current, flags) {
					script = append(script, "set", num, f, "off")
				}
			}
		}
		if unit != "" && len(script) > 0 {
			script = append([]string{"unit", unit}, script...)
		}
		if len(script) > 0 {
			addOut(script)
			changed = true
			if f := pt.run(script, device, align); f != nil {
				return f
			}
		}
	case "absent":
		if partByNum(parts, number) != nil || env.CheckMode {
			script = []string{"rm", strconv.FormatInt(number, 10)}
			addOut(script)
			changed = true
			if f := pt.run(script, device, align); f != nil {
				return f
			}
		}
	case "info":
		addOut([]string{"unit", unit, "print"})
	}
	generic, parts, fail = pt.deviceInfo(device)
	if fail != nil {
		return fail
	}
	return &agentproto.Result{Changed: changed, Extra: map[string]any{
		"disk": generic, "partitions": parts, "script": outputScript}}
}

// --- filesystem (community.general.filesystem) ---

var filesystemTypes = []string{"bcachefs", "ext2", "ext3", "ext4", "ext4dev", "f2fs", "reiserfs", "xfs",
	"btrfs", "vfat", "ocfs2", "swap", "ufs", "gfs2", "lvm"}

var filesystemSpec = args.Spec{
	"state":    {Default: "present", Choices: []string{"present", "absent"}},
	"fstype":   {Aliases: []string{"type"}, Choices: filesystemTypes},
	"dev":      {Required: true, Aliases: []string{"device"}},
	"opts":     {},
	"force":    {Type: "bool", Default: false},
	"resizefs": {Type: "bool", Default: false},
	"uuid":     {},
	"label":    {},
}

// fsClass is one Filesystem subclass's settings.
type fsClass struct {
	name              string // class name (fstype in messages)
	mkfs              string
	forceFlags        []string
	setUUIDOpts       []string
	setUUIDExtra      []string
	setLabelOpts      []string
	info, grow        string
	growSlack         int64
	growFlags         []string
	growMountOnly     bool
	changeUUID        string
	changeUUIDOpt     string
	changeUUIDHasArg  bool
	size              func(fs *fsRun, dev string) (int64, error)
	versionForceCheck func(fs *fsRun, c *fsClass)
}

func filesystemClass(fstype string) *fsClass {
	ext := func(name, mkfs string) *fsClass {
		return &fsClass{name: name, mkfs: mkfs, forceFlags: []string{"-F"}, setUUIDOpts: []string{"-U"}, info: "tune2fs",
			grow: "resize2fs", changeUUID: "tune2fs", changeUUIDOpt: "-U", changeUUIDHasArg: true, size: extSize}
	}
	switch fstype {
	case "ext2":
		return ext("Ext2", "mkfs.ext2")
	case "ext3":
		return ext("Ext3", "mkfs.ext3")
	case "ext4", "ext4dev":
		return ext("Ext4", "mkfs.ext4")
	case "xfs":
		return &fsClass{name: "XFS", mkfs: "mkfs.xfs", forceFlags: []string{"-f"}, info: "xfs_info", grow: "xfs_growfs",
			growSlack: 64*4096 - 1, growMountOnly: true, changeUUID: "xfs_admin", changeUUIDOpt: "-U", changeUUIDHasArg: true, size: xfsSize}
	case "reiserfs":
		return &fsClass{name: "Reiserfs", mkfs: "mkfs.reiserfs", forceFlags: []string{"-q"}}
	case "bcachefs":
		return &fsClass{name: "Bcachefs", mkfs: "mkfs.bcachefs", forceFlags: []string{"--force"}, setUUIDOpts: []string{"-U", "--uuid"},
			info: "bcachefs", grow: "bcachefs", growFlags: []string{"device", "resize"}, size: bcachefsSize}
	case "btrfs":
		return &fsClass{name: "Btrfs", mkfs: "mkfs.btrfs", info: "btrfs", grow: "btrfs", growFlags: []string{"filesystem", "resize", "max"},
			growMountOnly: true, size: btrfsSize, versionForceCheck: btrfsForce}
	case "ocfs2":
		return &fsClass{name: "Ocfs2", mkfs: "mkfs.ocfs2", forceFlags: []string{"-Fx"}}
	case "f2fs":
		return &fsClass{name: "F2fs", mkfs: "mkfs.f2fs", info: "dump.f2fs", grow: "resize.f2fs", size: f2fsSize, versionForceCheck: f2fsForce}
	case "vfat":
		return &fsClass{name: "VFAT", mkfs: "mkfs.vfat", info: "fatresize", grow: "fatresize", growFlags: []string{"-s", "max"}, size: vfatSize}
	case "LVM2_member":
		return &fsClass{name: "LVM", mkfs: "pvcreate", forceFlags: []string{"-f"}, setUUIDOpts: []string{"-u", "--uuid"},
			setUUIDExtra: []string{"--norestorefile"}, info: "pvs", grow: "pvresize", changeUUID: "pvchange", changeUUIDOpt: "-u", size: lvmSize}
	case "swap":
		return &fsClass{name: "Swap", mkfs: "mkswap", forceFlags: []string{"-f"}}
	case "ufs":
		return &fsClass{name: "UFS", mkfs: "newfs", info: "dumpfs", grow: "growfs", growFlags: []string{"-y"}, size: ufsSize}
	case "gfs2":
		return &fsClass{name: "GFS2", mkfs: "mkfs.gfs2", forceFlags: []string{"-O"}, setUUIDOpts: []string{"-U"}, setLabelOpts: []string{"-t"}}
	}
	return nil
}

type fsRun struct {
	env      *RunEnv
	warnings []any
}

func (fs *fsRun) bin(name string) (string, *agentproto.Result) {
	b, err := getBinPath(name)
	if err != nil {
		return "", agentproto.Fail("%v", err)
	}
	return b, nil
}

// fsFailErr carries a fail_json result through the size helpers.
type fsFailErr struct{ r *agentproto.Result }

func (f fsFailErr) Error() string { return f.r.Msg }

// valueErr is the ValueError a size parser raises (repr of the output).
type valueErr string

func (v valueErr) Error() string { return string(v) }

func (fs *fsRun) runRC(argv []string, envUpdate map[string]string) (string, string, error) {
	b, fail := fs.bin(argv[0])
	if fail != nil {
		return "", "", fsFailErr{fail}
	}
	argv = append([]string{b}, argv[1:]...)
	out, errOut, f := runCheckRC(fs.env, argv, envUpdate)
	if f != nil {
		return out, errOut, fsFailErr{f}
	}
	return out, errOut, nil
}

func (fs *fsRun) mountpoint(dev string) (string, error) {
	b, fail := fs.bin("findmnt")
	if fail != nil {
		return "", fsFailErr{fail}
	}
	rc, out, _ := runCommand(fs.env, []string{b, "--mtab", "--noheadings", "--output", "TARGET", "--source", dev}, cmdOpts{})
	if rc != 0 {
		return "", nil
	}
	return strings.Split(out, "\n")[0], nil
}

func extSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"tune2fs", "-l", dev}, langC)
	if err != nil {
		return 0, err
	}
	var count, size int64 = -1, -1
	for _, line := range pySplitLines(out) {
		if strings.Contains(line, "Block count:") {
			count, _ = strconv.ParseInt(strings.TrimSpace(strings.Split(line, ":")[1]), 10, 64)
		} else if strings.Contains(line, "Block size:") {
			size, _ = strconv.ParseInt(strings.TrimSpace(strings.Split(line, ":")[1]), 10, 64)
		}
		if count >= 0 && size >= 0 {
			return count * size, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

func xfsSize(fs *fsRun, dev string) (int64, error) {
	target := dev
	if mp, err := fs.mountpoint(dev); err != nil {
		return 0, err
	} else if mp != "" {
		target = mp
	}
	out, _, err := fs.runRC([]string{"xfs_info", target}, langC)
	if err != nil {
		return 0, err
	}
	var bsize, blocks int64 = -1, -1
	for _, line := range pySplitLines(out) {
		col := strings.Split(line, "=")
		if strings.TrimSpace(col[0]) == "data" && len(col) >= 4 {
			if strings.TrimSpace(col[1]) == "bsize" {
				bsize, _ = strconv.ParseInt(strings.Fields(col[2])[0], 10, 64)
			}
			if f := strings.Fields(col[2]); len(f) > 1 && f[1] == "blocks" {
				blocks, _ = strconv.ParseInt(strings.Split(col[3], ",")[0], 10, 64)
			}
		}
		if bsize >= 0 && blocks >= 0 {
			return bsize * blocks, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

var bcachefsSizeRe = regexp.MustCompile(`Size:\s+(?P<value>[\d.]+)\s*(?P<unit>\S+)`)

func bcachefsSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"bcachefs", "show-super", dev}, nil)
	if err != nil {
		return 0, err
	}
	factors := map[string]float64{"B": 1}
	for i, u := range []string{"k", "M", "G", "T", "P", "E", "Z", "Y"} {
		factors[u] = math.Pow(1024, float64(i+1))
		factors[strings.ToUpper(u[:1])+"iB"] = math.Pow(1024, float64(i+1))
		factors[u+"B"] = math.Pow(1000, float64(i+1))
	}
	factors["KiB"], factors["kB"] = 1024, 1000
	delete(factors, "kiB")
	delete(factors, "KB")
	for _, line := range pySplitLines(out) {
		if m := bcachefsSizeRe.FindStringSubmatch(line); m != nil {
			f, ok := factors[m[2]]
			if !ok {
				return 0, valueErr(pyStrRepr(out))
			}
			v, _ := strconv.ParseFloat(m[1], 64)
			return int64(v * f), nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

func btrfsSize(fs *fsRun, dev string) (int64, error) {
	mp, err := fs.mountpoint(dev)
	if err != nil {
		return 0, err
	}
	if mp == "" {
		return 0, fsFailErr{agentproto.Fail("%s needs to be mounted for Btrfs operations", dev)}
	}
	out, _, err := fs.runRC([]string{"btrfs", "filesystem", "usage", "-b", mp}, nil)
	if err != nil {
		return 0, err
	}
	for _, line := range pySplitLines(out) {
		if strings.Contains(line, "Device size") {
			f := strings.Fields(line)
			n, _ := strconv.ParseInt(f[len(f)-1], 10, 64)
			return n, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

func btrfsForce(fs *fsRun, c *fsClass) {
	out, errOut, err := fs.runRC([]string{"mkfs.btrfs", "--version"}, nil)
	if err != nil {
		return
	}
	re := regexp.MustCompile(` v([0-9.]+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		m = re.FindStringSubmatch(errOut)
	}
	if m != nil {
		if !looseVersionLess(m[1], "3.12") {
			c.forceFlags = []string{"-f"}
		}
	} else {
		c.forceFlags = []string{"-f"}
		fs.warnings = append(fs.warnings, fmt.Sprintf("Unable to identify mkfs.btrfs version (%s, %s)", pyStrRepr(out), pyStrRepr(errOut)))
	}
}

func f2fsForce(fs *fsRun, c *fsClass) {
	b, fail := fs.bin("mkfs.f2fs")
	if fail != nil {
		return
	}
	_, out, _ := runCommand(fs.env, []string{b, os.DevNull}, cmdOpts{Env: langC})
	if m := regexp.MustCompile(`F2FS-tools: mkfs.f2fs Ver: ([0-9.]+) \(`).FindStringSubmatch(out); m != nil {
		if !looseVersionLess(m[1], "1.9.0") {
			c.forceFlags = []string{"-f"}
		}
	}
}

func f2fsSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"dump.f2fs", dev}, langC)
	if err != nil {
		return 0, err
	}
	var size, count int64 = -1, -1
	for _, line := range pySplitLines(out) {
		f := strings.Fields(line)
		if strings.Contains(line, "Info: sector size = ") && len(f) > 4 {
			size, _ = strconv.ParseInt(f[4], 10, 64)
		} else if strings.Contains(line, "Info: total FS sectors = ") && len(f) > 5 {
			count, _ = strconv.ParseInt(f[5], 10, 64)
		}
		if size >= 0 && count >= 0 {
			return size * count, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

func vfatSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"fatresize", "--info", dev}, langC)
	if err != nil {
		return 0, err
	}
	lines := pySplitLines(out)
	if len(lines) > 0 {
		lines = lines[1:]
	}
	for _, line := range lines {
		param, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if p := strings.TrimSpace(param); p == "Size" || p == "Cur size" {
			n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return n, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

func lvmSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"pvs", "--noheadings", "--nosuffix", "--units", "b", "-o", "pv_size", dev}, nil)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		return 0, valueErr(pyStrRepr(out))
	}
	return n, nil
}

func ufsSize(fs *fsRun, dev string) (int64, error) {
	out, _, err := fs.runRC([]string{"dumpfs", dev}, langC)
	if err != nil {
		return 0, err
	}
	var frag, prov int64 = -1, -1
	for _, line := range pySplitLines(out) {
		f := strings.Fields(line)
		if strings.HasPrefix(line, "fsize") && len(f) > 1 {
			frag, _ = strconv.ParseInt(f[1], 10, 64)
		} else if strings.Contains(line, "providersize") {
			prov, _ = strconv.ParseInt(f[len(f)-1], 10, 64)
		}
		if frag >= 0 && prov >= 0 {
			return frag * prov, nil
		}
	}
	return 0, valueErr(pyStrRepr(out))
}

// filesystemModule ports community.general.filesystem (Linux).
func filesystemModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := filesystemSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if err := filesystemSpec.MutuallyExclusive(rawArgs, []string{"resizefs", "uuid"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	state := p.Str("state")
	if state == "present" && !p.Has("fstype") {
		return agentproto.Fail("state is present but all of the following are missing: fstype")
	}
	fs := &fsRun{env: env}
	done := func(r *agentproto.Result) *agentproto.Result {
		if len(fs.warnings) > 0 {
			if r.Extra == nil {
				r.Extra = map[string]any{}
			}
			r.Extra["warnings"] = fs.warnings
		}
		return r
	}
	fail := func(err error) *agentproto.Result {
		if f, ok := err.(fsFailErr); ok {
			return done(f.r)
		}
		return done(agentproto.Fail("%v", err))
	}
	dev := pyExpandPath(p.Str("dev"))
	fstype := p.Str("fstype")
	uuid := p.Str("uuid")
	var mkfsOpts []string
	if p.Has("opts") {
		mkfsOpts = strings.Fields(p.Str("opts"))
	}
	if _, err := os.Stat(dev); err != nil {
		msg := fmt.Sprintf("Device %s not found.", dev)
		if state == "present" {
			return agentproto.Fail("%s", msg)
		}
		return &agentproto.Result{Msg: msg}
	}
	blkid, f := fs.bin("blkid")
	if f != nil {
		return f
	}
	rc, raw, errOut := runCommand(env, []string{blkid, "-c", os.DevNull, "-o", "value", "-s", "TYPE", dev}, cmdOpts{})
	cur := strings.TrimSpace(raw)
	if rc != 0 && rc != 2 {
		fs.warnings = append(fs.warnings, fmt.Sprintf("blkid failed probing %s (rc=%d): %s", dev, rc, strings.TrimSpace(errOut)))
	}
	if cur != "" && strings.Contains(cur, " ") {
		cur = ""
		if m := regexp.MustCompile(`\bTYPE="?([^"\s]+)"?`).FindStringSubmatch(raw); m != nil {
			cur = m[1]
		}
	}

	if state == "absent" {
		if cur == "" {
			return done(&agentproto.Result{})
		}
		if !env.CheckMode {
			if _, _, err := fs.runRC([]string{"wipefs", "--all", dev}, nil); err != nil {
				return fail(err)
			}
		}
		return done(&agentproto.Result{Changed: true})
	}

	if fstype == "lvm" {
		fstype = "LVM2_member"
	}
	c := filesystemClass(fstype)
	if c == nil {
		return done(agentproto.Fail("module does not support this filesystem (%s) yet.", fstype))
	}
	if c.versionForceCheck != nil {
		c.versionForceCheck(fs, c)
	}
	if uuid != "" && c.changeUUID == "" && c.setUUIDOpts == nil {
		return done(agentproto.Fail("module does not support UUID option for this filesystem (%s) yet.", fstype))
	}
	curClass := filesystemClass(cur)
	sameFS := cur != "" && curClass != nil && curClass.name == c.name
	if sameFS && !p.Bool("resizefs") && uuid == "" && !p.Bool("force") {
		return done(&agentproto.Result{})
	}
	if sameFS && p.Bool("resizefs") {
		if c.grow == "" {
			return done(agentproto.Fail("module does not support resizing %s filesystem yet.", fstype))
		}
		out, r := fs.growFS(c, dev)
		if r != nil {
			return done(r)
		}
		return done(&agentproto.Result{Changed: true, Msg: out, Extra: map[string]any{"msg": out}})
	}
	if sameFS && uuid != "" {
		out, r := fs.changeUUID(c, uuid, dev)
		if r != nil {
			return done(r)
		}
		return done(&agentproto.Result{Changed: true, Msg: out, Extra: map[string]any{"msg": out}})
	}
	if !sameFS && cur != "" && !p.Bool("force") {
		r := agentproto.Fail("'%s' is already used as %s, use force=true to overwrite", dev, cur)
		r.Extra = map[string]any{"rc": int64(rc), "err": errOut}
		return done(r)
	}
	if !env.CheckMode {
		opts := mkfsOpts
		if uuid != "" && c.setUUIDOpts != nil && !anyIn(c.setUUIDOpts, opts) {
			opts = append(append(opts, c.setUUIDOpts[0], uuid), c.setUUIDExtra...)
		}
		if p.Has("label") && c.setLabelOpts != nil && !anyIn(c.setLabelOpts, opts) {
			opts = append(opts, c.setLabelOpts[0], p.Str("label"))
		}
		argv := append(append(append([]string{c.mkfs}, c.forceFlags...), opts...), dev)
		if _, _, err := fs.runRC(argv, nil); err != nil {
			return fail(err)
		}
		if uuid != "" && c.changeUUID != "" && c.setUUIDOpts == nil {
			if _, r := fs.changeUUID(c, uuid, dev); r != nil {
				return done(r)
			}
		}
	}
	return done(&agentproto.Result{Changed: true})
}

func anyIn(a, b []string) bool {
	for _, x := range a {
		if containsStr(b, x) {
			return true
		}
	}
	return false
}

// devSize is Device.size().
func (fs *fsRun) devSize(dev string) (int64, *agentproto.Result) {
	info, err := os.Stat(dev)
	if err != nil {
		return 0, agentproto.Fail("%s", pyStrOSError(err, dev))
	}
	if info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
		out, _, err := fs.runRC([]string{"blockdev", "--getsize64", dev}, nil)
		if err != nil {
			if f, ok := err.(fsFailErr); ok {
				return 0, f.r
			}
			return 0, agentproto.Fail("%v", err)
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		return n, nil
	}
	if info.Mode().IsRegular() {
		return info.Size(), nil
	}
	return 0, agentproto.Fail("Target device not supported: %s", dev)
}

// growFS is Filesystem.grow().
func (fs *fsRun) growFS(c *fsClass, dev string) (string, *agentproto.Result) {
	devSize, f := fs.devSize(dev)
	if f != nil {
		return "", f
	}
	if c.size == nil {
		return "", agentproto.Fail("module does not support resizing %s filesystem yet", c.name)
	}
	fsSize, err := c.size(fs, dev)
	if err != nil {
		if fe, ok := err.(fsFailErr); ok {
			return "", fe.r
		}
		fs.warnings = append(fs.warnings, fmt.Sprintf("unable to process %s output '%s'", c.info, err))
		return "", agentproto.Fail("unable to process %s output for %s", c.info, dev)
	}
	if fsSize+c.growSlack >= devSize {
		msg := fmt.Sprintf("%s filesystem is using the whole device %s", c.name, dev)
		return "", &agentproto.Result{Msg: msg}
	}
	if fs.env.CheckMode {
		return "", &agentproto.Result{Changed: true, Msg: fmt.Sprintf("resizing filesystem %s on device %s", c.name, dev)}
	}
	target := dev
	if c.growMountOnly {
		mp, err := fs.mountpoint(dev)
		if err != nil {
			return "", err.(fsFailErr).r
		}
		if mp == "" {
			return "", agentproto.Fail("%s needs to be mounted for %s operations", dev, c.name)
		}
		target = mp
	}
	argv := append(append([]string{c.grow}, c.growFlags...), target)
	out, _, err := fs.runRC(argv, nil)
	if err != nil {
		return "", err.(fsFailErr).r
	}
	return out, nil
}

// changeUUID is Filesystem.change_uuid().
func (fs *fsRun) changeUUID(c *fsClass, uuid, dev string) (string, *agentproto.Result) {
	if fs.env.CheckMode {
		// The module exits with change=True (sic): no changed key.
		return "", &agentproto.Result{Msg: fmt.Sprintf("Changing %s filesystem UUID on device %s", c.name, dev),
			Extra: map[string]any{"change": true}}
	}
	argv := []string{c.changeUUID, c.changeUUIDOpt}
	if c.changeUUIDHasArg {
		argv = append(argv, uuid)
	}
	argv = append(argv, dev)
	out, _, err := fs.runRC(argv, nil)
	if err != nil {
		return "", err.(fsFailErr).r
	}
	return out, nil
}
