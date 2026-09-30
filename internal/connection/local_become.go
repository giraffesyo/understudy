package connection

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// BecomeError is a privilege escalation failure on the local connection,
// worded as ansible-core's local connection plugin raises it ("Task
// failed: <Msg>"). Unreachable marks the timeout, which Ansible raises as
// a connection failure (the host becomes unreachable).
type BecomeError struct {
	Msg         string
	Unreachable bool
}

func (e *BecomeError) Error() string { return e.Msg }

// DefaultLocalBecomeTimeout is the local connection's become_success_timeout.
const DefaultLocalBecomeTimeout = 10 * time.Second

// sudoCommand is the sudo become plugin's build_become_command:
//
//	sudo <flags> [-p "<prompt>"] -u <user> /bin/sh -c 'echo <success> ; <cmd>'
//
// With a password the prompt is a per-run key sudo prints (-p) and the
// flags lose -n (and an n in combined short flags), so sudo asks.
func sudoCommand(b *BecomeSpec, cmd, id string) (line, success, prompt string) {
	success = "BECOME-SUCCESS-" + id
	flags := "-H -S -n"
	if b.Flags != nil {
		flags = *b.Flags
	}
	promptArg := ""
	if b.Password != "" {
		prompt = "[sudo via ansible, key=" + id + "] password:"
		if flags != "" {
			var reflag []string
			for _, f := range strings.Fields(flags) {
				if f == "-n" || f == "--non-interactive" {
					continue
				}
				if !strings.HasPrefix(f, "--") {
					f = sudoNFlag.ReplaceAllString(f, "$1$2")
				}
				reflag = append(reflag, shlexQuote(f))
			}
			flags = strings.Join(reflag, " ")
		}
		promptArg = `-p "` + prompt + `"`
	}
	line = strings.Join([]string{b.exe(), flags, promptArg, "-u " + ShellQuote(b.user()),
		"/bin/sh -c " + ShellQuote("echo "+success+" ; "+cmd)}, " ")
	return line, success, prompt
}

var sudoNFlag = regexp.MustCompile(`^(-\w*)n(\w*.*)`)

var shlexSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// shlexQuote is Python's shlex.quote.
func shlexQuote(s string) string {
	if shlexSafe.MatchString(s) {
		return s
	}
	return ShellQuote(s)
}

// becomeID is the become plugin's _gen_id: 32 random lowercase letters.
func becomeID() string {
	b := make([]byte, 32)
	rand.Read(b)
	for i := range b {
		b[i] = 'a' + b[i]%26
	}
	return string(b)
}

