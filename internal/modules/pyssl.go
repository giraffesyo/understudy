package modules

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CPython appends the _ssl.c source line that raised to every SSL error
// ("... (_ssl.c:1082)"). The line moves between CPython releases (even
// patch releases: 3.12.3 and 3.12.14 differ), so ansible's text depends
// on the exact Python build on the target. understudy reports the line
// only for builds it has measured, identified from the filesystem, and
// otherwise leaves the location out rather than claim one.

// Where the line comes from.
const (
	sslHandshake = iota // SSLSocket.do_handshake: verify failures, alerts, protocol errors
	sslCertChain        // SSLContext.load_cert_chain (client_cert)
	sslCAFile           // SSLContext.load_verify_locations(cafile=) (ca_path)
)

// pySSLLines maps a CPython release to its _ssl.c lines, measured with
// that build's ssl module (the distribution's package where noted).
var pySSLLines = map[string][3]int{
	"3.9.6":   {1129, 4044, 4293}, // macOS /usr/bin/python3
	"3.9.18":  {1129, 4044, 4293}, // Rocky Linux 9.3
	"3.9.25":  {1147, 4062, 4311}, // Rocky Linux 9.6
	"3.11.2":  {992, 3874, 4123},  // Debian 12
	"3.11.16": {1016, 3927, 4178}, // Homebrew
	"3.12.3":  {1000, 3845, 4096}, // Ubuntu 24.04
	"3.12.13": {1010, 3855, 4106}, // Alpine 3.20
	"3.12.14": {1010, 3855, 4106}, // Homebrew
	"3.14.7":  {1082, 4163, 4416}, // Homebrew
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

// targetPythonBuild is the CPython release ("3.12.3") of the task's
// Python, when the filesystem tells: its install path (Homebrew's Cellar,
// pyenv, uv), the dpkg or apk package that provides it, or the build tree
// its sysconfig data records. "" when unknown.
func targetPythonBuild(env *RunEnv) string {
	t := targetPython(env)
	t.buildOnce.Do(func() {
		minor := t.version
		if minor == "" {
			return
		}
		match := func(v string) bool {
			return v != "" && strings.HasPrefix(v, minor+".")
		}
		for _, p := range t.paths {
			if m := pyReleaseRe.FindStringSubmatch(p); m != nil && match(m[1]) {
				t.build = m[1]
				return
			}
		}
		for _, v := range []string{
			pkgDBVersion("/var/lib/dpkg/status", "Package: ", "Version: ",
				"libpython"+minor+"-minimal", "python"+minor+"-minimal"),
			pkgDBVersion("/lib/apk/db/installed", "P:", "V:", "python3"),
			sysconfigBuild(minor),
		} {
			if match(v) {
				t.build = v
				return
			}
		}
	})
	return t.build
}

// pkgDBVersion reads the upstream version of the first of names in a
// dpkg status or apk installed database (stanzas separated by blank
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
			// Upstream part: up to the Debian revision or apk release.
			if i := strings.IndexAny(v, "-+~"); i >= 0 {
				v = v[:i]
			}
			return v
		}
	}
	return ""
}

var sysconfigBuildRe = regexp.MustCompile(`Python-(3\.\d+\.\d+)[/'" ]`)

// sysconfigBuild finds the CPython release in the build paths the
// interpreter's _sysconfigdata records (RPM builds compile in
// .../BUILD/Python-3.9.21).
func sysconfigBuild(minor string) string {
	for _, prefix := range []string{"/usr/lib64", "/usr/lib", "/usr/local/lib"} {
		files, _ := filepath.Glob(prefix + "/python" + minor + "/_sysconfigdata_*.py")
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if m := sysconfigBuildRe.FindSubmatch(data); m != nil {
				return string(m[1])
			}
		}
	}
	return ""
}
