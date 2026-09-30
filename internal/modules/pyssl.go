package modules

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// CPython appends the _ssl.c source line that raised to every SSL error
// ("... (_ssl.c:1082)"). The line moves between CPython releases, and
// distributions patch _ssl.c too (Rocky's python3-libs 3.9.25-7.el9_8 and
// -7.el9_8.3 differ), so ansible's text depends on the exact Python build
// on the target. understudy reports the line only for builds it has
// measured, identified from the filesystem, and otherwise leaves the
// location out rather than claim one.

// Where the line comes from.
const (
	sslHandshake = iota // SSLSocket.do_handshake: verify failures, alerts, protocol errors
	sslCertChain        // SSLContext.load_cert_chain (client_cert)
	sslCAFile           // SSLContext.load_verify_locations(cafile=) (ca_path)
)

// pySSLLines maps a Python build to its _ssl.c lines, measured with that
// build's ssl module. Keys: "cpython-X.Y.Z" for an upstream build found
// by its install path, "deb:", "apk:" and "rpm:" plus the package version
// for a distribution's.
var pySSLLines = map[string][3]int{
	"cpython-3.11.16":        {1016, 3927, 4178}, // Homebrew
	"cpython-3.12.14":        {1010, 3855, 4106}, // Homebrew
	"cpython-3.14.7":         {1082, 4163, 4416}, // Homebrew
	"deb:3.11.2-6+deb12u8":   {992, 3874, 4123},  // Debian 12
	"deb:3.12.3-1ubuntu0.17": {1000, 3845, 4096}, // Ubuntu 24.04
	"apk:3.12.13-r0":         {1010, 3855, 4106}, // Alpine 3.20
	"rpm:3.9.18-1.el9_3":     {1129, 4044, 4293}, // Rocky Linux 9.3
	"rpm:3.9.25-7.el9_8":     {1147, 4062, 4311}, // Rocky Linux 9.6
	"rpm:3.9.25-7.el9_8.3":   {1147, 4062, 4313}, // Rocky Linux 9.6
}

// sslSuffix is " (_ssl.c:N)" for the target's Python build, or "".
func sslSuffix(env *RunEnv, kind int) string {
	lines, ok := pySSLLines[targetPythonBuild(env)]
	if !ok {
		return ""
	}
	return fmt.Sprintf(" (_ssl.c:%d)", lines[kind])
}

var pyReleaseRe = regexp.MustCompile(`(?:^|[/-])(3\.\d+\.\d+)(?:[/-]|$)`)

// targetPythonBuild identifies the task's Python build (a pySSLLines
// key) when the filesystem tells: its install path (Homebrew's Cellar,
// pyenv, uv name the release), else the dpkg, apk or rpm package that
// provides it. "" when unknown.
func targetPythonBuild(env *RunEnv) string {
	t := targetPython(env)
	t.buildOnce.Do(func() {
		minor := t.version
		if minor == "" {
			return
		}
		for _, p := range t.paths {
			if m := pyReleaseRe.FindStringSubmatch(p); m != nil && strings.HasPrefix(m[1], minor+".") {
				t.build = "cpython-" + m[1]
				return
			}
		}
		for _, c := range []struct{ kind, version string }{
			{"deb", pkgDBVersion("/var/lib/dpkg/status", "Package: ", "Version: ",
				"libpython"+minor+"-minimal", "python"+minor+"-minimal")},
			{"apk", pkgDBVersion("/lib/apk/db/installed", "P:", "V:", "python3")},
			{"rpm", rpmDBVersion(minor)},
		} {
			if strings.HasPrefix(c.version, minor+".") {
				t.build = c.kind + ":" + c.version
				return
			}
		}
	})
	return t.build
}

// pkgDBVersion reads the version (epoch dropped) of the first of names in
// a dpkg status or apk installed database (stanzas separated by blank
// lines, one "key<value>" line each).
func pkgDBVersion(db, nameKey, versionKey string, names ...string) string {
	f, err := os.Open(db)
	if err != nil {
		return ""
	}
	defer f.Close()
	found := map[string]string{}
	var pkg, ver string
	flush := func() {
		if pkg != "" && ver != "" {
			found[pkg] = ver
		}
		pkg, ver = "", ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, nameKey):
			pkg = strings.TrimPrefix(line, nameKey)
		case strings.HasPrefix(line, versionKey):
			ver = strings.TrimPrefix(line, versionKey)
		}
	}
	flush()
	for _, n := range names {
		if v := found[n]; v != "" {
			if _, after, ok := strings.Cut(v, ":"); ok { // dpkg epoch
				v = after
			}
			return v
		}
	}
	return ""
}

// rpmHeaderRe finds a python3 libs package's name, version and release,
// which an RPM header stores as consecutive NUL-terminated strings.
var rpmHeaderRe = regexp.MustCompile(`python3[0-9]*-libs\x00(3\.[0-9]+\.[0-9]+)\x00([0-9A-Za-z._+~^]{1,40})\x00`)

// rpmDBVersion is the version-release of the installed python3-libs
// ("3.9.25-7.el9_8.3"), read from the rpm database's raw headers without
// an SQLite or Berkeley DB reader. Pages freed by an upgrade can still
// hold the old header, so the newest version-release wins.
func rpmDBVersion(minor string) string {
	best := ""
	for _, db := range []string{"/var/lib/rpm/rpmdb.sqlite", "/var/lib/rpm/rpmdb.sqlite-wal", "/var/lib/rpm/Packages"} {
		data, err := os.ReadFile(db)
		if err != nil {
			continue
		}
		for _, m := range rpmHeaderRe.FindAllSubmatch(data, -1) {
			v := string(m[1]) + "-" + string(m[2])
			if strings.HasPrefix(v, minor+".") && (best == "" || rpmVerCmp(v, best) > 0) {
				best = v
			}
		}
	}
	return best
}

// rpmVerCmp is rpm's rpmvercmp over whole version-release strings:
// alphanumeric segments compare numerically or lexically, a numeric
// segment beating an alphabetic one, '~' sorting before anything.
func rpmVerCmp(a, b string) int {
	isAlnum := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && !isAlnum(a[0]) && a[0] != '~' {
			a = a[1:]
		}
		for len(b) > 0 && !isAlnum(b[0]) && b[0] != '~' {
			b = b[1:]
		}
		if strings.HasPrefix(a, "~") || strings.HasPrefix(b, "~") {
			if !strings.HasPrefix(a, "~") {
				return 1
			}
			if !strings.HasPrefix(b, "~") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if len(a) == 0 || len(b) == 0 {
			break
		}
		num := isDigit(a[0])
		seg := func(s string) (string, string) {
			i := 0
			for i < len(s) && isAlnum(s[i]) && isDigit(s[i]) == num {
				i++
			}
			return s[:i], s[i:]
		}
		var sa, sb string
		sa, a = seg(a)
		sb, b = seg(b)
		if sb == "" {
			// Different segment types: numeric wins.
			if num {
				return 1
			}
			return -1
		}
		if num {
			sa, sb = strings.TrimLeft(sa, "0"), strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				return len(sa) - len(sb)
			}
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}

