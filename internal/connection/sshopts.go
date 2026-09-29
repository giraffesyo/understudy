package connection

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshOptions are the OpenSSH client options understudy's native client
// honors from ansible_ssh_common_args / ansible_ssh_extra_args (and the
// matching CLI flags).
type sshOptions struct {
	ProxyJump      []string // hops, "[user@]host[:port]", first to last
	ProxyCommand   string
	IdentityFiles  []string
	Port           int
	StrictHostKeys *bool
	Ignored        []string // options with no native equivalent
}

// harmlessSSHOptions tune OpenSSH itself (multiplexing, keepalive,
// compression); the native client reuses one connection per host anyway.
var harmlessSSHOptions = map[string]bool{
	"controlmaster": true, "controlpersist": true, "controlpath": true,
	"serveraliveinterval": true, "serveralivecountmax": true, "tcpkeepalive": true,
	"compression": true, "connecttimeout": true, "loglevel": true,
	"preferredauthentications": true, "passwordauthentication": true,
	"kbdinteractiveauthentication": true, "pubkeyauthentication": true,
	"gssapiauthentication": true, "identitiesonly": true, "forwardagent": true,
	"user": true, "addkeystoagent": true, "batchmode": true,
}

// parseSSHArgs reads an OpenSSH-style argument string ("-o K=V", "-oK=V",
// "-o K V", "-J hops", "-i key", "-p port", "-C").
func parseSSHArgs(args ...string) (sshOptions, error) {
	var o sshOptions
	for _, a := range args {
		words, err := shellWords(a)
		if err != nil {
			return o, fmt.Errorf("parsing SSH arguments %q: %v", a, err)
		}
		for i := 0; i < len(words); i++ {
			w := words[i]
			next := func() (string, error) {
				if i+1 >= len(words) {
					return "", fmt.Errorf("SSH argument %s needs a value", w)
				}
				i++
				return words[i], nil
			}
			switch {
			case w == "-o" || strings.HasPrefix(w, "-o"):
				opt := strings.TrimPrefix(w, "-o")
				if opt == "" {
					if opt, err = next(); err != nil {
						return o, err
					}
				}
				key, val, ok := strings.Cut(opt, "=")
				if !ok {
					// "-o Key Value" spelling.
					if key, val, ok = strings.Cut(opt, " "); !ok {
						if val, err = next(); err != nil {
							return o, err
						}
					}
				}
				o.set(strings.TrimSpace(key), strings.TrimSpace(val))
			case w == "-J":
				v, err := next()
				if err != nil {
					return o, err
				}
				o.set("ProxyJump", v)
			case w == "-i":
				v, err := next()
				if err != nil {
					return o, err
				}
				o.IdentityFiles = append(o.IdentityFiles, v)
			case w == "-p":
				v, err := next()
				if err != nil {
					return o, err
				}
				o.set("Port", v)
			case w == "-C" || w == "-q" || w == "-v" || w == "-4" || w == "-6" || w == "-A" || w == "-T" || w == "-t" || w == "-tt":
				// client behavior flags with no effect on the native client
			default:
				o.Ignored = append(o.Ignored, w)
			}
		}
	}
	return o, nil
}

func (o *sshOptions) set(key, val string) {
	switch strings.ToLower(key) {
	case "proxyjump":
		if strings.EqualFold(val, "none") {
			o.ProxyJump = nil
			return
		}
		for _, hop := range strings.Split(val, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				o.ProxyJump = append(o.ProxyJump, hop)
			}
		}
	case "proxycommand":
		if !strings.EqualFold(val, "none") {
			o.ProxyCommand = val
		}
	case "identityfile":
		o.IdentityFiles = append(o.IdentityFiles, expandHome(val))
	case "port":
		if n, err := strconv.Atoi(val); err == nil {
			o.Port = n
		}
	case "stricthostkeychecking":
		strict := !(strings.EqualFold(val, "no") || strings.EqualFold(val, "off") || strings.EqualFold(val, "accept-new"))
		o.StrictHostKeys = &strict
	case "userknownhostsfile":
		if val == "/dev/null" {
			strict := false
			o.StrictHostKeys = &strict
		}
	default:
		if !harmlessSSHOptions[strings.ToLower(key)] {
			o.Ignored = append(o.Ignored, key+"="+val)
		}
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// shellWords splits a string like /bin/sh would (quotes, backslashes).
func shellWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case c == '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unbalanced single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unbalanced double quote")
			}
		case c == '\\' && i+1 < len(s):
			in = true
			i++
			cur.WriteByte(s[i])
		default:
			in = true
			cur.WriteByte(c)
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// splitHop parses "[user@]host[:port]" (IPv6 as [addr]:port).
func splitHop(hop, defUser string) (user, addr string) {
	user = defUser
	if at := strings.LastIndexByte(hop, '@'); at >= 0 {
		user, hop = hop[:at], hop[at+1:]
	}
	if host, port, err := net.SplitHostPort(hop); err == nil {
		return user, net.JoinHostPort(host, port)
	}
	return user, net.JoinHostPort(strings.Trim(hop, "[]"), "22")
}

// dialVia opens the SSH client for addr, through ProxyCommand or a
// ProxyJump chain when configured, else directly.
func dialVia(cfg SSHConfig, addr string, clientCfg *ssh.ClientConfig) (*ssh.Client, error) {
	if cfg.ProxyCommand != "" {
		host, port, _ := net.SplitHostPort(addr)
		cmdline := strings.NewReplacer("%h", host, "%p", port, "%r", clientCfg.User, "%%", "%").Replace(cfg.ProxyCommand)
		conn, err := commandConn(cmdline)
		if err != nil {
			return nil, fmt.Errorf("ProxyCommand %q: %w", cmdline, err)
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return ssh.NewClient(c, chans, reqs), nil
	}
	if len(cfg.ProxyJump) == 0 {
		return ssh.Dial("tcp", addr, clientCfg)
	}
	// First hop directly, each later hop (and the target) through the
	// previous hop's client.
	var prev *ssh.Client
	targets := append(append([]string{}, cfg.ProxyJump...), "")
	for i, hop := range targets {
		hopCfg := *clientCfg
		hopAddr := addr
		if i < len(cfg.ProxyJump) {
			hopCfg.User, hopAddr = splitHop(hop, clientCfg.User)
		}
		var client *ssh.Client
		var err error
		if prev == nil {
			client, err = ssh.Dial("tcp", hopAddr, &hopCfg)
		} else {
			conn, derr := prev.Dial("tcp", hopAddr)
			if derr != nil {
				return nil, fmt.Errorf("jump to %s: %w", hopAddr, derr)
			}
			c, chans, reqs, cerr := ssh.NewClientConn(conn, hopAddr, &hopCfg)
			if cerr != nil {
				conn.Close()
				return nil, fmt.Errorf("jump to %s: %w", hopAddr, cerr)
			}
			client = ssh.NewClient(c, chans, reqs)
		}
		if err != nil {
			return nil, fmt.Errorf("jump host %s: %w", hopAddr, err)
		}
		prev = client
	}
	return prev, nil
}

// commandConn runs a ProxyCommand and presents its stdio as a net.Conn.
func commandConn(cmdline string) (net.Conn, error) {
	cmd := exec.Command("/bin/sh", "-c", cmdline)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pipeConn{r: stdout, w: stdin, cmd: cmd}, nil
}

type pipeConn struct {
	r   io.ReadCloser
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.w.Close()
	p.r.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}
func (p *pipeConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (p *pipeConn) SetDeadline(time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "proxycommand" }
