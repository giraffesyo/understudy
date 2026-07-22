package connection

import (
	"io"
	"strings"
)

// applyBecome wraps a shell command in sudo when a BecomeSpec is present.
// Two deterministic modes:
//
//   - no password: sudo -H -S -n — fails immediately (recognizable stderr)
//     when NOPASSWD is not configured, instead of hanging on a prompt.
//   - password: sudo -k -H -S -p ” — -k invalidates cached credentials so
//     sudo ALWAYS reads exactly one password line from stdin; without it a
//     cached credential would leave the password line to be swallowed by
//     the agent's JSON reader.
//
// The password line is prepended to the caller's stdin.
func applyBecome(cmd string, opts ExecOptions) (string, io.Reader) {
	if opts.Become == nil {
		return cmd, opts.Stdin
	}
	b := opts.Become
	user := b.User
	if user == "" {
		user = "root"
	}
	quoted := ShellQuote(cmd)
	if b.Password == "" {
		return "sudo -H -S -n -u " + ShellQuote(user) + " /bin/sh -c " + quoted, opts.Stdin
	}
	wrapped := "sudo -k -H -S -p '' -u " + ShellQuote(user) + " /bin/sh -c " + quoted
	pw := strings.NewReader(b.Password + "\n")
	if opts.Stdin == nil {
		return wrapped, pw
	}
	return wrapped, io.MultiReader(pw, opts.Stdin)
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
