package connection

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// HostVars supplies behavioral connection variables for a host
// (ansible_host, ansible_user, ...). Implemented by the vars store.
type HostVars interface {
	RawHostVar(host, name string) (any, bool)
}

// ManagerOptions carry CLI/config-level connection defaults.
type ManagerOptions struct {
	Connection      string // -c override: "", "ssh", "local", "smart"
	RemoteUser      string // -u
	PrivateKey      string // --private-key
	Password        string // -k prompt result
	HostKeyChecking bool
	Timeout         time.Duration
	RemoteTmp       string
	KeyPassphrase   func() (string, error)
}

// Manager caches one connection (and bootstrapped agent) per host.
type Manager struct {
	Vars HostVars
	Opts ManagerOptions

	mu    sync.Mutex
	hosts map[string]*hostConn
}

type hostConn struct {
	once      sync.Once
	conn      Connection
	inProcess bool // local: modules run in-process, no agent client
	err       error

	agentOnce sync.Once
	agent     *AgentClient
	agentErr  error
}

func NewManager(vars HostVars, opts ManagerOptions) *Manager {
	return &Manager{Vars: vars, Opts: opts, hosts: map[string]*hostConn{}}
}

func (m *Manager) hostConn(host string) *hostConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	hc, ok := m.hosts[host]
	if !ok {
		hc = &hostConn{}
		m.hosts[host] = hc
	}
	return hc
}

// Get returns the connection for a host, dialing on first use.
// inProcess=true means modules run in-process (local connection).
func (m *Manager) Get(ctx context.Context, host string) (Connection, bool, error) {
	hc := m.hostConn(host)
	hc.once.Do(func() {
		hc.conn, hc.inProcess, hc.err = m.dial(ctx, host)
	})
	return hc.conn, hc.inProcess, hc.err
}

// Agent returns the bootstrapped agent client for a host, uploading the
// agent binary on first use. Lazy so `raw` works on targets the agent
// doesn't support.
func (m *Manager) Agent(ctx context.Context, host string) (*AgentClient, error) {
	conn, inProcess, err := m.Get(ctx, host)
	if err != nil {
		return nil, err
	}
	if inProcess {
		return nil, fmt.Errorf("internal error: local connections do not use the agent")
	}
	hc := m.hostConn(host)
	hc.agentOnce.Do(func() {
		path, err := Bootstrap(ctx, conn, m.Opts.RemoteTmp)
		if err != nil {
			hc.agentErr = fmt.Errorf("agent bootstrap on %s: %w", host, err)
			return
		}
		hc.agent = &AgentClient{Conn: conn, AgentPath: path}
	})
	return hc.agent, hc.agentErr
}

func (m *Manager) strVar(host, name, fallback string) string {
	if m.Vars != nil {
		if v, ok := m.Vars.RawHostVar(host, name); ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return fallback
}

func (m *Manager) intVar(host, name string, fallback int) int {
	if m.Vars != nil {
		if v, ok := m.Vars.RawHostVar(host, name); ok {
			if n, ok := v.(int64); ok {
				return int(n)
			}
		}
	}
	return fallback
}

func (m *Manager) dial(ctx context.Context, host string) (Connection, bool, error) {
	kind := m.Opts.Connection
	if kind == "" {
		kind = m.strVar(host, "ansible_connection", "")
	}
	if kind == "" || kind == "smart" {
		if host == "localhost" || host == "127.0.0.1" {
			kind = "local"
		} else {
			kind = "ssh"
		}
	}

	switch kind {
	case "local":
		return NewLocal(), true, nil
	case "ssh":
		cfg := SSHConfig{
			Host:            m.strVar(host, "ansible_host", host),
			Port:            m.intVar(host, "ansible_port", 22),
			User:            m.strVar(host, "ansible_user", m.strVar(host, "ansible_ssh_user", m.Opts.RemoteUser)),
			Password:        m.strVar(host, "ansible_password", m.strVar(host, "ansible_ssh_pass", m.Opts.Password)),
			HostKeyChecking: m.Opts.HostKeyChecking,
			Timeout:         m.Opts.Timeout,
			KeyPassphrase:   m.Opts.KeyPassphrase,
		}
		if key := m.strVar(host, "ansible_ssh_private_key_file", m.Opts.PrivateKey); key != "" {
			cfg.PrivateKeys = append(cfg.PrivateKeys, key)
		}
		conn, err := DialSSH(cfg)
		if err != nil {
			return nil, false, err
		}
		return conn, false, nil
	}
	return nil, false, fmt.Errorf("unknown connection type %q for host %s", kind, host)
}

// Reset closes a host's cached connection so the next use redials
// (meta: reset_connection).
func (m *Manager) Reset(host string) {
	m.mu.Lock()
	hc, ok := m.hosts[host]
	delete(m.hosts, host)
	m.mu.Unlock()
	if ok && hc.conn != nil {
		hc.conn.Close()
	}
}

// CloseAll tears down every cached connection.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, hc := range m.hosts {
		if hc.conn != nil {
			hc.conn.Close()
		}
	}
}
