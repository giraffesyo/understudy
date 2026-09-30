package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysctl puts a sysctl script on PATH that logs its argv and behaves
// like procps-ng for one key (current value in $dir/value).
func fakeSysctl(t *testing.T, value string, script string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho \"$*\" >> " + dir + "/log\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sysctl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func readLog(t *testing.T, dir string) []string {
	data, _ := os.ReadFile(filepath.Join(dir, "log"))
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestSysctlSetFailureCarriesSysctlMessage(t *testing.T) {
	dir := fakeSysctl(t, "15000\n", `
case "$1" in
  -e) cat "$(dirname "$0")/value" ;;
  -w) echo 'sysctl: permission denied on key "user.max_user_namespaces"' >&2; exit 1 ;;
esac`)
	conf := filepath.Join(dir, "sysctl.conf")
	res := sysctlModule(&RunEnv{}, map[string]any{
		"name": "user.max_user_namespaces", "value": 0, "sysctl_set": true, "reload": false,
		"sysctl_file": conf,
	})
	want := "setting user.max_user_namespaces failed: sysctl: permission denied on key \"user.max_user_namespaces\"\n"
	if !res.Failed || res.Msg != want {
		t.Fatalf("result = %+v, want msg %q", res, want)
	}
	log := readLog(t, dir)
	if len(log) != 2 || log[0] != "-e -n user.max_user_namespaces" || log[1] != "-w user.max_user_namespaces=0" {
		t.Fatalf("sysctl invocations = %q", log)
	}
}

func TestSysctlWritesFileAndReloads(t *testing.T) {
	dir := fakeSysctl(t, "0\n", `
case "$1" in
  -e) cat "$(dirname "$0")/value" ;;
esac`)
	conf := filepath.Join(dir, "sysctl.conf")
	os.WriteFile(conf, []byte("# comment\n  net.ipv4.ip_forward = 0\nkernel.x=1\nkernel.x=2\nbad line\n"), 0o644)
	res := sysctlModule(&RunEnv{}, map[string]any{
		"name": "net.ipv4.ip_forward", "value": "yes", "sysctl_file": conf, "ignoreerrors": true,
	})
	if res.Failed || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	got, _ := os.ReadFile(conf)
	if want := "# comment\nnet.ipv4.ip_forward=1\nkernel.x=1\nbad line\n"; string(got) != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
	log := readLog(t, dir)
	if log[len(log)-1] != "-e -p "+conf {
		t.Fatalf("reload = %q", log)
	}

	// Steady state: file and live value agree, nothing runs but the read.
	os.WriteFile(filepath.Join(dir, "value"), []byte("1\n"), 0o644)
	os.Remove(filepath.Join(dir, "log"))
	res = sysctlModule(&RunEnv{}, map[string]any{"name": "net.ipv4.ip_forward", "value": "1", "sysctl_file": conf})
	if res.Failed || res.Changed {
		t.Fatalf("second run = %+v", res)
	}
	if log := readLog(t, dir); len(log) != 1 {
		t.Fatalf("second run invocations = %q", log)
	}
}

func TestSysctlReloadFailure(t *testing.T) {
	dir := fakeSysctl(t, "1\n", `
case "$1" in
  -e) cat "$(dirname "$0")/value" ;;
  -p) echo 'sysctl: setting key "vm.swappiness": Read-only file system' >&2 ;;
esac`)
	conf := filepath.Join(dir, "sysctl.conf")
	res := sysctlModule(&RunEnv{}, map[string]any{"name": "vm.swappiness", "value": "10", "sysctl_file": conf})
	if !res.Failed || res.Msg != "Failed to reload sysctl: sysctl: setting key \"vm.swappiness\": Read-only file system\n" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSysctlCheckModeAndAbsent(t *testing.T) {
	dir := fakeSysctl(t, "1\n", `case "$1" in -e) cat "$(dirname "$0")/value" ;; esac`)
	conf := filepath.Join(dir, "sysctl.conf")
	os.WriteFile(conf, []byte("vm.a=1\n"), 0o644)
	res := sysctlModule(&RunEnv{CheckMode: true}, map[string]any{"name": "vm.a", "state": "absent", "sysctl_file": conf})
	if !res.Changed {
		t.Fatalf("check mode absent = %+v", res)
	}
	if got, _ := os.ReadFile(conf); string(got) != "vm.a=1\n" {
		t.Fatalf("check mode wrote %q", got)
	}
	res = sysctlModule(&RunEnv{}, map[string]any{"name": "vm.a", "state": "absent", "sysctl_file": conf, "reload": false})
	if got, _ := os.ReadFile(conf); !res.Changed || string(got) != "" {
		t.Fatalf("absent = %+v, file %q", res, got)
	}
	res = sysctlModule(&RunEnv{}, map[string]any{"name": "vm.a", "sysctl_file": conf})
	if !res.Failed || res.Msg != "state is present but all of the following are missing: value" {
		t.Fatalf("missing value = %+v", res)
	}
}
