package connection

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// applyBecome wraps a shell command for sudo when a BecomeSpec is present
// (su and doas need a terminal and go through execPTYBecome instead).
// Two deterministic modes:
//
//   - no password: sudo <flags> (default -H -S -n) — -n fails immediately
//     (recognizable stderr) when NOPASSWD is not configured, instead of
//     hanging on a prompt.
//   - password: sudo -k <flags without -n> -p ” — -k invalidates cached
//     credentials so sudo ALWAYS reads exactly one password line from
//     stdin; without it a cached credential would leave the password line
//     to be swallowed by the agent's JSON reader.
//
// become_exe replaces the sudo binary. The password line is prepended to
// the caller's stdin.
func applyBecome(cmd string, opts ExecOptions) (string, io.Reader) {
	if opts.Become == nil {
		return cmd, opts.Stdin
	}
	b := opts.Become
	quoted := ShellQuote(cmd)
	exe := b.exe()
	flags := "-H -S -n"
	if b.Flags != nil {
		flags = *b.Flags
	}
	if b.Password == "" {
		return joinWords(exe, flags, "-u", ShellQuote(b.user()), "/bin/sh -c", quoted), opts.Stdin
	}
	wrapped := joinWords(exe, "-k", removeWord(flags, "-n"), "-p ''", "-u", ShellQuote(b.user()), "/bin/sh -c", quoted)
	pw := strings.NewReader(b.Password + "\n")
	if opts.Stdin == nil {
		return wrapped, pw
	}
	return wrapped, io.MultiReader(pw, opts.Stdin)
}

func (b *BecomeSpec) user() string {
	if b.User == "" {
		return "root"
	}
	return b.User
}

func (b *BecomeSpec) method() string {
	if b.Method == "" {
		return "sudo"
	}
	return b.Method
}

func (b *BecomeSpec) exe() string {
	if b.Exe != "" {
		return b.Exe
	}
	return b.method()
}

// needsPTY reports whether the method prompts on a terminal (su, doas).
func (b *BecomeSpec) needsPTY() bool {
	return b != nil && (b.method() == "su" || b.method() == "doas")
}