// execLocalSudo runs cmd through sudo the way ansible-core's local
// connection does: start the become command, answer sudo's password
// prompt (on stdout or stderr) through stdin, and wait for the success
// marker before the command's own stdin is sent. Output that precedes the
// marker (and the prompt) is stripped from the result.
func execLocalSudo(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	b := opts.Become
	line, success, prompt := sudoCommand(b, cmd, becomeID())
	c := exec.Command("/bin/sh", "-c", line)
	stdin, err := c.StdinPipe()
	if err != nil {
		return ExecResult{}, err
	}
	stdoutR, err := c.StdoutPipe()
	if err != nil {
		return ExecResult{}, err
	}
	stderrR, err := c.StderrPipe()
	if err != nil {
		return ExecResult{}, err
	}
	if err := c.Start(); err != nil {
		return ExecResult{}, fmt.Errorf("local exec: %w", err)
	}

	var (
		mu               sync.Mutex
		outBuf, errBuf   []byte
		outDone, errDone bool
		update           = make(chan struct{}, 1)
		readers          sync.WaitGroup
	)
	signal := func() {
		select {
		case update <- struct{}{}:
		default:
		}
	}
	pump := func(r io.Reader, buf *[]byte, done *bool) {
		defer readers.Done()
		chunk := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(chunk)
			if n > 0 {
				mu.Lock()
				*buf = append(*buf, chunk[:n]...)
				mu.Unlock()
				signal()
			}
			if rerr != nil {
				mu.Lock()
				*done = true
				mu.Unlock()
				signal()
				return
			}
		}
	}
	readers.Add(2)
	go pump(stdoutR, &outBuf, &outDone)
	go pump(stderrR, &errBuf, &errDone)

	abort := func(err error) (ExecResult, error) {
		stdin.Close()
		c.Process.Kill()
		// A grandchild may still hold the pipes open: reap in the
		// background rather than wait for it.
		go func() {
			readers.Wait()
			c.Wait()
		}()
		return ExecResult{}, err
	}
	timeout := b.SuccessTimeout
	if timeout <= 0 {
		timeout = DefaultLocalBecomeTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	expectPrompt := prompt != ""
	sent := false
	lastOut, lastErr := 0, 0
	errMsg := func(reason string, out, errb []byte) string {
		msg := reason + " waiting for become success"
		if expectPrompt && !sent {
			msg += " or become password prompt"
		}
		msg += "."
		if len(out) > 0 {
			msg += "\n>>> Standard Output\n" + string(out)
		}
		if len(errb) > 0 {
			msg += "\n>>> Standard Error\n" + string(errb)
		}
		return strings.TrimRight(msg, " \t\r\n")
	}
	for {
		mu.Lock()
		out, errb := append([]byte(nil), outBuf...), append([]byte(nil), errBuf...)
		eof := outDone && errDone
		mu.Unlock()
		if becomeSucceeded(out, success) {
			break
		}
		if eof {
			return abort(&BecomeError{Msg: errMsg("Premature end of stream", out, errb)})
		}
		if expectPrompt && (hasPrompt(out[lastOut:], prompt) || hasPrompt(errb[lastErr:], prompt)) {
			if sent {
				return abort(&BecomeError{Msg: errMsg("Duplicate become password prompt encountered", out, errb)})
			}
			lastOut, lastErr = len(out), len(errb)
			if _, err := io.WriteString(stdin, b.Password+"\n"); err != nil {
				return abort(fmt.Errorf("local exec: %w", err))
			}
			sent = true
		}
		select {
		case <-update:
		case <-timer.C:
			mu.Lock()
			out, errb = append([]byte(nil), outBuf...), append([]byte(nil), errBuf...)
			mu.Unlock()
			return abort(&BecomeError{Msg: errMsg("Timed out", out, errb), Unreachable: true})
		case <-ctx.Done():
			return abort(ctx.Err())
		}
	}

	// Escalated: the command's own stdin follows the password.
	go func() {
		if opts.Stdin != nil {
			io.Copy(stdin, opts.Stdin)
		}
		stdin.Close()
	}()
	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c.Process.Kill()
		case <-waitDone:
		}
	}()
	readers.Wait()
	werr := c.Wait()
	close(waitDone)
	res := ExecResult{Stdout: stripThrough(outBuf, success), Stderr: errBuf}
	if expectPrompt {
		res.Stderr = stripThrough(errBuf, prompt)
	}
	if werr != nil {
		ee, ok := werr.(*exec.ExitError)
		if !ok {
			return res, fmt.Errorf("local exec: %w", werr)
		}
		res.RC = ee.ExitCode()
	}
	return res, ctx.Err()
}

// becomeSucceeded is check_success: a line of output contains the marker.
func becomeSucceeded(out []byte, success string) bool {
	for _, l := range bytes.SplitAfter(out, []byte("\n")) {
		if bytes.Contains(bytes.TrimRight(l, " \t\r\n\v\f"), []byte(success)) {
			return true
		}
	}
	return false
}

// hasPrompt is check_password_prompt: a line starts with the prompt.
func hasPrompt(out []byte, prompt string) bool {
	p := []byte(strings.TrimSpace(prompt))
	for _, l := range bytes.Split(out, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(l), p) {
			return true
		}
	}
	return false
}

// stripThrough drops everything up to and including the first match and
// the whitespace after it (the become plugin's _strip_through_prefix).
func stripThrough(data []byte, match string) []byte {
	i := bytes.Index(data, []byte(match))
	if i < 0 {
		return data
	}
	return bytes.TrimLeft(data[i+len(match):], " \t\r\n\v\f")
}
