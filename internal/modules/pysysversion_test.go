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
		{"before 3.9.5 the branch is default", cBinary("3.8.2", "%s%s%s, %.20s, %.9s", "main", "default", "Jan  1 2020", "12:00:00", "[GCC 9.3.0]"),
			"3.8", "3.8.2 (default, Jan  1 2020, 12:00:00) [GCC 9.3.0]"},
		{"two build dates", cBinary("3.12.3", "main", "Aug 31 2026", "Aug 30 2026", "10:18:26", "[GCC 13.3.0]"), "3.12", ""},
		{"no compiler banner", cBinary("3.12.3", "main", "Aug 31 2026", "10:18:26"), "3.12", ""},
	}
	for _, c := range cases {
		if got := pySysVersionFromBinary(c.data, c.minor); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
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
