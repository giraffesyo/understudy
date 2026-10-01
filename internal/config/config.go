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
	// Shell plugin options for become users' temporary files (nil / ""
	// / false: the defaults).
	AdminUsers        []string // admin_users / ANSIBLE_ADMIN_USERS
	SystemTmpdirs     []string // system_tmpdirs / ANSIBLE_SYSTEM_TMPDIRS
	CommonRemoteGroup string   // common_remote_group / ANSIBLE_COMMON_REMOTE_GROUP
	WorldReadableTemp bool     // allow_world_readable_tmpfiles / ANSIBLE_SHELL_ALLOW_WORLD_READABLE_TEMP
	Source            string   // which file was loaded ("" = defaults)
	RolesPath         []string // roles_path / ANSIBLE_ROLES_PATH

	StdoutCallback      string
	CallbackPlugins     []string // callback_plugins / ANSIBLE_CALLBACK_PLUGINS
	CallbacksEnabled    []string
	DisplayOkHosts      bool
	DisplaySkippedHosts bool

	DeprecationWarnings bool // deprecation_warnings / ANSIBLE_DEPRECATION_WARNINGS
	// DuplicateDictKey is what loading YAML with a repeated mapping key
	// does: "warn", "error" or "ignore" (duplicate_dict_key /
	// ANSIBLE_DUPLICATE_YAML_DICT_KEY).
	DuplicateDictKey string
	TaskTimeout      int // task_timeout / ANSIBLE_TASK_TIMEOUT: the timeout keyword's default (0 = none)
	// InjectFactsSet: inject_facts_as_vars is configured (ini or
	// ANSIBLE_INJECT_FACT_VARS) rather than left at its default.
	InjectFactsSet bool

	// Inventory settings: localhost_warning, [inventory]
	// inventory_unparsed_warning, unparsed_is_failed,
	// any_unparsed_is_failed, enable_plugins, ignore_extensions and
	// ignore_patterns, and host_pattern_mismatch, with their
	// ANSIBLE_* environment variables.
	LocalhostWarning             bool
	InventoryUnparsedWarning     bool
	InventoryUnparsedIsFailed    bool
	InventoryAnyUnparsedIsFailed bool
	InventoryEnabled             []string
	InventoryIgnoreExts          []string
	InventoryIgnorePatterns      []string
	HostPatternMismatch          string // "warning", "error" or "ignore"
}

// Defaults returns Ansible's defaults for the supported keys.
func Defaults() *Config {
	return &Config{
		RolesPath:           splitPathspec(strings.Join(DefaultRolesPath, ":")),
		Forks:               5,
		HostKeyChecking:     true,
		Timeout:             10 * time.Second,
		DisplayOkHosts:      true,
		DisplaySkippedHosts: true,
		DeprecationWarnings: true,
		DuplicateDictKey:    "warn",

		LocalhostWarning:         true,
		InventoryUnparsedWarning: true,
		HostPatternMismatch:      "warning",
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
		// pathspec values in the file resolve against its directory.
		for i, p := range cfg.RolesPath {
			if !filepath.IsAbs(p) {
				cfg.RolesPath[i] = filepath.Join(filepath.Dir(path), p)
			}
		}
		for i, p := range cfg.Inventory {
			if !filepath.IsAbs(p) {
				cfg.Inventory[i] = filepath.Join(filepath.Dir(path), p)
			}
		}
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
			case "admin_users":
				cfg.AdminUsers = splitList(val)
			case "system_tmpdirs":
				cfg.SystemTmpdirs = splitList(val)
			case "common_remote_group":
				cfg.CommonRemoteGroup = val
			case "allow_world_readable_tmpfiles":
				cfg.WorldReadableTemp = iniBool(val, cfg.WorldReadableTemp)
			case "callback_plugins":
				cfg.CallbackPlugins = splitColonList(val)
			case "stdout_callback":
				cfg.StdoutCallback = val
			case "callbacks_enabled", "callback_whitelist", "callback_enabled":
				cfg.CallbacksEnabled = splitList(val)
			case "display_ok_hosts":
				cfg.DisplayOkHosts = iniBool(val, cfg.DisplayOkHosts)
			case "display_skipped_hosts":
				cfg.DisplaySkippedHosts = iniBool(val, cfg.DisplaySkippedHosts)
			case "roles_path":
				cfg.RolesPath = splitPathspec(val)
			case "duplicate_dict_key":
				cfg.DuplicateDictKey = strings.ToLower(strings.TrimSpace(val))
			case "deprecation_warnings":
				cfg.DeprecationWarnings = iniBool(val, cfg.DeprecationWarnings)
			case "inject_facts_as_vars":
				cfg.InjectFactsSet = true
			case "task_timeout":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.TaskTimeout = n
				}
			case "interpreter_python":
				// Parsed and ignored.
			case "localhost_warning":
				cfg.LocalhostWarning = iniBool(val, cfg.LocalhostWarning)
			case "host_pattern_mismatch":
				cfg.HostPatternMismatch = strings.ToLower(val)
			case "inventory_ignore_extensions":
				cfg.InventoryIgnoreExts = splitList(val)
			case "inventory_ignore_patterns":
				cfg.InventoryIgnorePatterns = splitList(val)
			}
		case "inventory":
			switch key {
			case "inventory_unparsed_warning":
				cfg.InventoryUnparsedWarning = iniBool(val, cfg.InventoryUnparsedWarning)
			case "unparsed_is_failed":
				cfg.InventoryUnparsedIsFailed = iniBool(val, cfg.InventoryUnparsedIsFailed)
			case "any_unparsed_is_failed":
				cfg.InventoryAnyUnparsedIsFailed = iniBool(val, cfg.InventoryAnyUnparsedIsFailed)
			case "enable_plugins":
				cfg.InventoryEnabled = splitList(val)
			case "ignore_extensions":
				cfg.InventoryIgnoreExts = splitList(val)
			case "ignore_patterns":
				cfg.InventoryIgnorePatterns = splitList(val)
			case "host_pattern_mismatch":
				cfg.HostPatternMismatch = strings.ToLower(val)
			}
		}
	}
}

