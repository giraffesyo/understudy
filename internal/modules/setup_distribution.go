package modules

import (
	"regexp"
	"sort"
	"strings"
)

// Distribution facts: ansible's DistributionFiles over the bundled `distro`
// library's guesses (module_utils/facts/system/distribution.py).

var osFamilyMap = func() map[string]string {
	m := map[string]string{}
	for fam, members := range map[string][]string{
		"RedHat": {"RedHat", "RHEL", "Fedora", "CentOS", "Scientific", "SLC",
			"Ascendos", "CloudLinux", "PSBM", "OracleLinux", "OVS",
			"OEL", "Amazon", "Amzn", "Virtuozzo", "XenServer", "Alibaba",
			"EulerOS", "openEuler", "AlmaLinux", "Rocky", "TencentOS",
			"EuroLinux", "Kylin Linux Advanced Server", "MIRACLE"},
		"Debian": {"Debian", "Ubuntu", "Raspbian", "Neon", "KDE neon",
			"Linux Mint", "SteamOS", "Devuan", "Kali", "Cumulus Linux",
			"Pop!_OS", "Parrot", "Pardus GNU/Linux", "Uos", "Deepin", "OSMC"},
		"Suse": {"SuSE", "SLES", "SLED", "openSUSE", "openSUSE Tumbleweed",
			"SLES_SAP", "SUSE_LINUX", "openSUSE Leap", "ALP-Dynamic", "SL-Micro"},
		"Archlinux":  {"Archlinux", "Antergos", "Manjaro"},
		"Mandrake":   {"Mandrake", "Mandriva"},
		"Solaris":    {"Solaris", "Nexenta", "OmniOS", "OpenIndiana", "SmartOS"},
		"Slackware":  {"Slackware"},
		"Altlinux":   {"Altlinux"},
		"SMGL":       {"SMGL"},
		"Gentoo":     {"Gentoo", "Funtoo"},
		"Alpine":     {"Alpine"},
		"AIX":        {"AIX"},
		"HP-UX":      {"HPUX"},
		"Darwin":     {"MacOSX"},
		"FreeBSD":    {"FreeBSD", "TrueOS"},
		"ClearLinux": {"Clear Linux OS", "Clear Linux Mix"},
		"DragonFly":  {"DragonflyBSD", "DragonFlyBSD", "Gentoo/DragonflyBSD", "Gentoo/DragonFlyBSD"},
		"NetBSD":     {"NetBSD"},
	} {
		for _, d := range members {
			m[d] = fam
		}
	}
	return m
}()

// distroInfo is the subset of the `distro` library ansible relies on.
type distroInfo struct {
	osRelease  map[string]string
	release    map[string]string // distro release file (e.g. /etc/redhat-release)
	debVersion string
}

func loadDistro(e *factEnv) *distroInfo {
	d := &distroInfo{osRelease: map[string]string{}, release: map[string]string{}}
	data, ok := e.fileContent("/etc/os-release")
	if !ok {
		data, _ = e.fileContent("/usr/lib/os-release")
	}
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		d.osRelease[strings.ToLower(k)] = shellUnquote(v)
	}
	if v, ok := d.osRelease["version"]; ok {
		if m := regexp.MustCompile(`\((\D+)\)|,\s*(\D+)`).FindStringSubmatch(v); m != nil {
			c := m[1]
			if c == "" {
				c = m[2]
			}
			d.osRelease["codename"] = c
			d.osRelease["release_codename"] = c
		}
	}
	if v, ok := d.osRelease["version_codename"]; ok {
		d.osRelease["codename"] = v
	} else if v, ok := d.osRelease["ubuntu_codename"]; ok {
		d.osRelease["codename"] = v
	}

	ignore := map[string]bool{"debian_version": true, "lsb-release": true, "oem-release": true,
		"os-release": true, "system-release": true, "plesk-release": true, "iredmail-release": true,
		"board-release": true, "ec2_version": true}
	pat := regexp.MustCompile(`^(\w+)[-_](release|version)$`)
	names, _ := e.listDir("/etc")
	sort.Strings(names)
	for _, n := range names {
		if ignore[n] {
			continue
		}
		m := pat.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		if !e.isFile("/etc/" + n) {
			continue
		}
		lines := e.fileLines("/etc/" + n)
		first := ""
		if len(lines) > 0 {
			first = lines[0]
		}
		info := parseDistroReleaseContent(first)
		if _, ok := info["name"]; !ok {
			continue
		}
		info["id"] = m[1]
		if strings.Contains(strings.ToLower(info["name"]), "cloudlinux") {
			info["id"] = "cloudlinux"
		}
		d.release = info
		break
	}
	d.debVersion, _ = e.fileContent("/etc/debian_version")
	return d
}

