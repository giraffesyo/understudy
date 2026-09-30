package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPySplitExt(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":        ".txt",
		"/d/a.tar.gz":  ".gz",
		"/d/.hidden":   "",
		"/d/..hidden":  "",
		"/d/.cfg.bak":  ".bak",
		"/d.x/noext":   "",
		"trailingdot.": ".",
	} {
		if got := pySplitExt(in); got != want {
			t.Errorf("pySplitExt(%q) = %q, want %q", in, got, want)
		}
	}
}

// The transfer is staged under remote_tmp (created 0700) like Ansible's.
func TestStagePayloadRemoteTmp(t *testing.T) {
	rt := filepath.Join(t.TempDir(), "rt")
	dir, report, src, err := stagePayload(nil, []byte("x"), ".txt", rt)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != rt || !strings.HasPrefix(filepath.Base(dir), "ansible-tmp-") ||
		src != filepath.Join(dir, ".source.txt") || report != src {
		t.Errorf("staged at %q (dir %q, reported %q), want %s/ansible-tmp-*/.source.txt", src, dir, report, rt)
	}
	if info, err := os.Stat(rt); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("remote_tmp mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
}

// Under become, remote_tmp's ~ is the login user's home.
func TestStagePayloadLoginHome(t *testing.T) {
	home := t.TempDir()
	dir, report, _, err := stagePayload(&RunEnv{LoginHome: home}, []byte("x"), "", "~/.ansible/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != filepath.Join(home, ".ansible", "tmp") || report != filepath.Join(dir, ".source") {
		t.Errorf("staged in %q (reported %q), want under %s/.ansible/tmp", dir, report, home)
	}
}

// For an unprivileged become user the login user's staging directory is
// reported, while the file is written where the module can.
func TestStagePayloadStageDir(t *testing.T) {
	dir, report, src, err := stagePayload(&RunEnv{StageDir: "/var/tmp/ansible-tmp-1.5-2-3"}, []byte("x"), ".txt", "~/.ansible/tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if report != "/var/tmp/ansible-tmp-1.5-2-3/.source.txt" {
		t.Errorf("reported %q", report)
	}
	if b, err := os.ReadFile(src); err != nil || string(b) != "x" {
		t.Errorf("staged file %q: %q, %v", src, b, err)
	}
}
