package modules

import (
	"reflect"
	"strings"
	"testing"
)

func pkgOptsFor(t *testing.T, mgr string, raw map[string]any) pkgOpts {
	t.Helper()
	p, err := pkgSpec.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	o, err := pkgOptions(mgr, p, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPkgOptionsDnfParams(t *testing.T) {
	o := pkgOptsFor(t, "dnf", map[string]any{
		"name": []any{"x"}, "cacheonly": true, "best": false,
		"disable_plugin": []any{"a"}, "enable_plugin": []any{"b"},
		"download_only": true, "download_dir": "/tmp/d",
	})
	for _, want := range []string{"--cacheonly", "--disableplugin=a", "--enableplugin=b"} {
		if !containsStr(o.repo, want) {
			t.Errorf("repo opts %v lack %s", o.repo, want)
		}
	}
	for _, want := range []string{"--setopt=best=False", "--downloadonly", "--downloaddir=/tmp/d"} {
		if !containsStr(o.install, want) {
			t.Errorf("install opts %v lack %s", o.install, want)
		}
	}
	// nobest takes precedence over best.
	o = pkgOptsFor(t, "dnf", map[string]any{"name": []any{"x"}, "best": true, "nobest": true})
	if containsStr(o.install, "--setopt=best=True") || !containsStr(o.install, "--nobest") {
		t.Errorf("nobest precedence: %v", o.install)
	}
}

func TestPkgOptionsAptParams(t *testing.T) {
	o := pkgOptsFor(t, "apt", map[string]any{
		"name": []any{"x"}, "force": true, "fail_on_autoremove": true, "only_upgrade": true,
		"allow_unauthenticated": true, "allow_change_held_packages": true,
	})
	for _, want := range []string{"--force-yes", "--no-remove", "--only-upgrade", "--allow-unauthenticated", "--allow-change-held-packages"} {
		if !containsStr(o.install, want) {
			t.Errorf("install opts %v lack %s", o.install, want)
		}
	}
	for _, want := range []string{"--force-yes", "--allow-change-held-packages"} {
		if !containsStr(o.remove, want) {
			t.Errorf("remove opts %v lack %s", o.remove, want)
		}
	}
	p, _ := pkgSpec.Parse(map[string]any{"cacheonly": true})
	if _, err := pkgOptions("apt", p, map[string]any{"cacheonly": true}, nil); err == nil {
		t.Error("dnf-only option accepted by apt")
	}
	p, _ = pkgSpec.Parse(map[string]any{"policy_rc_d": 101})
	if _, err := pkgOptions("dnf", p, map[string]any{"policy_rc_d": 101}, nil); err == nil {
		t.Error("apt-only option accepted by dnf")
	}
}

func TestAptDpkgOptions(t *testing.T) {
	p, _ := pkgSpec.Parse(map[string]any{})
	want := `-o "Dpkg::Options::=--force-confdef" -o "Dpkg::Options::=--force-confold" -o DPkg::Lock::Timeout=60`
	if got := aptDpkgOptions(p); got != want {
		t.Errorf("got %s", got)
	}
	argv, err := shlexSplit("/usr/bin/apt-get -y " + want + "   upgrade --with-new-pkgs ")
	if err != nil || !reflect.DeepEqual(argv[len(argv)-3:], []string{"DPkg::Lock::Timeout=60", "upgrade", "--with-new-pkgs"}) {
		t.Errorf("split: %q %v", argv, err)
	}
}

func TestApkParsePackages(t *testing.T) {
	out := strings.Join([]string{
		"(1/3) Installing libcurl (8.5.0-r0)",
		"(2/3) Upgrading curl (8.4.0-r0 -> 8.5.0-r0)",
		"( 3/10) Purging foo (1.0-r0)",
		"OK: 10 MiB in 20 packages",
	}, "\n")
	got := apkParsePackages(out)
	want := []any{"libcurl", "curl", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestApkSpecRejectsAptOptions(t *testing.T) {
	if _, err := apkSpec.Parse(map[string]any{"name": []any{"x"}, "purge": true}); err == nil {
		t.Error("apk accepted an apt option")
	}
}
