package connection

import (
	"io"
	"strings"
	"testing"
)

func TestParseUname(t *testing.T) {
	cases := []struct {
		out        string
		goos, arch string
		wantErr    string
	}{
		{"Linux x86_64", "linux", "amd64", ""},
		{"Linux aarch64", "linux", "arm64", ""},
		{"Linux arm64", "linux", "arm64", ""},
		{"Darwin arm64", "", "", "not supported"},
		{"Linux armv7l", "", "", "not supported"},
		{"garbage", "", "", "unexpected"},
	}
	for _, c := range cases {
		goos, arch, err := parseUname(c.out)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parseUname(%q) err = %v, want %q", c.out, err, c.wantErr)
			}
			continue
		}
		if err != nil || goos != c.goos || arch != c.arch {
			t.Errorf("parseUname(%q) = %s/%s, %v", c.out, goos, arch, err)
		}
	}
}

func TestBecomeWrapping(t *testing.T) {
	// No become: command passes through untouched.
	cmd, stdin := applyBecome("echo hi", ExecOptions{})
	if cmd != "echo hi" || stdin != nil {
		t.Errorf("no-become: %q, %v", cmd, stdin)
	}

	// NOPASSWD mode uses -n (fail fast, never prompt).
	cmd, _ = applyBecome("echo hi", ExecOptions{Become: &BecomeSpec{User: "root"}})
	if !strings.Contains(cmd, "sudo -H -S -n -u 'root'") || !strings.Contains(cmd, `'echo hi'`) {
		t.Errorf("nopasswd wrap: %q", cmd)
	}

	// Password mode uses -k (always consume exactly one stdin line).
	cmd, stdin = applyBecome("echo hi", ExecOptions{Become: &BecomeSpec{User: "deploy", Password: "s3cret"}})
	if !strings.Contains(cmd, "sudo -k -H -S -p '' -u 'deploy'") {
		t.Errorf("password wrap: %q", cmd)
	}
	data, _ := io.ReadAll(stdin)
	if string(data) != "s3cret\n" {
		t.Errorf("password stdin = %q", data)
	}

	// Password prepends to existing stdin (the agent's JSON frame).
	cmd, stdin = applyBecome("agent run", ExecOptions{
		Become: &BecomeSpec{User: "root", Password: "pw"},
		Stdin:  strings.NewReader(`{"proto":1}` + "\n"),
	})
	data, _ = io.ReadAll(stdin)
	if string(data) != "pw\n{\"proto\":1}\n" {
		t.Errorf("password+frame stdin = %q", data)
	}
	_ = cmd
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":        "'simple'",
		"has space":     "'has space'",
		"it's":          `'it'"'"'s'`,
		"":              "''",
		"$HOME; rm -rf": `'$HOME; rm -rf'`,
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSudoPasswordErrorDetection(t *testing.T) {
	if !IsSudoPasswordError([]byte("sudo: a password is required\n")) {
		t.Error("should detect password-required")
	}
	if IsSudoPasswordError([]byte("some other error")) {
		t.Error("false positive")
	}
}
