package connection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSudoCommand(t *testing.T) {
	id := strings.Repeat("k", 32)
	line, success, prompt := sudoCommand(&BecomeSpec{}, "id", id)
	if line != "sudo -H -S -n  -u 'root' /bin/sh -c 'echo BECOME-SUCCESS-"+id+" ; id'" || prompt != "" || success != "BECOME-SUCCESS-"+id {
		t.Errorf("no password: %q %q", line, prompt)
	}
	line, _, prompt = sudoCommand(&BecomeSpec{User: "app", Password: "pw", Exe: "/opt/sudo", Flags: strp("-H -S -n -En --preserve-env")}, "id", id)
	want := `/opt/sudo -H -S -E --preserve-env -p "[sudo via ansible, key=` + id + `] password:" -u 'app' /bin/sh -c 'echo BECOME-SUCCESS-` + id + ` ; id'`
	if line != want || prompt != "[sudo via ansible, key="+id+"] password:" {
		t.Errorf("password drops -n:\n got %q\nwant %q", line, want)
	}
	if id := becomeID(); !regexp.MustCompile(`^[a-z]{32}$`).MatchString(id) {
		t.Errorf("become id %q", id)
	}
}

// fakeSudo writes a sudo look-alike: skips options, prompts (-p) and
// checks the password on stdin, then runs the command.
func fakeSudo(t *testing.T, body string) string {
	t.Helper()
	script := `#!/bin/sh
prompt=
while [ $# -gt 0 ]; do
  case $1 in -p) prompt=$2; shift 2 ;; -u) shift 2 ;; -*) shift ;; *) break ;; esac
done
` + body + `
if [ -n "$prompt" ]; then
  printf '%s' "$prompt" >&2
  read -r pw
  [ "$pw" = s3cret ] || { echo "Sorry, try again." >&2; exit 1; }
fi
exec "$@"
`
	path := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalSudo(t *testing.T) {
	l := NewLocal()
	ctx := context.Background()
	exe := fakeSudo(t, "")

	for _, pw := range []string{"", "s3cret"} {
		res, err := l.Exec(ctx, "cat; echo err >&2; exit 3", ExecOptions{
			Stdin:  strings.NewReader("frame\x00bytes\n"),
			Become: &BecomeSpec{Exe: exe, Password: pw},
		})
		if err != nil {
			t.Fatalf("password %q: %v", pw, err)
		}
		if res.RC != 3 || string(res.Stdout) != "frame\x00bytes\n" || string(res.Stderr) != "err\n" {
			t.Fatalf("password %q: rc %d, stdout %q, stderr %q", pw, res.RC, res.Stdout, res.Stderr)
		}
	}

	_, err := l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Exe: exe, Password: "wrong"}})
	var be *BecomeError
	if !errors.As(err, &be) || be.Unreachable ||
		!regexp.MustCompile(`^Premature end of stream waiting for become success\.\n>>> Standard Error\n\[sudo via ansible, key=[a-z]{32}\] password:Sorry, try again\.$`).MatchString(be.Msg) {
		t.Fatalf("wrong password: %v", err)
	}

	refuse := fakeSudo(t, `echo out; echo "sudo: a password is required" >&2; exit 1`)
	_, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Exe: refuse}})
	if !errors.As(err, &be) || be.Msg != "Premature end of stream waiting for become success.\n>>> Standard Output\nout\n\n>>> Standard Error\nsudo: a password is required" {
		t.Fatalf("refused: %v", err)
	}

	// Never escalating: a connection failure, with "or become password
	// prompt" while a prompt is still expected.
	hang := fakeSudo(t, "sleep 5")
	start := time.Now()
	_, err = l.Exec(ctx, "true", ExecOptions{Become: &BecomeSpec{Exe: hang, Password: "x", SuccessTimeout: 200 * time.Millisecond}})
	if !errors.As(err, &be) || !be.Unreachable || be.Msg != "Timed out waiting for become success or become password prompt." {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
}
