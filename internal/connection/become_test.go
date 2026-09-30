package connection

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func TestSudoFlagsAndExe(t *testing.T) {
	cmd, _ := applyBecome("id", ExecOptions{Become: &BecomeSpec{User: "root", Exe: "/usr/local/bin/sudo", Flags: strp("-E -S -n")}})
	if cmd != "/usr/local/bin/sudo -E -S -n -u 'root' /bin/sh -c 'id'" {
		t.Errorf("sudo exe/flags: %q", cmd)
	}
	cmd, _ = applyBecome("id", ExecOptions{Become: &BecomeSpec{User: "app", Password: "pw", Flags: strp("-H -S -n -E")}})
	if cmd != "sudo -k -H -S -E -p '' -u 'app' /bin/sh -c 'id'" {
		t.Errorf("sudo password drops -n: %q", cmd)
	}
	cmd, _ = applyBecome("id", ExecOptions{Become: &BecomeSpec{Flags: strp("")}})
	if cmd != "sudo -u 'root' /bin/sh -c 'id'" {
		t.Errorf("empty flags: %q", cmd)
	}
}

func TestPTYBecomeCommands(t *testing.T) {
	cases := []struct {
		spec BecomeSpec
		want string
	}{
		{BecomeSpec{Method: "su"}, `su 'root' -c 'echo hi'`},
		{BecomeSpec{Method: "su", User: "app", Flags: strp("-l"), Exe: "/bin/su"}, `/bin/su -l 'app' -c 'echo hi'`},
		{BecomeSpec{Method: "doas"}, `doas -n -u 'root' /bin/sh -c 'echo hi'`},
		{BecomeSpec{Method: "doas", Password: "pw"}, `doas -u 'root' /bin/sh -c 'echo hi'`},
		{BecomeSpec{Method: "doas", User: "app", Flags: strp("-n"), Exe: "/usr/bin/doas"}, `/usr/bin/doas -n -u 'app' /bin/sh -c 'echo hi'`},
	}
	for _, c := range cases {
		if got := ptyBecomeCommand(&c.spec, "echo hi"); got != c.want {
			t.Errorf("%+v: %q, want %q", c.spec, got, c.want)
		}
		if !c.spec.needsPTY() {
			t.Errorf("%s should need a pty", c.spec.Method)
		}
	}
	if (&BecomeSpec{}).needsPTY() || (*BecomeSpec)(nil).needsPTY() {
		t.Error("sudo / no become must not use a pty")
	}
}

func TestPromptDetection(t *testing.T) {
	for _, s := range []string{"Password: ", "Passwort:", "root's Password :", "Mot de passe : ", "密码："} {
		if !promptSeen("su", []byte(s)) {
			t.Errorf("su prompt %q not detected", s)
		}
	}
	if promptSeen("su", []byte("BECOME-SUCCESS-x")) {
		t.Error("marker taken for a prompt")
	}
	if !promptSeen("doas", []byte("doas (me@host) password: ")) || promptSeen("doas", []byte("hello doas (")) {
		t.Error("doas prompt must match at the start only")
	}
}

// fakeSu writes a su look-alike: prompts on the terminal when a password
// is expected, then runs the -c command (su [flags] user -c cmd).
func fakeSu(t *testing.T, password string) string {
	t.Helper()
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminals")
	}
	script := `#!/bin/sh
[ -t 0 ] || { echo "su: must be run from a terminal" >&2; exit 1; }
while [ "$1" != "-c" ]; do shift; done
if [ -n "` + password + `" ]; then
  printf 'Password: '
  read -r pw
  if [ "$pw" != "` + password + `" ]; then echo; echo "su: Authentication failure"; exit 1; fi
fi
exec /bin/sh -c "$2"
`
	path := filepath.Join(t.TempDir(), "su")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalSuDialog(t *testing.T) {
	l := NewLocal()
	ctx := context.Background()

	exe := fakeSu(t, "s3cret")
	res, err := l.Exec(ctx, "cat; echo err >&2; exit 3", ExecOptions{
		Stdin:  strings.NewReader("frame\x00bytes\n"),
		Become: &BecomeSpec{Method: "su", Exe: exe, Password: "s3cret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 3 || string(res.Stdout) != "frame\x00bytes\n" || string(res.Stderr) != "err\n" {
		t.Fatalf("result = rc %d, stdout %q, stderr %q", res.RC, res.Stdout, res.Stderr)
	}

	_, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Method: "su", Exe: exe, Password: "wrong"}})
	if err == nil || err.Error() != "Incorrect su password" {
		t.Fatalf("wrong password: %v", err)
	}
	_, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Method: "su", Exe: exe}})
	if err == nil || err.Error() != "Missing su password" {
		t.Fatalf("no password: %v", err)
	}

	// No prompt at all (root su): straight to the command.
	res, err = l.Exec(ctx, "echo ok", ExecOptions{Become: &BecomeSpec{Method: "su", Exe: fakeSu(t, "")}})
	if err != nil || res.RC != 0 || string(res.Stdout) != "ok\n" {
		t.Fatalf("passwordless su: %+v, %v", res, err)
	}
}

func TestLocalDoasDialog(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminals")
	}
	script := `#!/bin/sh
nopass=
while [ "$1" != "/bin/sh" ]; do [ "$1" = -n ] && nopass=1; shift; done
if [ -n "$nopass" ]; then echo "doas: Authorization required" >&2; exit 1; fi
printf 'doas (me@host) password: '
read -r pw
[ "$pw" = pw ] || { echo "doas: Permission denied" >&2; exit 1; }
shift; shift
exec /bin/sh -c "$1"
`
	exe := filepath.Join(t.TempDir(), "doas")
	os.WriteFile(exe, []byte(script), 0o755)
	l := NewLocal()
	ctx := context.Background()
	res, err := l.Exec(ctx, "id -u >/dev/null && echo ran", ExecOptions{Become: &BecomeSpec{Method: "doas", Exe: exe, Password: "pw"}})
	if err != nil || string(res.Stdout) != "ran\n" {
		t.Fatalf("doas: %+v, %v", res, err)
	}
	if _, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Method: "doas", Exe: exe}}); err == nil || err.Error() != "Missing doas password" {
		t.Fatalf("doas -n: %v", err)
	}
	if _, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Method: "doas", Exe: exe, Password: "bad"}}); err == nil || err.Error() != "Incorrect doas password" {
		t.Fatalf("doas bad password: %v", err)
	}
}

func TestPTYBecomeTimeout(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no pseudo-terminals")
	}
	old := becomeTimeout
	becomeTimeout = 300 * time.Millisecond
	defer func() { becomeTimeout = old }()
	exe := filepath.Join(t.TempDir(), "su")
	os.WriteFile(exe, []byte("#!/bin/sh\necho thinking\nsleep 5\n"), 0o755)
	_, err := NewLocal().Exec(context.Background(), "true", ExecOptions{Become: &BecomeSpec{Method: "su", Exe: exe}})
	if err == nil || err.Error() != "Timeout (0s) waiting for privilege escalation prompt: thinking" {
		t.Fatalf("timeout: %v", err)
	}
}
