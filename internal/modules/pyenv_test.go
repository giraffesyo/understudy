package modules

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPyReleaseFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/homebrew/Cellar/python@3.14/3.14.7/Frameworks/Python.framework/Versions/3.14/bin/python3.14": "3.14.7",
		"/home/u/.pyenv/versions/3.12.1/bin/python3.12":                                                    "3.12.1",
		"/home/u/.local/share/uv/python/cpython-3.11.9-linux-x86_64-gnu/bin/python3.11":                    "3.11.9",
		"/usr/bin/python3.12": "",
	} {
		got := ""
		if m := pyReleaseRe.FindStringSubmatch(path); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

func TestPkgDBVersion(t *testing.T) {
	dir := t.TempDir()
	dpkg := filepath.Join(dir, "status")
	os.WriteFile(dpkg, []byte("Package: libpython3.12-minimal\nStatus: install ok installed\n"+
		"Version: 3.12.3-1ubuntu0.8\n\nPackage: python3.12-minimal\nVersion: 3.12.3-1ubuntu0.8\n"), 0o644)
	if v := pkgDBVersion(dpkg, "Package: ", "Version: ", "libpython3.12-minimal"); v != "3.12.3-1ubuntu0.8" {
		t.Errorf("dpkg: %q", v)
	}
	apk := filepath.Join(dir, "installed")
	os.WriteFile(apk, []byte("C:Q1abc\nP:python3\nV:3.12.12-r0\nA:x86_64\n\nP:musl\nV:1.2.5-r0\n"), 0o644)
	if v := pkgDBVersion(apk, "P:", "V:", "python3"); v != "3.12.12-r0" {
		t.Errorf("apk: %q", v)
	}
}

func TestPyDistVersion(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "PyMySQL-1.0.2.dist-info"), 0o755)
	os.WriteFile(filepath.Join(dir, "PyMySQL-1.0.2.dist-info", "METADATA"),
		[]byte("Metadata-Version: 2.1\nName: PyMySQL\nVersion: 1.0.2\n\nbody Version: 9\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "mysqlclient-2.1.1.egg-info"), []byte("Name: mysqlclient\nVersion: 2.1.1\n"), 0o644)
	if v := pyDistVersionIn([]string{dir}, "pymysql"); v != "1.0.2" {
		t.Errorf("pymysql: %q", v)
	}
	if v := pyDistVersionIn([]string{dir}, "mysqlclient"); v != "2.1.1" {
		t.Errorf("mysqlclient: %q", v)
	}
	if v := pyDistVersionIn([]string{dir}, "cryptography"); v != "" {
		t.Errorf("cryptography: %q", v)
	}
}

func TestRPMVerCmp(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"3.9.25-7.el9_8.3", "3.9.25-7.el9_8", 1},
		{"3.9.25-7.el9_8", "3.9.18-1.el9_3", 1},
		{"3.9.10-1", "3.9.9-1", 1},
		{"1.0~rc1", "1.0", -1},
		{"1.0a", "1.0.1", -1},
		{"2.0", "2.0", 0},
	} {
		got := rpmVerCmp(c.a, c.b)
		if (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) {
			t.Errorf("rpmVerCmp(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRPMHeaderRe(t *testing.T) {
	data := []byte("xxpython3-libs\x003.9.25\x007.el9_8\x00yy python3-libs\x003.9.25\x007.el9_8.3\x00zz")
	var got []string
	for _, m := range rpmHeaderRe.FindAllSubmatch(data, -1) {
		got = append(got, string(m[1])+"-"+string(m[2]))
	}
	if len(got) != 2 || got[1] != "3.9.25-7.el9_8.3" {
		t.Errorf("matches: %q", got)
	}
}