var distroReleaseReversed = regexp.MustCompile(`^(?:[^)]*\)(.*)\()? *(?:STL )?([\d.+\-a-z]*\d) *(?:esaeler *)?(.+)`)

// parseDistroReleaseContent is distro's _parse_distro_release_content.
func parseDistroReleaseContent(line string) map[string]string {
	info := map[string]string{}
	rev := reverseString(strings.TrimSpace(line))
	if m := distroReleaseReversed.FindStringSubmatch(rev); m != nil {
		info["name"] = reverseString(m[3])
		if m[2] != "" {
			info["version_id"] = reverseString(m[2])
		}
		if m[1] != "" {
			info["codename"] = reverseString(m[1])
		}
	} else if line != "" {
		info["name"] = strings.TrimSpace(line)
	}
	return info
}

func reverseString(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func (d *distroInfo) id() string {
	norm := func(s string, table map[string]string) string {
		s = strings.ReplaceAll(strings.ToLower(s), " ", "_")
		if v, ok := table[s]; ok {
			return v
		}
		return s
	}
	if v := d.osRelease["id"]; v != "" {
		return norm(v, map[string]string{"ol": "oracle", "opensuse-leap": "opensuse"})
	}
	if v := d.release["id"]; v != "" {
		return norm(v, map[string]string{"redhat": "rhel"})
	}
	return ""
}

func (d *distroInfo) like() string { return d.osRelease["id_like"] }

func (d *distroInfo) version(best bool) string {
	versions := []string{
		d.osRelease["version_id"],
		d.release["version_id"],
		parseDistroReleaseContent(d.osRelease["pretty_name"])["version_id"],
	}
	id := d.id()
	if id == "debian" || containsWord(d.like(), "debian") {
		versions = append(versions, d.debVersion)
	}
	version := ""
	if best {
		for _, v := range versions {
			if strings.Count(v, ".") > strings.Count(version, ".") || version == "" {
				version = v
			}
		}
	} else {
		for _, v := range versions {
			if v != "" {
				version = v
				break
			}
		}
	}
	return version
}

func (d *distroInfo) codename() string {
	if v, ok := d.osRelease["codename"]; ok {
		return v
	}
	return d.release["codename"]
}

func containsWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}