// DefaultRolesPath is ansible-core's DEFAULT_ROLES_PATH.
var DefaultRolesPath = []string{"~/.ansible/roles", "/usr/share/ansible/roles", "/etc/ansible/roles"}

// splitPathspec splits a colon-separated path list, expanding ~.
func splitPathspec(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ":") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = home + p[1:]
			}
		}
		out = append(out, p)
	}
	return out
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("ANSIBLE_ROLES_PATH"); v != "" {
		cfg.RolesPath = nil
		for _, p := range splitPathspec(v) {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			cfg.RolesPath = append(cfg.RolesPath, p)
		}
	}
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
	if v := os.Getenv("ANSIBLE_ADMIN_USERS"); v != "" {
		cfg.AdminUsers = splitList(v)
	}
	if v := os.Getenv("ANSIBLE_SYSTEM_TMPDIRS"); v != "" {
		cfg.SystemTmpdirs = splitList(v)
	}
	if v := os.Getenv("ANSIBLE_COMMON_REMOTE_GROUP"); v != "" {
		cfg.CommonRemoteGroup = v
	}
	if v := os.Getenv("ANSIBLE_SHELL_ALLOW_WORLD_READABLE_TEMP"); v != "" {
		cfg.WorldReadableTemp = iniBool(v, cfg.WorldReadableTemp)
	}
	if v := os.Getenv("ANSIBLE_CALLBACK_PLUGINS"); v != "" {
		cfg.CallbackPlugins = splitColonList(v)
	}
	if v := os.Getenv("ANSIBLE_STDOUT_CALLBACK"); v != "" {
		cfg.StdoutCallback = v
	}
	for _, k := range []string{"ANSIBLE_CALLBACKS_ENABLED", "ANSIBLE_CALLBACK_WHITELIST"} {
		if v := os.Getenv(k); v != "" {
			cfg.CallbacksEnabled = splitList(v)
		}
	}
	if v := os.Getenv("ANSIBLE_DISPLAY_OK_HOSTS"); v != "" {
		cfg.DisplayOkHosts = iniBool(v, cfg.DisplayOkHosts)
	}
	if v := os.Getenv("ANSIBLE_DUPLICATE_YAML_DICT_KEY"); v != "" {
		cfg.DuplicateDictKey = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("ANSIBLE_DEPRECATION_WARNINGS"); v != "" {
		cfg.DeprecationWarnings = iniBool(v, cfg.DeprecationWarnings)
	}
	if os.Getenv("ANSIBLE_INJECT_FACT_VARS") != "" {
		cfg.InjectFactsSet = true
	}
	if v := os.Getenv("ANSIBLE_TASK_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.TaskTimeout = n
		}
	}
	if v := os.Getenv("ANSIBLE_DISPLAY_SKIPPED_HOSTS"); v != "" {
		cfg.DisplaySkippedHosts = iniBool(v, cfg.DisplaySkippedHosts)
	}
	if v := os.Getenv("ANSIBLE_DEPRECATION_WARNINGS"); v != "" {
		cfg.DeprecationWarnings = iniBool(v, cfg.DeprecationWarnings)
	}
	for env, dst := range map[string]*bool{
		"ANSIBLE_LOCALHOST_WARNING":                &cfg.LocalhostWarning,
		"ANSIBLE_INVENTORY_UNPARSED_WARNING":       &cfg.InventoryUnparsedWarning,
		"ANSIBLE_INVENTORY_UNPARSED_FAILED":        &cfg.InventoryUnparsedIsFailed,
		"ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED": &cfg.InventoryAnyUnparsedIsFailed,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = iniBool(v, *dst)
		}
	}
	if v := os.Getenv("ANSIBLE_INVENTORY_ENABLED"); v != "" {
		cfg.InventoryEnabled = splitList(v)
	}
	if v := os.Getenv("ANSIBLE_INVENTORY_IGNORE"); v != "" {
		cfg.InventoryIgnoreExts = splitList(v)
	}
	if v := os.Getenv("ANSIBLE_INVENTORY_IGNORE_REGEX"); v != "" {
		cfg.InventoryIgnorePatterns = splitList(v)
	}
	if v := os.Getenv("ANSIBLE_HOST_PATTERN_MISMATCH"); v != "" {
		cfg.HostPatternMismatch = strings.ToLower(strings.TrimSpace(v))
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

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitColonList splits Ansible's pathspec lists (colon-separated).
func splitColonList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ":") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, expandUser(p))
		}
	}
	return out
}
