// Package config loads ansible.cfg with Ansible's discovery order and
// environment-variable overrides.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds the settings understudy respects in v0.1.
type Config struct {
	Inventory       []string
	RemoteUser      string
	Forks           int
	HostKeyChecking bool
	PrivateKeyFile  string
	Timeout         time.Duration
	RemoteTmp       string
	Source          string // which file was loaded ("" = defaults)
}

// Defaults returns Ansible's defaults for the supported keys.
func Defaults() *Config {
	return &Config{
		Forks:           5,
		HostKeyChecking: true,
		Timeout:         10 * time.Second,
	}
}

// Load discovers and parses ansible.cfg: ANSIBLE_CONFIG, ./ansible.cfg,
// ~/.ansible.cfg, /etc/ansible/ansible.cfg — first hit wins (no merging,
// like Ansible). Environment variables override file values.
func Load() (*Config, error) {
	cfg := Defaults()

	var candidates []string
	if env := os.Getenv("ANSIBLE_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, "ansible.cfg")
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	candidates = append(candidates, "/etc/ansible/ansible.cfg")

	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		applyINI(cfg, string(data))
		cfg.Source = path
		break
	}

	applyEnvOverrides(cfg)
	return cfg, nil
}

// applyINI parses the tiny ansible.cfg INI dialect (key = value under
// [section] headers) and applies known keys.
func applyINI(cfg *Config, content string) {
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			if end := strings.IndexByte(line, ']'); end > 0 {
				section = line[1:end]
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch section {
		case "defaults":
			switch key {
			case "inventory":
				cfg.Inventory = splitPathList(val)
			case "remote_user":
				cfg.RemoteUser = val
			case "forks":
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					cfg.Forks = n
				}
			case "host_key_checking":
				cfg.HostKeyChecking = iniBool(val, cfg.HostKeyChecking)
			case "private_key_file":
				cfg.PrivateKeyFile = expandUser(val)
			case "timeout":
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					cfg.Timeout = time.Duration(n) * time.Second
				}
			case "remote_tmp":
				cfg.RemoteTmp = val
			case "interpreter_python", "roles_path":
				// Parsed and ignored in v0.1.
			}
		}
	}
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("ANSIBLE_INVENTORY"); v != "" {
		cfg.Inventory = splitPathList(v)
	}
	if v := os.Getenv("ANSIBLE_REMOTE_USER"); v != "" {
		cfg.RemoteUser = v
	}
	if v := os.Getenv("ANSIBLE_FORKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Forks = n
		}
	}
	if v := os.Getenv("ANSIBLE_HOST_KEY_CHECKING"); v != "" {
		cfg.HostKeyChecking = iniBool(v, cfg.HostKeyChecking)
	}
	if v := os.Getenv("ANSIBLE_PRIVATE_KEY_FILE"); v != "" {
		cfg.PrivateKeyFile = expandUser(v)
	}
	if v := os.Getenv("ANSIBLE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("ANSIBLE_REMOTE_TMP"); v != "" {
		cfg.RemoteTmp = v
	}
}

func iniBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	return def
}

func splitPathList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, expandUser(p))
		}
	}
	return out
}

func expandUser(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