// shellUnquote handles os-release value quoting (shlex-style).
func shellUnquote(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	var quote byte
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(v) && strings.IndexByte("\"\\$`\n", v[i+1]) >= 0 {
				i++
				b.WriteByte(v[i])
			} else {
				b.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '\\' && i+1 < len(v):
			i++
			b.WriteByte(v[i])
		case c == '#' && b.Len() == 0:
			return ""
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

type distFile struct {
	path, name string
	allowEmpty bool
}

var osDistList = []distFile{
	{"/etc/altlinux-release", "Altlinux", false},
	{"/etc/oracle-release", "OracleLinux", false},
	{"/etc/slackware-version", "Slackware", false},
	{"/etc/centos-release", "CentOS", false},
	{"/etc/redhat-release", "RedHat", false},
	{"/etc/vmware-release", "VMwareESX", true},
	{"/etc/openwrt_release", "OpenWrt", false},
	{"/etc/os-release", "Amazon", false},
	{"/etc/system-release", "Amazon", false},
	{"/etc/alpine-release", "Alpine", false},
	{"/etc/arch-release", "Archlinux", true},
	{"/etc/os-release", "Archlinux", false},
	{"/etc/os-release", "SUSE", false},
	{"/etc/SuSE-release", "SUSE", false},
	{"/etc/gentoo-release", "Gentoo", false},
	{"/etc/os-release", "Debian", false},
	{"/etc/lsb-release", "Debian", false},
	{"/etc/lsb-release", "Mandriva", false},
	{"/etc/sourcemage-release", "SMGL", false},
	{"/usr/lib/os-release", "ClearLinux", false},
	{"/etc/coreos/update.conf", "Coreos", false},
	{"/etc/os-release", "Flatcar", false},
	{"/etc/os-release", "NA", false},
}

var distSearchString = map[string]string{
	"OracleLinux": "Oracle Linux",
	"RedHat":      "Red Hat",
	"Altlinux":    "ALT",
	"SMGL":        "Source Mage GNU/Linux",
}

func collectDistribution(e *factEnv, prior map[string]any) map[string]any {
	u := e.uname()
	f := map[string]any{
		"distribution":         u.sysname,
		"distribution_release": u.release,
		"distribution_version": u.version,
	}
	switch e.system {
	case "Linux":
		for k, v := range processDistFiles(e) {
			f[k] = v
		}
	case "Darwin":
		f["distribution"] = "MacOSX"
		if sw := e.binPath("sw_vers"); sw != "" {
			if rc, out, _ := e.run(sw, "-productVersion"); rc == 0 {
				v := strings.TrimSpace(out)
				f["distribution_version"] = v
				f["distribution_major_version"] = strings.SplitN(v, ".", 2)[0]
			}
		}
	}
	dist, _ := f["distribution"].(string)
	f["os_family"] = dist
	if fam, ok := osFamilyMap[dist]; ok {
		f["os_family"] = fam
	}
	return f
}

func processDistFiles(e *factEnv) map[string]any {
	d := loadDistro(e)
	facts := guessDistribution(e, d)
	for _, df := range osDistList {
		if !e.exists(df.path) {
			continue
		}
		if df.allowEmpty {
			facts["distribution"] = df.name
			facts["distribution_file_path"] = df.path
			facts["distribution_file_variety"] = df.name
			break
		}
		content, ok := e.fileContent(df.path)
		if !ok {
			continue // exists but empty
		}
		parsed, pf := parseDistFile(e, d, df.name, content, df.path, facts)
		if parsed {
			facts["distribution"] = df.name
			facts["distribution_file_path"] = df.path
			facts["distribution_file_variety"] = df.name
			facts["distribution_file_parsed"] = true
			for k, v := range pf {
				facts[k] = v
			}
			break
		}
	}
	return facts
}

func guessDistribution(e *factEnv, d *distroInfo) map[string]any {
	id := d.id()
	dist := pyCapitalize(id)
	switch dist {
	case "Amzn":
		dist = "Amazon"
	case "Rhel":
		dist = "Redhat"
	case "":
		dist = "OtherLinux"
	}
	version := d.version(false)
	if version != "" {
		switch id {
		case "centos":
			parts := strings.Split(d.version(true), ".")
			if len(parts) > 2 {
				parts = parts[:2]
			}
			version = strings.Join(parts, ".")
		case "debian":
			version = d.version(true)
		}
	}
	// get_distribution_codename
	var codename *string
	if v, ok := d.osRelease["version_codename"]; ok {
		codename = &v
	} else if v, ok := d.osRelease["ubuntu_codename"]; ok {
		codename = &v
	}
	if codename == nil && id == "ubuntu" {
		if v, ok := lsbReleaseFileAttr(e, "DISTRIB_CODENAME"); ok {
			codename = &v
		}
	}
	if codename == nil {
		if c := d.codename(); c != "" {
			codename = &c
		}
	}
	g := map[string]any{"distribution": dist}
	if version == "" {
		version = "NA"
	}
	g["distribution_version"] = version
	if codename == nil {
		g["distribution_release"] = "NA"
	} else {
		g["distribution_release"] = *codename
	}
	major := strings.SplitN(version, ".", 2)[0]
	if major == "" {
		major = "NA"
	}
	g["distribution_major_version"] = major
	return g
}

func lsbReleaseFileAttr(e *factEnv, key string) (string, bool) {
	for _, line := range e.fileLines("/etc/lsb-release") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return shellUnquote(v), true
		}
	}
	return "", false
}

