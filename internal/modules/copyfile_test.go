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
	dir, src, err := stagePayload([]byte("x"), ".txt", rt)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != rt || !strings.HasPrefix(filepath.Base(dir), "ansible-tmp-") ||
		src != filepath.Join(dir, ".source.txt") {
		t.Errorf("staged at %q (dir %q), want %s/ansible-tmp-*/.source.txt", src, dir, rt)
	}
	if info, err := os.Stat(rt); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("remote_tmp mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
}
