package modules

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sys.version of the target's Python, which some module errors quote
// ("... using /usr/bin/python3 (3.12.3 (main, Aug 31 2026, 10:18:26)
// [GCC 13.3.0])"). understudy does not run Python to ask: CPython builds
// it from string constants compiled into the interpreter (or its
// libpython) — PY_VERSION, the build date and time, and the compiler
// banner — formatted "%.80s (%.80s) %.80s" with build info "BRANCH,
// DATE, TIME". Those strings are read off the binary; the branch follows
// from the version (pyNoGitBranch).

// pyBuildInfoFormat is Py_GetBuildInfo's format string, which marks the
// binary that holds the build strings.
var pyBuildInfoFormat = []byte("%s%s%s, %.20s, %.9s\x00")

var (
	pyBuildDateRe = regexp.MustCompile(`^[A-Z][a-z]{2} [ 0-3][0-9] [0-9]{4}$`)
	pyBuildTimeRe = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}:[0-9]{2}$`)
	pyCompilerRe  = regexp.MustCompile(`^\n?\[(GCC|Clang|clang) [^\n]*\]$`)
)

// targetPythonSysVersion is the task's Python's sys.version; ok false
// when its binary does not show it.
func targetPythonSysVersion(env *RunEnv) (string, bool) {
	t := targetPython(env)
	if t.version == "" || len(t.paths) == 0 {
		return "", false
	}
	t.sysVersionOnce.Do(func() {
		t.sysVersion = pySysVersionOf(t.paths[len(t.paths)-1], t.version)
	})
	return t.sysVersion, t.sysVersion != ""
}

// pySysVersionOf reads sys.version from the interpreter at exe (major.minor
// minor) or the libpython it links.
func pySysVersionOf(exe, minor string) string {
	prefix := filepath.Dir(filepath.Dir(exe))
	// A macOS framework build keeps it in the framework's Python.
	candidates := []string{exe, filepath.Join(prefix, "Python")}
	for _, dir := range []string{filepath.Join(prefix, "lib64"), filepath.Join(prefix, "lib"), "/usr/lib64", "/lib64",
		"/usr/lib", "/lib", "/usr/local/lib"} {
		m, _ := filepath.Glob(filepath.Join(dir, "libpython"+minor+"*.so*"))
		candidates = append(candidates, m...)
		m, _ = filepath.Glob(filepath.Join(dir, "*-linux-*", "libpython"+minor+"*.so*"))
		candidates = append(candidates, m...)
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err != nil || !bytes.Contains(data, pyBuildInfoFormat) {
			continue
		}
		if v := pySysVersionFromBinary(data, minor); v != "" {
			return v
		}
	}
	return ""
}

// pySysVersionFromBinary assembles sys.version from the C strings of a
// CPython binary: each part must be the only candidate there.
func pySysVersionFromBinary(data []byte, minor string) string {
	versionRe := regexp.MustCompile(`^` + regexp.QuoteMeta(minor) + `\.[0-9]+(?:(?:a|b|rc)[0-9]+)?\+?$`)
	var version, date, clock, compiler []string
	for _, s := range cStrings(data) {
		switch {
		case versionRe.MatchString(s):
			version = append(version, s)
		case pyBuildDateRe.MatchString(s):
			date = append(date, s)
		case pyBuildTimeRe.MatchString(s):
			clock = append(clock, s)
		case pyCompilerRe.MatchString(s):
			compiler = append(compiler, s)
		}
	}
	version, date, clock, compiler = dedupeStrings(version), dedupeStrings(date), dedupeStrings(clock), dedupeStrings(compiler)
	if len(version) != 1 || len(date) != 1 || len(clock) != 1 || len(compiler) != 1 {
		return ""
	}
	branch := pyNoGitBranch(version[0])
	trunc := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	info := branch + ", " + trunc(date[0], 20) + ", " + trunc(clock[0], 9)
	return trunc(version[0], 80) + " (" + trunc(info, 80) + ") " + trunc(compiler[0], 80)
}

// pyNoGitBranch is the name Py_GetBuildInfo gives a build made outside a
// git checkout (every distro's), which the binary does not show apart:
// the literal is often only the tail of another string ("__main__"), as
// in Rocky 9's x86_64 libpython. Modules/getbuildinfo.c changed it from
// "default" to "main" in 3.9.8, 3.10.1 and 3.11.0a2; 3.8 and earlier
// kept "default".
func pyNoGitBranch(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 3 {
		return "main"
	}
	major, minor := atoiSafe(parts[0]), atoiSafe(parts[1])
	// The micro version, then any pre-release ("0a2", "25+").
	n := strings.IndexFunc(parts[2], func(r rune) bool { return r < '0' || r > '9' })
	if n < 0 {
		n = len(parts[2])
	}
	micro, pre := atoiSafe(parts[2][:n]), strings.TrimSuffix(parts[2][n:], "+")
	switch {
	case major < 3:
		return "default"
	case major > 3 || minor >= 12:
		return "main"
	case minor == 11 && micro == 0 && (pre == "a0" || pre == "a1"):
		return "default"
	case minor == 11, minor == 10 && micro >= 1, minor == 9 && micro >= 8:
		return "main"
	}
	return "default"
}

// cStrings lists the NUL-terminated runs of printable ASCII (tabs and
// newlines included) in a binary.
func cStrings(data []byte) []string {
	var out []string
	start := -1
	for i, c := range data {
		printable := c == '\t' || c == '\n' || (c >= 0x20 && c < 0x7f)
		switch {
		case printable:
			if start < 0 {
				start = i
			}
		case c == 0 && start >= 0:
			if i-start >= 3 {
				out = append(out, string(data[start:i]))
			}
			start = -1
		default:
			start = -1
		}
	}
	return out
}

// pySysVersionMessage is sys.version as the modules quote it, newlines
// removed; without the binary's strings, the major.minor understudy
// knows.
func pySysVersionMessage(env *RunEnv) string {
	if v, ok := targetPythonSysVersion(env); ok {
		return strings.ReplaceAll(v, "\n", "")
	}
	return targetPythonVersion(env)
}