func parseDistFile(e *factEnv, d *distroInfo, name, data, path string, collected map[string]any) (bool, map[string]any) {
	f := map[string]any{}
	data = strings.Trim(data, `'"\`)
	if s, ok := distSearchString[name]; ok {
		if strings.Contains(data, s) {
			f["distribution"] = name
			f["distribution_file_search_string"] = s
		} else {
			f["distribution"] = strings.Fields(data)[0]
		}
		return true, f
	}
	if name == "Archlinux" {
		if strings.Contains(data, "Arch Linux") {
			f["distribution"] = name
			return true, f
		}
		return false, f
	}
	switch name {
	case "Slackware":
		if !strings.Contains(data, "Slackware") {
			return false, f
		}
		f["distribution"] = name
		if v := regexp.MustCompile(`\w+[.]\w+\+?`).FindString(data); v != "" {
			f["distribution_version"] = v
		}
		return true, f
	case "CentOS":
		if strings.Contains(data, "CentOS Stream") {
			f["distribution_release"] = "Stream"
			return true, f
		}
		if strings.Contains(data, "TencentOS Server") {
			f["distribution"] = "TencentOS"
			return true, f
		}
		return false, f
	case "OpenWrt":
		if !strings.Contains(data, "OpenWrt") {
			return false, f
		}
		f["distribution"] = name
		if m := regexp.MustCompile(`DISTRIB_RELEASE="(.*)"`).FindStringSubmatch(data); m != nil {
			f["distribution_version"] = m[1]
		}
		if m := regexp.MustCompile(`DISTRIB_CODENAME="(.*)"`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
		return true, f
	case "Amazon":
		if !strings.Contains(data, "Amazon") {
			return false, f
		}
		f["distribution"] = "Amazon"
		if path == "/etc/os-release" {
			if m := regexp.MustCompile(`VERSION_ID="(.*)"`).FindStringSubmatch(data); m != nil {
				f["distribution_version"] = m[1]
				parts := strings.Split(m[1], ".")
				f["distribution_major_version"] = parts[0]
				if len(parts) > 1 {
					f["distribution_minor_version"] = parts[1]
				} else {
					f["distribution_minor_version"] = "NA"
				}
			}
		} else {
			v := "NA"
			for _, w := range strings.Fields(data) {
				if isDigits(w) {
					v = w
					break
				}
			}
			f["distribution_version"] = v
		}
		return true, f
	case "Alpine":
		f["distribution"] = "Alpine"
		f["distribution_version"] = data
		return true, f
	case "SUSE":
		return parseSUSE(e, data, path)
	case "Debian":
		return parseDebian(e, data, path, collected)
	case "Mandriva":
		if !strings.Contains(data, "Mandriva") {
			return false, f
		}
		f["distribution"] = name
		if m := regexp.MustCompile(`DISTRIB_RELEASE="(.*)"`).FindStringSubmatch(data); m != nil {
			f["distribution_version"] = m[1]
		}
		if m := regexp.MustCompile(`DISTRIB_CODENAME="(.*)"`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
		f["distribution"] = name
		return true, f
	case "ClearLinux":
		if !strings.Contains(strings.ToLower(data), "clearlinux") {
			return false, f
		}
		for _, kv := range [][2]string{{"NAME", "distribution"}, {"VERSION_ID", "distribution_major_version"}, {"VERSION_ID", "distribution_version"}, {"ID", "distribution_release"}} {
			if m := regexp.MustCompile(`(?m)^` + kv[0] + `=(.*)`).FindStringSubmatch(data); m != nil {
				f[kv[1]] = strings.Trim(m[1], `"`)
			}
		}
		return true, f
	case "Coreos", "Flatcar":
		if !strings.EqualFold(d.id(), name) || data == "" {
			return false, f
		}
		key := "GROUP"
		if name == "Flatcar" {
			key = "VERSION"
		}
		if m := regexp.MustCompile(key + `=(.*)`).FindStringSubmatch(data); m != nil {
			v := strings.Trim(m[1], `"`)
			if name == "Coreos" {
				f["distribution_release"] = v
			} else {
				f["distribution_major_version"] = strings.SplitN(v, ".", 2)[0]
				f["distribution_version"] = v
			}
		}
		return true, f
	case "NA":
		for _, line := range strings.Split(data, "\n") {
			if m := regexp.MustCompile(`^NAME=(.*)`).FindStringSubmatch(line); m != nil && collected["distribution"] == "NA" {
				f["distribution"] = strings.Trim(m[1], `"`)
			}
			if m := regexp.MustCompile(`^VERSION=(.*)`).FindStringSubmatch(line); m != nil && collected["distribution_version"] == "NA" {
				f["distribution_version"] = strings.Trim(m[1], `"`)
			}
		}
		return true, f
	}
	return false, f
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseSUSE(e *factEnv, data, path string) (bool, map[string]any) {
	f := map[string]any{}
	lower := strings.ToLower(data)
	if !strings.Contains(lower, "suse") {
		return false, f
	}
	verRe := regexp.MustCompile(`^VERSION_ID="?([0-9]+\.?[0-9]*)"?`)
	relRe := regexp.MustCompile(`^VERSION_ID="?[0-9]+\.?([0-9]*)"?`)
	if path == "/etc/os-release" {
		for _, line := range strings.Split(data, "\n") {
			if m := regexp.MustCompile(`^NAME=(.*)`).FindStringSubmatch(line); m != nil {
				f["distribution"] = strings.Trim(m[1], `"`)
			}
			if m := verRe.FindStringSubmatch(line); m != nil {
				f["distribution_version"] = m[1]
				f["distribution_major_version"] = strings.SplitN(m[1], ".", 2)[0]
			}
			if strings.Contains(lower, "open") {
				if m := relRe.FindStringSubmatch(line); m != nil {
					f["distribution_release"] = m[1]
				}
			} else if strings.Contains(lower, "enterprise") && strings.Contains(line, "VERSION_ID") {
				rel := "0"
				if m := relRe.FindStringSubmatch(line); m != nil && m[1] != "" {
					rel = m[1]
				}
				f["distribution_release"] = rel
			}
		}
	} else if path == "/etc/SuSE-release" {
		lines := strings.Split(data, "\n")
		if strings.Contains(lower, "open") {
			f["distribution"] = strings.Fields(lines[0])[0]
			for _, line := range lines {
				if m := regexp.MustCompile(`CODENAME *= *([^\n]+)`).FindStringSubmatch(line); m != nil {
					f["distribution_release"] = strings.TrimSpace(m[1])
				}
			}
		} else if strings.Contains(lower, "enterprise") {
			f["distribution"] = "SLES"
			for _, line := range lines {
				if m := regexp.MustCompile(`PATCHLEVEL = ([0-9]+)`).FindStringSubmatch(line); m != nil {
					f["distribution_release"] = m[1]
					if v, ok := f["distribution_version"].(string); ok {
						f["distribution_version"] = v + "." + m[1]
					}
				}
			}
		}
	}
	if e.isLink("/etc/products.d/baseproduct") && strings.HasSuffix(e.realpath("/etc/products.d/baseproduct"), "SLES_SAP.prod") {
		f["distribution"] = "SLES_SAP"
	}
	return true, f
}

