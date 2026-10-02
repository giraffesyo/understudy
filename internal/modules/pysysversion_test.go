package modules

import (
	"os"
	"strings"
	"testing"
)

// cBinary lays strings out as a binary's string table does.
func cBinary(strs ...string) []byte {
	return []byte("\x7fELF\x00\x01\x02" + strings.Join(strs, "\x00") + "\x00\xff\xfe")
}

func TestPySysVersionFromBinary(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		minor string
		want  string
	}{
		{"Ubuntu 24.04", cBinary("3.2.0", "3.12.3", "%s%s%s, %.20s, %.9s", "main", "default", "Aug 31 2026", "10:18:26", "[GCC 13.3.0]"),
			"3.12", "3.12.3 (main, Aug 31 2026, 10:18:26) [GCC 13.3.0]"},
		{"Rocky 9 (a newline before the compiler)", cBinary("3.9.25", "%s%s%s, %.20s, %.9s", "main", "Aug  6 2026", "00:00:00",
			"\n[GCC 11.5.0 20240719 (Red Hat 11.5.0-14)]"),
			"3.9", "3.9.25 (main, Aug  6 2026, 00:00:00) \n[GCC 11.5.0 20240719 (Red Hat 11.5.0-14)]"},
		{"Rocky 9 x86_64 (main only the tail of __main__)", cBinary("3.9.25", "%s%s%s, %.20s, %.9s", "__main__", "Aug  6 2026", "00:00:00",
			"\n[GCC 11.5.0 20240719 (Red Hat 11.5.0-14)]"),
			"3.9", "3.9.25 (main, Aug  6 2026, 00:00:00) \n[GCC 11.5.0 20240719 (Red Hat 11.5.0-14)]"},
		{"3.8 builds say default", cBinary("3.8.2", "%s%s%s, %.20s, %.9s", "main", "default", "Jan  1 2020", "12:00:00", "[GCC 9.3.0]"),
			"3.8", "3.8.2 (default, Jan  1 2020, 12:00:00) [GCC 9.3.0]"},
		{"macOS's 3.9.6", cBinary("3.9.6", "%s%s%s, %.20s, %.9s", "main", "default", "May 22 2026", "11:13:45", "\n[Clang 21.0.0 (clang-2100.1.1.101)]"),
			"3.9", "3.9.6 (default, May 22 2026, 11:13:45) \n[Clang 21.0.0 (clang-2100.1.1.101)]"},
		{"two build dates", cBinary("3.12.3", "main", "Aug 31 2026", "Aug 30 2026", "10:18:26", "[GCC 13.3.0]"), "3.12", ""},
		{"no compiler banner", cBinary("3.12.3", "main", "Aug 31 2026", "10:18:26"), "3.12", ""},
	}
	for _, c := range cases {
		if got := pySysVersionFromBinary(c.data, c.minor); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPyNoGitBranch follows Modules/getbuildinfo.c's tags.
func TestPyNoGitBranch(t *testing.T) {
	for v, want := range map[string]string{
		"2.7.18": "default", "3.6.15": "default", "3.8.20": "default", "3.9.0": "default", "3.9.7": "default",
		"3.9.8": "main", "3.9.25": "main", "3.10.0": "default", "3.10.0rc2": "default", "3.10.1": "main",
		"3.11.0a1": "default", "3.11.0a2": "main", "3.11.0": "main", "3.12.3": "main", "3.14.0+": "main",
	} {
		if got := pyNoGitBranch(v); got != want {
			t.Errorf("%s: %s, want %s", v, got, want)
		}
	}
}

// TestPySysVersionOfRealBinaries reads interpreters copied out of distro
// images, when PYSYSVERSION_BINARIES names a directory holding them as
// <file> and <file>.want (the sys.version that Python prints).
func TestPySysVersionOfRealBinaries(t *testing.T) {
	dir := os.Getenv("PYSYSVERSION_BINARIES")
	if dir == "" {
		t.Skip("PYSYSVERSION_BINARIES not set")
	}
	wants, _ := os.ReadDir(dir)
	for _, w := range wants {
		bin, ok := strings.CutSuffix(w.Name(), ".want")
		if !ok {
			continue
		}
		data, err := os.ReadFile(dir + "/" + bin)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(dir + "/" + w.Name())
		lines := strings.SplitN(string(want), "\n", 2)
		minor, sysVersion := lines[0], strings.TrimSuffix(lines[1], "\n")
		if got := pySysVersionFromBinary(data, minor); got != sysVersion {
			t.Errorf("%s: got %q, want %q", bin, got, sysVersion)
		}
	}
}
