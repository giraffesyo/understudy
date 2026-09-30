package fsutil

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"syscall"
	"testing"
)

// fakeSELinux points the SELinux probes at a tree with selinuxfs,
// libselinux and a small policy, and keeps labels per inode (so a rename
// carries the source's label, as it does on disk).
func fakeSELinux(t *testing.T) map[uint64]string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"/etc/ld.so.cache":        "...libselinux.so.1...",
		"/sys/fs/selinux/enforce": "1",
		"/sys/fs/selinux/mls":     "1",
		"/etc/selinux/config":     "SELINUX=enforcing\nSELINUXTYPE=targeted\n",
		"/proc/mounts":            "/dev/root / ext4 rw 0 0\n",
		"/etc/selinux/targeted/contexts/files/file_contexts": `
/.*			system_u:object_r:default_t:s0
/srv(/.*)?		system_u:object_r:var_t:s0
/srv/keys(/.*)?		system_u:object_r:ssh_home_t:s0
/srv/keys/exact		system_u:object_r:etc_t:s0
/srv/none(/.*)?		<<none>>
/var/run(/.*)?		system_u:object_r:var_run_t:s0
/srv/dir	-d	system_u:object_r:etc_t:s0
`,
		"/etc/selinux/targeted/contexts/files/file_contexts.homedirs":  "/srv/home/[^/]+/\\.ssh(/.*)?\tunconfined_u:object_r:ssh_home_t:s0\n",
		"/etc/selinux/targeted/contexts/files/file_contexts.subs_dist": "/run /var/run\n",
	}
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldGet, oldSet, oldFS := seRoot, lgetfilecon, lsetfilecon, isSelinuxfs
	isSelinuxfs = func(path string) bool {
		_, err := os.Stat(path + "/enforce")
		return err == nil
	}
	labels := map[uint64]string{}
	ino := func(path string) (uint64, error) {
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			return 0, err
		}
		return uint64(st.Ino), nil
	}
	lgetfilecon = func(path string) (string, error) {
		i, err := ino(path)
		if err != nil {
			return "", err
		}
		if l, ok := labels[i]; ok {
			return l, nil
		}
		return "unconfined_u:object_r:user_tmp_t:s0", nil
	}
	lsetfilecon = func(path, con string) error {
		i, err := ino(path)
		if err != nil {
			return err
		}
		labels[i] = con
		return nil
	}
	reset := func(root string) {
		seRoot = root
		seOnce, fcOnce = sync.Once{}, sync.Once{}
		seEnabled, seMLS, fc = false, false, fcDB{}
	}
	reset(root)
	t.Cleanup(func() {
		lgetfilecon, lsetfilecon, isSelinuxfs = oldGet, oldSet, oldFS
		reset(oldRoot)
	})
	return labels
}

func TestMatchPathCon(t *testing.T) {
	fakeSELinux(t)
	for path, want := range map[string]string{
		"/srv/keys/exact":                      "system_u:object_r:etc_t:s0", // exact paths win
		"/srv/keys/other":                      "system_u:object_r:ssh_home_t:s0",
		"/srv/x":                               "system_u:object_r:var_t:s0",
		"/srv//keys/x":                         "system_u:object_r:ssh_home_t:s0",
		"/srv/home/alice/.ssh/authorized_keys": "unconfined_u:object_r:ssh_home_t:s0", // homedirs
		"/run/foo":                             "system_u:object_r:var_run_t:s0",      // subs_dist
		"/srv/none/x":                          "",
		"/srv/dir":                             "system_u:object_r:etc_t:s0", // mode 0 matches any type
		"/elsewhere":                           "system_u:object_r:default_t:s0",
	} {
		loadFileContexts()
		got, ok := fc.lookup(path)
		if !ok {
			got = ""
		}
		if got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	if !hasMetaChars(`/srv/a\.b(/.*)?`) || hasMetaChars(`/srv/a\.b`) {
		t.Error("hasMetaChars")
	}
}

func TestSetDefaultSELinuxContext(t *testing.T) {
	labels := fakeSELinux(t)
	if !SELinuxEnabled() {
		t.Fatal("fake SELinux not enabled")
	}
	// matchpathcon labels the realpath; point the policy at it.
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(seRoot, "/etc/selinux/targeted/contexts/files/file_contexts.local"),
		[]byte(regexp.QuoteMeta(dir)+"/keys(/.*)?\tsystem_u:object_r:ssh_home_t:s0\n"), 0o644)
	fcOnce, fc = sync.Once{}, fcDB{}
	f := filepath.Join(dir, "keys")
	os.WriteFile(f, nil, 0o600)

	changed, err := SetSELinuxContextIfDifferent(f, SELinuxDefaultContext(f), true)
	if err != nil || !changed || len(labels) != 0 {
		t.Fatalf("check mode: changed=%v err=%v labels=%v", changed, err, labels)
	}
	if err := SetDefaultSELinuxContext(f, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := lgetfilecon(f); got != "system_u:object_r:ssh_home_t:s0" {
		t.Errorf("label %q", got)
	}
	if changed, _ := SetSELinuxContextIfDifferent(f, SELinuxDefaultContext(f), false); changed {
		t.Error("relabeled an already-correct file")
	}
	// None parts keep the current value.
	changed, err = SetSELinuxContextIfDifferent(f, SEContext{"", "", "etc_t", ""}, false)
	if got, _ := lgetfilecon(f); err != nil || !changed || got != "system_u:object_r:etc_t:s0" {
		t.Errorf("partial: %q changed=%v err=%v", got, changed, err)
	}
}

// atomic_move keeps an existing destination's label, though the renamed
// file brings its own.
func TestAtomicMoveKeepsSELinuxLabel(t *testing.T) {
	fakeSELinux(t)
	dir := t.TempDir()
	dest, src := filepath.Join(dir, "authorized_keys"), filepath.Join(dir, "tmp")
	os.WriteFile(dest, []byte("old\n"), 0o600)
	lsetfilecon(dest, "system_u:object_r:ssh_home_t:s0")
	os.WriteFile(src, []byte("new\n"), 0o600)
	if err := AtomicMove(src, dest, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := lgetfilecon(dest); got != "system_u:object_r:ssh_home_t:s0" {
		t.Errorf("label %q", got)
	}
}

func TestSELinuxErrors(t *testing.T) {
	fakeSELinux(t)
	lsetfilecon = func(string, string) error { return syscall.EINVAL }
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	_, err := SetSELinuxContextIfDifferent(f, SEContext{"system_u", "object_r", "bogus_t", "s0"}, false)
	se, ok := err.(*SELinuxError)
	if !ok || se.Msg != "invalid selinux context: [Errno 22] Invalid argument" {
		t.Fatalf("err = %#v", err)
	}
	want := map[string]any{"path": f,
		"new_context": []any{"system_u", "object_r", "bogus_t", "s0"},
		"cur_context": []any{"unconfined_u", "object_r", "user_tmp_t", "s0"},
		"input_was":   []any{"system_u", "object_r", "bogus_t", "s0"}}
	if !reflect.DeepEqual(se.Extra, want) {
		t.Errorf("extra = %v", se.Extra)
	}
	if _, err := SELinuxContext(filepath.Join(t.TempDir(), "missing")); err == nil || err.Error() != "Failed to retrieve selinux context." {
		t.Errorf("missing file: %v", err)
	}
}
