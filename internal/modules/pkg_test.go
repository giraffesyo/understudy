package modules

import (
	"reflect"
	"strings"
	"testing"
)

func pkgOptsFor(t *testing.T, mgr string, raw map[string]any) pkgOpts {
	t.Helper()
	spec := dnfSpec
	if mgr == "apt" {
		spec = aptSpec
	}
	p, err := spec.Parse(raw)
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
	// Each module accepts only its own parameters, as AnsibleModule does.
	if _, fail := parseModuleArgs(aptSpec, map[string]any{"name": "x", "cacheonly": true}, "apt"); fail == nil ||
		!strings.HasPrefix(fail.Msg, "Unsupported parameters for (apt) module: cacheonly. Supported parameters include: allow_change_held_packages,") ||
		!strings.HasSuffix(fail.Msg, " (allow-downgrade, allow-downgrades, allow-unauthenticated, allow_downgrades, default-release, install-recommends, name, pkg, update-cache).") {
		t.Errorf("apt accepted a dnf option: %+v", fail)
	}
	if _, fail := parseModuleArgs(dnfSpec, map[string]any{"name": "x", "policy_rc_d": 101}, "ansible.legacy.dnf"); fail == nil ||
		fail.Msg != "Unsupported parameters for (ansible.legacy.dnf) module: policy_rc_d. Supported parameters include: "+
			"allow_downgrade, allowerasing, autoremove, best, bugfix, cacheonly, conf_file, disable_excludes, "+
			"disable_gpg_check, disable_plugin, disablerepo, download_dir, download_only, enable_plugin, enablerepo, "+
			"exclude, install_weak_deps, installroot, list, lock_timeout, name, nobest, releasever, security, "+
			"skip_broken, sslverify, state, update_cache, update_only, use_backend, validate_certs (expire-cache, pkg)." {
		t.Errorf("dnf accepted an apt option: %+v", fail)
	}
}

func TestAptDpkgOptions(t *testing.T) {
	p, _ := aptSpec.Parse(map[string]any{})
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
