package modules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// Expected contents were produced by ansible-core 2.21's yum_repository
// and community.general's sudoers.

func TestYumRepositoryRendering(t *testing.T) {
	dir := t.TempDir()
	run := func(a map[string]any) *agentproto.Result {
		a["reposdir"] = dir
		return yumRepositoryModule(&RunEnv{}, a)
	}
	res := run(map[string]any{"name": "epel", "description": "EPEL YUM repo",
		"baseurl": []any{"https://a.example/x", "https://b.example/y"}, "gpgcheck": true,
		"enabled": false, "exclude": []any{"kernel*", "foo"}, "cost": int64(5), "password": "sekrit"})
	if res.Failed || !res.Changed {
		t.Fatalf("first: %+v", res)
	}
	diff := res.Diff.(map[string]any)
	if diff["after"] != "[epel]\nbaseurl = https://a.example/x\nhttps://b.example/y\ncost = 5\nenabled = 0\nexclude = kernel* foo\ngpgcheck = 1\nname = EPEL YUM repo\npassword = ********\n\n" {
		t.Errorf("after = %q", diff["after"])
	}
	res = run(map[string]any{"name": "epel-src", "file": "epel", "description": "src",
		"mirrorlist": "https://m.example/", "keepalive": true})
	if len(res.Extra["deprecations"].([]any)) != 1 {
		t.Errorf("want the keepalive deprecation: %+v", res.Extra)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "epel.repo"))
	want := "[epel]\nbaseurl = https://a.example/x\n\thttps://b.example/y\ncost = 5\nenabled = 0\nexclude = kernel* foo\n" +
		"gpgcheck = 1\nname = EPEL YUM repo\npassword = sekrit\n\n[epel-src]\nkeepalive = 1\nmirrorlist = https://m.example/\nname = src\n\n"
	if string(got) != want {
		t.Errorf("file =\n%q\nwant\n%q", got, want)
	}
	// Re-reading the written file (continuation lines) is a no-op.
	if res := run(map[string]any{"name": "epel-src", "file": "epel", "description": "src",
		"mirrorlist": "https://m.example/", "keepalive": true}); res.Changed {
		t.Error("second run changed")
	}
	if res := run(map[string]any{"name": "x", "baseurl": "http://x"}); res.Msg != "state is present but all of the following are missing: description" {
		t.Errorf("msg = %q", res.Msg)
	}
	run(map[string]any{"name": "epel", "file": "epel", "state": "absent"})
	run(map[string]any{"name": "epel-src", "file": "epel", "state": "absent"})
	if pathExists(filepath.Join(dir, "epel.repo")) {
		t.Error("empty repo file not removed")
	}
}

func TestSudoersContent(t *testing.T) {
	p, err := sudoersSpec.Parse(map[string]any{"name": "ops", "group": "operators", "host": "web1", "runas": "alice",
		"commands": []any{"/bin/ls", "/usr/bin/less"}, "noexec": true, "setenv": true,
		"defaults": []any{"!targetpw", "env_keep += FOO"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sudoersContent(p)
	want := "Defaults:%operators !targetpw\nDefaults:%operators env_keep += FOO\n%operators web1=(alice)NOEXEC:NOPASSWD:SETENV: /bin/ls, /usr/bin/less\n"
	if got != want {
		t.Errorf("got %q", got)
	}
	p, _ = sudoersSpec.Parse(map[string]any{"name": "bob", "user": "bob", "commands": "ALL"})
	if got, _ := sudoersContent(p); got != "bob ALL=NOPASSWD: ALL\n" {
		t.Errorf("got %q", got)
	}
}
