package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDiscoveryAndOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "ansible.cfg")
	os.WriteFile(cfgFile, []byte(`
[defaults]
forks = 20
remote_user = deploy
host_key_checking = False
timeout = 30
inventory = ./hosts,~/inv2
remote_tmp = /opt/tmp
interpreter_python = /usr/bin/python3

[ssh_connection]
pipelining = True
`), 0o644)

	t.Setenv("ANSIBLE_CONFIG", cfgFile)
	t.Setenv("ANSIBLE_FORKS", "")
	os.Unsetenv("ANSIBLE_FORKS")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Forks != 20 || cfg.RemoteUser != "deploy" || cfg.HostKeyChecking ||
		cfg.Timeout != 30*time.Second || cfg.RemoteTmp != "/opt/tmp" {
		t.Errorf("cfg = %+v", cfg)
	}
	// A set but empty variable wins over the file: an integer setting
	// takes its default, a boolean is False.
	t.Setenv("ANSIBLE_FORKS", "")
	t.Setenv("ANSIBLE_TIMEOUT", "")
	t.Setenv("ANSIBLE_DISPLAY_OK_HOSTS", "")
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if cfg.Forks != 5 || cfg.Timeout != 10*time.Second || cfg.DisplayOkHosts {
		t.Errorf("empty environment: forks %d, timeout %v, display_ok_hosts %v", cfg.Forks, cfg.Timeout, cfg.DisplayOkHosts)
	}
	os.Unsetenv("ANSIBLE_FORKS")
	os.Unsetenv("ANSIBLE_TIMEOUT")
	os.Unsetenv("ANSIBLE_DISPLAY_OK_HOSTS")
	// Relative inventory paths resolve against the config file's directory.
	if len(cfg.Inventory) != 2 || cfg.Inventory[0] != filepath.Join(dir, "hosts") {
		t.Errorf("inventory = %v", cfg.Inventory)
	}
	if cfg.Source != cfgFile {
		t.Errorf("source = %q", cfg.Source)
	}

	// Env beats file.
	t.Setenv("ANSIBLE_FORKS", "3")
	t.Setenv("ANSIBLE_REMOTE_USER", "envuser")
	cfg, _ = Load()
	if cfg.Forks != 3 || cfg.RemoteUser != "envuser" {
		t.Errorf("env override failed: %+v", cfg)
	}
}

func TestDefaultsWhenNoFile(t *testing.T) {
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(t.TempDir(), "nonexistent.cfg"))
	t.Chdir(t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Forks != 5 || !cfg.HostKeyChecking || cfg.Source != "" {
		t.Errorf("defaults = %+v", cfg)
	}
}