func parseDebian(e *factEnv, data, path string, collected map[string]any) (bool, map[string]any) {
	f := map[string]any{}
	switch {
	case strings.Contains(data, "Debian") || strings.Contains(data, "Raspbian"):
		f["distribution"] = "Debian"
		if m := regexp.MustCompile(`PRETTY_NAME=[^(]+ \(?([^)]+?)\)`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
		if collected["distribution_release"] == "NA" && strings.Contains(data, "Debian") {
			if dpkg := e.binPath("dpkg"); dpkg != "" {
				if rc, out, _ := e.run(dpkg, "--status", "tzdata"); rc == 0 {
					for _, line := range strings.Split(out, "\n") {
						if strings.Contains(line, "Provides") {
							parts := strings.Split(line, "-")
							if len(parts) > 1 {
								f["distribution_release"] = strings.TrimSpace(parts[1])
							}
						}
					}
				}
			}
		}
		for _, line := range e.fileLines("/etc/debian_version") {
			if m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				f["distribution_minor_version"] = m[2]
			}
		}
	case strings.Contains(data, "Ubuntu"):
		f["distribution"] = "Ubuntu"
	case strings.Contains(data, "SteamOS"):
		f["distribution"] = "SteamOS"
	case (path == "/etc/lsb-release" || path == "/etc/os-release") && (strings.Contains(data, "Kali") || strings.Contains(data, "Parrot")):
		if strings.Contains(data, "Kali") {
			f["distribution"] = "Kali"
		} else {
			f["distribution"] = "Parrot"
		}
		if m := regexp.MustCompile(`DISTRIB_RELEASE=(.*)`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
	case strings.Contains(data, "Devuan"):
		f["distribution"] = "Devuan"
		if m := regexp.MustCompile(`PRETTY_NAME=\"?[^(\"]+ \(?([^) \"]+)\)?`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
		if m := regexp.MustCompile(`VERSION_ID=\"(.*)\"`).FindStringSubmatch(data); m != nil {
			f["distribution_version"] = m[1]
			f["distribution_major_version"] = m[1]
		}
	case strings.Contains(data, "Cumulus"):
		f["distribution"] = "Cumulus Linux"
		if m := regexp.MustCompile(`VERSION_ID=(.*)`).FindStringSubmatch(data); m != nil {
			v := strings.Trim(m[1], `"`)
			f["distribution_version"] = v
			parts := strings.Split(v, ".")
			f["distribution_major_version"] = parts[0]
			if len(parts) > 1 {
				f["distribution_release"] = parts[0] + "." + parts[1]
			}
		}
	case strings.Contains(data, "Mint"):
		f["distribution"] = "Linux Mint"
		if m := regexp.MustCompile(`VERSION_ID=\"(.*)\"`).FindStringSubmatch(data); m != nil {
			f["distribution_version"] = m[1]
			f["distribution_major_version"] = strings.SplitN(m[1], ".", 2)[0]
		}
	case strings.Contains(data, "UOS") || strings.Contains(data, "Uos") || strings.Contains(data, "uos"):
		f["distribution"] = "Uos"
		if m := regexp.MustCompile(`VERSION_CODENAME=\"?([^\"]+)\"?`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
	case strings.Contains(data, "Deepin") || strings.Contains(data, "deepin"):
		f["distribution"] = "Deepin"
		if m := regexp.MustCompile(`VERSION_CODENAME=\"?([^\"]+)\"?`).FindStringSubmatch(data); m != nil {
			f["distribution_release"] = m[1]
		}
	default:
		return false, f
	}
	return true, f
}
