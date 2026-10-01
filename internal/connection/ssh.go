package connection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHConfig describes how to reach one host.
type SSHConfig struct {
	Host            string // address (ansible_host or inventory name)
	Port            int    // default 22
	User            string // default: current user
	Password        string
	PrivateKeys     []string // explicit key files, tried in order
	KeyPassphrase   func() (string, error)
	HostKeyChecking bool
	Timeout         time.Duration // dial timeout, default 10s
	ProxyJump       []string      // jump hosts, first to last
	ProxyCommand    string        // OpenSSH ProxyCommand (%h %p %r expanded)
}

// SSH is one host's connection: a single ssh.Client reused for the whole
// run, with per-exec sessions.
type SSH struct {
	client *ssh.Client
	cfg    SSHConfig
	done   chan struct{}
}

// DialSSH establishes the connection, running the auth chain:
// ssh-agent -> explicit keys -> default keys -> password.
func DialSSH(cfg SSHConfig) (*SSH, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.User == "" {
		cfg.User = os.Getenv("USER")
	}

	var methods []ssh.AuthMethod

	// When an explicit key file is configured, use ONLY it — like Ansible's
	// IdentitiesOnly=yes. Offering the agent's and default keys first can
	// exhaust the server's MaxAuthTries before the intended key is reached.
	explicitKey := len(cfg.PrivateKeys) > 0

	// 1. ssh-agent (skipped when an explicit key is given).
	if !explicitKey {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if conn, err := net.Dial("unix", sock); err == nil {
				ag := agent.NewClient(conn)
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			}
		}
	}

	// 2. Key files: the explicit ones, or the conventional defaults.
	keyFiles := append([]string{}, cfg.PrivateKeys...)
	if !explicitKey {
		if home, err := os.UserHomeDir(); err == nil {
			for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
				keyFiles = append(keyFiles, filepath.Join(home, ".ssh", name))
			}
		}
	}
	var signers []ssh.Signer
	for _, path := range keyFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if isPassphraseErr(err, &passErr) && cfg.KeyPassphrase != nil {
				phrase, perr := cfg.KeyPassphrase()
				if perr != nil {
					continue
				}
				signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(phrase))
				if err != nil {
					continue
				}
			} else {
				continue
			}
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	// 3. Password, under both auth flavors (many sshds accept only
	// keyboard-interactive).
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
		methods = append(methods, ssh.KeyboardInteractive(
			func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = cfg.Password
				}
				return answers, nil
			}))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH authentication methods available for %s (no agent, keys, or password)", cfg.Host)
	}

	hostKeyCallback := ssh.InsecureIgnoreHostKey()
	if cfg.HostKeyChecking {
		cb, err := knownHostsCallback()
		if err != nil {
			return nil, err
		}
		hostKeyCallback = cb
	}

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.Timeout,
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	client, err := dialVia(cfg, addr, clientCfg)
	if err != nil {
		if cfg.HostKeyChecking && strings.Contains(err.Error(), "knownhosts") {
			return nil, fmt.Errorf(
				"host key verification failed for %s: %w\n(add it with: ssh-keyscan -H %s >> ~/.ssh/known_hosts, or set host_key_checking = False)",
				cfg.Host, err, cfg.Host)
		}
		return nil, fmt.Errorf("ssh connection to %s failed: %w", addr, err)
	}

	s := &SSH{client: client, cfg: cfg, done: make(chan struct{})}
	go s.keepalive()
	return s, nil
}

func isPassphraseErr(err error, out **ssh.PassphraseMissingError) bool {
	pe, ok := err.(*ssh.PassphraseMissingError)
	if ok {
		*out = pe
	}
	return ok
}

func knownHostsCallback() (ssh.HostKeyCallback, error) {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".ssh", "known_hosts")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	if _, err := os.Stat("/etc/ssh/ssh_known_hosts"); err == nil {
		files = append(files, "/etc/ssh/ssh_known_hosts")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("host key checking is enabled but no known_hosts file exists (set host_key_checking = False to disable)")
	}
	return knownhosts.New(files...)
}

// keepalive prevents idle sshd timeouts during long tasks.
func (s *SSH) keepalive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.client.SendRequest("keepalive@openssh.com", true, nil)
		}
	}
}

// Exec runs cmd in a fresh session, applying become wrapping.
func (s *SSH) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	if opts.Become.needsPTY() {
		plain := func(ctx context.Context, c string, in io.Reader) (ExecResult, error) {
			return s.Exec(ctx, c, ExecOptions{Stdin: in, Timeout: opts.Timeout})
		}
		return execPTYBecome(ctx, plain, s.startPTY, cmd, opts)
	}
	shellCmd, stdin := applyBecome(cmd, opts)

	session, err := s.client.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("opening SSH session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	if stdin != nil {
		session.Stdin = stdin
	}

	errCh := make(chan error, 1)
	go func() { errCh <- session.Run(shellCmd) }()

	select {
	case <-ctx.Done():
		// A terminated agent takes the module's process groups down
		// with it; whatever ignores the SIGTERM is killed.
		session.Signal(ssh.SIGTERM)
		select {
		case <-errCh:
		case <-time.After(killGrace):
			session.Signal(ssh.SIGKILL)
		}
		return ExecResult{}, ctx.Err()
	case err := <-errCh:
		res := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				res.RC = exitErr.ExitStatus()
				return res, nil
			}
			return res, fmt.Errorf("ssh exec: %w", err)
		}
		return res, nil
	}
}

// sshPTY is a command running in a session with a pseudo-terminal.
type sshPTY struct {
	session *ssh.Session
	in      io.WriteCloser
	out     io.Reader
}

func (p *sshPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *sshPTY) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *sshPTY) Wait() error {
	err := p.session.Wait()
	p.session.Close()
	return err
}
func (p *sshPTY) Kill() { p.session.Signal(ssh.SIGKILL); p.session.Close() }

// startPTY runs cmd in a session with a terminal (echo off), for become
// methods that prompt on a tty (su, doas).
func (s *SSH) startPTY(ctx context.Context, cmd string) (ptyProcess, error) {
	session, err := s.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("opening SSH session: %w", err)
	}
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	if err := session.RequestPty("xterm", 24, 200, modes); err != nil {
		session.Close()
		return nil, fmt.Errorf("requesting a terminal for become: %w", err)
	}
	in, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	out, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	if err := session.Start(cmd); err != nil {
		session.Close()
		return nil, err
	}
	return &sshPTY{session: session, in: in, out: out}, nil
}

// WriteFile streams content to path via cat with an atomic temp+rename.
// Used only for agent bootstrap; regular file placement goes via the agent.
func (s *SSH) WriteFile(ctx context.Context, path string, content io.Reader, mode uint32) error {
	dir := filepath.Dir(path)
	tmp := path + ".tmp"
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %o %s && mv -f %s %s",
		ShellQuote(dir), ShellQuote(tmp), mode, ShellQuote(tmp), ShellQuote(tmp), ShellQuote(path))
	res, err := s.Exec(ctx, cmd, ExecOptions{Stdin: content})
	if err != nil {
		return err
	}
	if res.RC != 0 {
		return fmt.Errorf("remote write to %s failed (rc=%d): %s", path, res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func (s *SSH) Close() error {
	close(s.done)
	return s.client.Close()
}