// joinWords joins non-empty command fragments with single spaces.
func joinWords(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func removeWord(s, word string) string {
	var out []string
	for _, f := range strings.Fields(s) {
		if f != word {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// ptyBecomeCommand builds the su / doas command line running inner (a
// shell command) as the become user, the way Ansible's su and
// community.general.doas become plugins do:
//
//	su <flags> <user> -c '<inner>'
//	doas <flags> [-n] -u <user> /bin/sh -c '<inner>'
//
// doas gets -n when there is no password, so it fails instead of
// prompting.
func ptyBecomeCommand(b *BecomeSpec, inner string) string {
	flags := ""
	if b.Flags != nil {
		flags = *b.Flags
	}
	switch b.method() {
	case "su":
		return joinWords(b.exe(), flags, ShellQuote(b.user()), "-c", ShellQuote(inner))
	case "doas":
		if b.Password == "" && !strings.Contains(flags, "-n") {
			flags = joinWords(flags, "-n")
		}
		return joinWords(b.exe(), flags, "-u", ShellQuote(b.user()), "/bin/sh -c", ShellQuote(inner))
	}
	return inner
}

// suPrompt is the su plugin's check_password_prompt (its localized
// prompts, then optional space and a colon).
var suPrompt = regexp.MustCompile(`(?i)(?:Password|암호|パスワード|Adgangskode|Contraseña|Contrasenya|Hasło|Heslo|Jelszó|Lösenord|Mật khẩu|Mot de passe|Parola|Parool|Pasahitza|Passord|Passwort|Salasana|Sandi|Senha|Wachtwoord|ססמה|Лозинка|Парола|Пароль|गुप्तशब्द|शब्दकूट|సంకేతపదము|හස්පදය|密码|密碼|口令)\s*[:：]`)

// doasPrompt is the doas plugin's prompt check (re.match: at the start).
var doasPrompt = regexp.MustCompile(`^(?:doas \(|Password:)`)

func promptSeen(method string, out []byte) bool {
	if method == "doas" {
		return doasPrompt.Match(bytes.TrimLeft(out, "\r\n"))
	}
	return suPrompt.Match(out)
}

// becomeFailed / becomeMissing are the plugins' fail and missing strings.
func becomeFailed(method string, out []byte) bool {
	switch method {
	case "su":
		return bytes.Contains(out, []byte("Authentication failure"))
	case "doas":
		// OpenBSD doas, then OpenDoas (Linux) wording.
		return bytes.Contains(out, []byte("Permission denied")) || bytes.Contains(out, []byte("Authentication failed"))
	}
	return false
}

func becomeMissing(method string, out []byte) bool {
	return method == "doas" && (bytes.Contains(out, []byte("Authorization required")) || bytes.Contains(out, []byte("Authentication required")))
}

// becomeTimeout is how long to wait for a prompt or success marker
// (ansible-core's default timeout of 10s plus its 2s become grace).
var becomeTimeout = 12 * time.Second

// ptyProcess is a command running on a pseudo-terminal: reads return the
// terminal's output, writes type into it.
type ptyProcess interface {
	io.ReadWriter
	Wait() error // the process exit; a non-nil error for a non-zero exit
	Kill()
}

// plainExec runs a shell command without become.
type plainExec func(ctx context.Context, cmd string, stdin io.Reader) (ExecResult, error)

// startPTY starts a shell command on a pseudo-terminal.
type startPTY func(ctx context.Context, cmd string) (ptyProcess, error)

// execPTYBecome runs cmd as the become user with su or doas, which read
// passwords from a terminal. The command's stdin, stdout, stderr and exit
// status travel through files in a private temporary directory (the
// terminal only carries the password dialog): stdin is staged before, the
// results are read back after, both as the connecting user.
func execPTYBecome(ctx context.Context, plain plainExec, start startPTY, cmd string, opts ExecOptions) (ExecResult, error) {
	b := opts.Become
	res, err := plain(ctx, `umask 077 && mktemp -d "${TMPDIR:-/tmp}/understudy-become-XXXXXXXX"`, nil)
	if err != nil {
		return ExecResult{}, err
	}
	dir := strings.TrimSpace(string(res.Stdout))
	if res.RC != 0 || dir == "" {
		return ExecResult{}, fmt.Errorf("creating a temporary directory for %s failed: %s", b.method(), strings.TrimSpace(string(res.Stderr)))
	}
	defer plain(context.WithoutCancel(ctx), "rm -rf "+ShellQuote(dir), nil)
	in, out, errf, rc := dir+"/in", dir+"/out", dir+"/err", dir+"/rc"
	stdin := opts.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stage := fmt.Sprintf("umask 077 && cat > %s && : > %s && : > %s && : > %s",
		ShellQuote(in), ShellQuote(out), ShellQuote(errf), ShellQuote(rc))
	if u := b.user(); u != "root" && u != "0" {
		// An unprivileged become user needs access to the files; like
		// Ansible, grant it with POSIX ACLs rather than world access.
		stage += fmt.Sprintf(" && setfacl -m u:%s:x %s && setfacl -m u:%s:r %s && setfacl -m u:%s:rw %s %s %s",
			ShellQuote(u), ShellQuote(dir), ShellQuote(u), ShellQuote(in), ShellQuote(u), ShellQuote(out), ShellQuote(errf), ShellQuote(rc))
	}
	if res, err = plain(ctx, stage, stdin); err != nil {
		return ExecResult{}, err
	}
	if res.RC != 0 {
		return ExecResult{}, fmt.Errorf("Failed to set permissions on the temporary files understudy needs to create when becoming an unprivileged user (%s): %s",
			b.user(), strings.TrimSpace(string(res.Stderr)))
	}

	marker := "BECOME-SUCCESS-" + randomHex(16)
	inner := fmt.Sprintf("echo %s; /bin/sh -c %s <%s >%s 2>%s; echo $? >%s",
		marker, ShellQuote(cmd), ShellQuote(in), ShellQuote(out), ShellQuote(errf), ShellQuote(rc))
	if err := runPTYDialog(ctx, start, b, ptyBecomeCommand(b, inner), marker); err != nil {
		return ExecResult{}, err
	}

	outRes, err := plain(ctx, "cat "+ShellQuote(out), nil)
	if err != nil {
		return ExecResult{}, err
	}
	errRes, err := plain(ctx, "cat "+ShellQuote(errf), nil)
	if err != nil {
		return ExecResult{}, err
	}
	rcRes, err := plain(ctx, "cat "+ShellQuote(rc), nil)
	if err != nil {
		return ExecResult{}, err
	}
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(rcRes.Stdout)), "%d", &code); err != nil {
		return ExecResult{}, fmt.Errorf("%s: the command's exit status is missing", b.method())
	}
	return ExecResult{RC: code, Stdout: outRes.Stdout, Stderr: errRes.Stdout}, nil
}

// runPTYDialog runs the become command on a terminal, answering the
// password prompt, until it exits. It fails like Ansible's connection
// plugins: "Missing <method> password", "Incorrect <method> password",
// or a timeout waiting for the prompt; a command that exits without
// printing the success marker failed to escalate.
func runPTYDialog(ctx context.Context, start startPTY, b *BecomeSpec, command, marker string) error {
	proc, err := start(ctx, command)
	if err != nil {
		return err
	}
	var (
		mu     sync.Mutex
		buf    []byte
		update = make(chan struct{}, 1)
		done   = make(chan struct{})
	)
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, rerr := proc.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf = append(buf, chunk[:n]...)
				mu.Unlock()
				select {
				case update <- struct{}{}:
				default:
				}
			}
			if rerr != nil {
				close(done)
				return
			}
		}
	}()
	scrub := func(s string) string {
		s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
		if b.Password != "" {
			s = strings.ReplaceAll(s, b.Password, "********")
		}
		return s
	}
	method := b.method()
	timer := time.NewTimer(becomeTimeout)
	defer timer.Stop()
	sent, succeeded := false, false
	fail := func(err error) error {
		proc.Kill()
		<-done
		proc.Wait()
		return err
	}
	for !succeeded {
		mu.Lock()
		out := append([]byte(nil), buf...)
		mu.Unlock()
		switch {
		case bytes.Contains(out, []byte(marker)):
			succeeded = true
			continue
		case becomeMissing(method, out):
			return fail(&BecomeError{Msg: fmt.Sprintf("Missing %s password", method)})
		case sent && becomeFailed(method, out):
			return fail(&BecomeError{Msg: fmt.Sprintf("Incorrect %s password", method)})
		case !sent && promptSeen(method, out):
			if b.Password == "" {
				return fail(&BecomeError{Msg: fmt.Sprintf("Missing %s password", method)})
			}
			mu.Lock()
			buf = buf[len(out):]
			mu.Unlock()
			if _, err := io.WriteString(proc, b.Password+"\n"); err != nil {
				return fail(err)
			}
			sent = true
			timer.Reset(becomeTimeout)
			continue
		}
		select {
		case <-update:
		case <-done:
			mu.Lock()
			out = append([]byte(nil), buf...)
			mu.Unlock()
			if bytes.Contains(out, []byte(marker)) {
				succeeded = true
				continue
			}
			proc.Wait()
			switch {
			case becomeMissing(method, out):
				return &BecomeError{Msg: fmt.Sprintf("Missing %s password", method)}
			case becomeFailed(method, out) && sent:
				return &BecomeError{Msg: fmt.Sprintf("Incorrect %s password", method)}
			case promptSeen(method, out) && !sent:
				return &BecomeError{Msg: fmt.Sprintf("Missing %s password", method)}
			}
			return fmt.Errorf("privilege escalation with %s failed: %s", method, scrub(string(out)))
		case <-timer.C:
			mu.Lock()
			out = append([]byte(nil), buf...)
			mu.Unlock()
			return fail(fmt.Errorf("Timeout (%ds) waiting for privilege escalation prompt: %s", int(becomeTimeout/time.Second), scrub(string(out))))
		case <-ctx.Done():
			return fail(ctx.Err())
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	proc.Wait() // the command's own status is in the rc file
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ShellQuote wraps s in single quotes, POSIX-escaping embedded quotes.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// IsSudoPasswordError recognizes sudo -n's refusal on stderr.
func IsSudoPasswordError(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "a password is required") ||
		strings.Contains(s, "password is required") ||
		strings.Contains(s, "sudo: a terminal is required")
}
