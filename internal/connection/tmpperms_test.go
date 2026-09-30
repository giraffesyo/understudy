package connection

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBecomeUnprivileged(t *testing.T) {
	sh := &ShellOptions{RemoteUser: "deploy"}
	for _, c := range []struct {
		spec *BecomeSpec
		want bool
	}{
		{nil, false},
		{&BecomeSpec{Shell: sh}, false},                 // root
		{&BecomeSpec{User: "toor", Shell: sh}, false},   // an admin user
		{&BecomeSpec{User: "deploy", Shell: sh}, false}, // the remote user
		{&BecomeSpec{User: "app", Shell: sh}, true},
		{&BecomeSpec{User: "app"}, false}, // no shell options: not managed
		{&BecomeSpec{User: "app", Shell: &ShellOptions{AdminUsers: []string{"app"}}}, false},
		{&BecomeSpec{User: "deploy", Shell: &ShellOptions{}}, true}, // remote_user unset
	} {
		if got := becomeUnprivileged(c.spec); got != c.want {
			t.Errorf("%+v: %v, want %v", c.spec, got, c.want)
		}
	}
}

// stubConn runs commands locally with stub tools first on PATH.
type stubConn struct {
	*Local
	bin string
}

func (s stubConn) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	return s.Local.Exec(ctx, "PATH="+ShellQuote(s.bin)+":$PATH; export PATH; "+cmd, opts)
}

// fixupStubs installs setfacl, chown, chgrp and chmod stand-ins: those
// named in ok succeed ("chmod a+rx": chmod takes that mode too), the
// rest fail the way GNU coreutils does.
func fixupStubs(t *testing.T, ok ...string) string {
	bin := t.TempDir()
	stub := map[string]string{
		"setfacl": "echo 'setfacl: not supported' >&2; exit 1",
		"chown":   "echo \"chown: changing ownership of '$2': Operation not permitted\" >&2; exit 1",
		"chgrp":   "echo \"chgrp: invalid group: '$1'\" >&2; exit 1",
		"chmod":   "case $1 in u+rwx) exit 0;; esac; echo \"chmod: invalid mode: '$1'\" >&2; echo \"Try 'chmod --help' for more information.\" >&2; exit 1",
	}
	for _, name := range ok {
		if name == "chmod a+rx" {
			stub["chmod"] = strings.Replace(stub["chmod"], "u+rwx)", "u+rwx|a+rx)", 1)
			continue
		}
		stub[name] = "exit 0"
	}
	for name, body := range stub {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func TestFixupPermsChain(t *testing.T) {
	ctx := context.Background()
	spec := func(sh ShellOptions) *BecomeSpec { return &BecomeSpec{User: "app", Shell: &sh} }
	paths := []string{"/var/tmp/ansible-tmp-1/", "/var/tmp/ansible-tmp-1/AnsiballZ_command.py"}

	// Nothing works: the error carries the last command's exact stderr.
	err := fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t)}, "", paths, spec(ShellOptions{RemoteUser: "deploy"}))
	want := "Failed to set permissions on the temporary files Ansible needs to create when becoming an unprivileged user " +
		"(rc: 1, err: chmod: invalid mode: 'A+user:app:rx:allow'\nTry 'chmod --help' for more information.\n}). " +
		"For information on working around this, see " + privilegeLink + "#risks-of-becoming-an-unprivileged-user"
	if err == nil || err.Error() != want {
		t.Errorf("no method:\n got %v\nwant %s", err, want)
	}

	// ACLs, then chown, are enough.
	for _, ok := range []string{"setfacl", "chown"} {
		if err := fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t, ok)}, "", paths, spec(ShellOptions{RemoteUser: "deploy"})); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}

	// A privileged remote user whose chown failed is an error.
	err = fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t)}, "", paths, spec(ShellOptions{RemoteUser: "root"}))
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to change ownership of the temporary files") {
		t.Errorf("admin: %v", err)
	}

	// A common group that chgrp rejects, then world-readable files with
	// its warning.
	var warned []string
	sh := ShellOptions{RemoteUser: "deploy", CommonRemoteGroup: "nogroup", Warn: func(m string) { warned = append(warned, m) }}
	err = fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t)}, "", paths, spec(sh))
	if err == nil || !strings.Contains(err.Error(), "err: chgrp: invalid group: 'nogroup'\n})") {
		t.Errorf("common group: %v", err)
	}
	sh.WorldReadableTemp = true
	if err := fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t, "chmod a+rx")}, "", paths, spec(sh)); err != nil {
		t.Errorf("world-readable: %v", err)
	}
	if len(warned) != 1 || !strings.HasPrefix(warned[0], "Using world-readable permissions") {
		t.Errorf("warnings: %q", warned)
	}

	// The prep command runs first; its failure stops the chain.
	err = fixupPerms(ctx, stubConn{NewLocal(), fixupStubs(t, "setfacl")}, "exit 3", paths, spec(ShellOptions{}))
	if err == nil || !strings.Contains(err.Error(), "rc: 3") {
		t.Errorf("prep: %v", err)
	}
}
