// Package config loads ansible.cfg with Ansible's discovery order and
// environment-variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	ShowCustomStats     bool // show_custom_stats / ANSIBLE_SHOW_CUSTOM_STATS

	// The command line's defaults: transport (-c), become_method and
	// become_user (--become-method, --become-user), poll_interval (-P)
	// and module_name (-m).
	Transport    string
	BecomeMethod string
	BecomeUser   string
	PollInterval int
	ModuleName   string
	Verbosity    int // verbosity / ANSIBLE_VERBOSITY: where -v counts from

	// Warnings are the configuration's own warnings (a world writable
	// working directory's ansible.cfg, ignored).
	Warnings []string

	DeprecationWarnings bool // deprecation_warnings / ANSIBLE_DEPRECATION_WARNINGS
	// DuplicateDictKey is what loading YAML with a repeated mapping key
	// does: "warn", "error" or "ignore" (duplicate_dict_key /
	// ANSIBLE_DUPLICATE_YAML_DICT_KEY).
	DuplicateDictKey string
	TaskTimeout      int // task_timeout / ANSIBLE_TASK_TIMEOUT: the timeout keyword's default (0 = none)
	// AllowBrokenConditionals is allow_broken_conditionals /
	// ANSIBLE_ALLOW_BROKEN_CONDITIONALS: a conditional that is not a
	// boolean warns rather than failing.
	AllowBrokenConditionals bool
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
		Transport:           "ssh",
		BecomeMethod:        "sudo",
		BecomeUser:          "root",
		PollInterval:        15,
		ModuleName:          "command",

		LocalhostWarning:         true,
		InventoryUnparsedWarning: true,
		HostPatternMismatch:      "warning",
	}
}

// Error is a configuration error ansible-core raises while loading its
// constants, before the command line is even parsed: shown as
// "ERROR: <message>", exit code 5.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ExitCode is ansible's exit status for a configuration error.
func (e *Error) ExitCode() int { return 5 }

// Load discovers and parses ansible.cfg as ansible-core's ConfigManager
// does: ANSIBLE_CONFIG (a directory names its ansible.cfg), then
// ./ansible.cfg (unless the directory is world writable, which warns),
// ~/.ansible.cfg and /etc/ansible/ansible.cfg; the first readable one
// wins (no merging). Environment variables override file values. A file
// configparser cannot read, or a setting of the wrong type or outside its
// choices, is an *Error.
func Load() (*Config, error) {
	cfg := Defaults()
	path, warnings := findConfigFile()
	cfg.Warnings = warnings
	var ini *iniFile
	if path != "" {
		switch ext := filepath.Ext(path); ext {
		case ".ini", ".cfg":
		case ".yaml", ".yml":
			return nil, &Error{"Unsupported configuration file type: yaml"}
		default:
			return nil, &Error{fmt.Sprintf("Unsupported configuration file extension for %s: %s", path, ext)}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, &Error{fmt.Sprintf("Error reading config file (%s): %v", path, err)}
		}
		// Undecodable bytes are kept (surrogateescape), not an error.
		if ini, err = parseINI(string(data)); err != nil {
			return nil, &Error{fmt.Sprintf("Error reading config file (%s): %v", path, err)}
		}
		cfg.Source = path
	}
	if err := checkTypedSettings(ini, path); err != nil {
		return nil, err
	}
	if ini != nil {
		applyINI(cfg, ini)
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
	}

	applyEnvOverrides(cfg)
	return cfg, nil
}

// findConfigFile is find_ini_config_file: the config file in effect ("" =
// none) and the warning for a world writable working directory's
// ansible.cfg, skipped.
func findConfigFile() (string, []string) {
	var candidates []string
	fromEnv, envSet := os.LookupEnv("ANSIBLE_CONFIG")
	if envSet {
		fromEnv = unfrackPath(fromEnv)
		if st, err := os.Stat(fromEnv); err == nil && st.IsDir() {
			fromEnv = filepath.Join(fromEnv, "ansible.cfg")
		}
		candidates = append(candidates, fromEnv)
	}
	warnCwd := false
	cwd, err := syscall.Getwd() // os.getcwd(): the real path, not $PWD
	if err == nil {
		if st, err := os.Stat(cwd); err == nil {
			cwdCfg := filepath.Join(cwd, "ansible.cfg")
			if st.Mode().Perm()&0o002 != 0 {
				if _, err := os.Stat(cwdCfg); err == nil {
					warnCwd = true
				}
			} else {
				candidates = append(candidates, cwdCfg)
			}
		}
	}
	candidates = append(candidates, unfrackPath("~/.ansible.cfg"), "/etc/ansible/ansible.cfg")
	path := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil && syscall.Access(c, 4) == nil { // R_OK
			path = c
			break
		}
	}
	var warnings []string
	if warnCwd && !(envSet && fromEnv == path) {
		warnings = append(warnings, fmt.Sprintf("Ansible is being run in a world writable directory (%s), ignoring it as an ansible.cfg source. "+
			"For more information see https://docs.ansible.com/ansible/devel/reference_appendices/config.html#cfg-in-world-writable-dir", cwd))
	}
	return path, warnings
}

// unfrackPath is unfrackpath(path, follow=False): ~ and $VARS expanded,
// made absolute and normalized.
func unfrackPath(p string) string {
	p = os.ExpandEnv(expandUser(p))
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// iniString is a string setting's ini value, unquoted as ensure_type
// unquotes ini strings.
func iniString(f *iniFile, section, key string) (string, bool) {
	v, ok := f.get(section, key)
	return unquote(v), ok
}

// applyINI applies the file's settings understudy knows. Where a setting
// has several keys, the last one set wins (ConfigManager._loop_entries).
func applyINI(cfg *Config, f *iniFile) {
	str := func(section, key string, set func(string)) {
		if v, ok := iniString(f, section, key); ok {
			set(v)
		}
	}
	boolean := func(section, key string, dst *bool) {
		if v, ok := iniString(f, section, key); ok {
			*dst = pyBoolean(v)
		}
	}
	integer := func(section, key string, set func(int)) {
		if v, ok := f.get(section, key); ok {
			if n, ok := pyDecimalInt(v); ok {
				set(n)
			}
		}
	}
	str("defaults", "inventory", func(v string) { cfg.Inventory = splitPathList(v) })
	str("defaults", "remote_user", func(v string) { cfg.RemoteUser = v })
	integer("defaults", "forks", func(n int) { cfg.Forks = n })
	boolean("defaults", "host_key_checking", &cfg.HostKeyChecking)
	str("defaults", "private_key_file", func(v string) { cfg.PrivateKeyFile = expandUser(v) })
	integer("defaults", "timeout", func(n int) { cfg.Timeout = time.Duration(n) * time.Second })
	str("defaults", "remote_tmp", func(v string) { cfg.RemoteTmp = v })
	str("defaults", "admin_users", func(v string) { cfg.AdminUsers = splitList(v) })
	str("defaults", "system_tmpdirs", func(v string) { cfg.SystemTmpdirs = splitList(v) })
	str("defaults", "common_remote_group", func(v string) { cfg.CommonRemoteGroup = v })
	boolean("defaults", "allow_world_readable_tmpfiles", &cfg.WorldReadableTemp)
	str("defaults", "callback_plugins", func(v string) { cfg.CallbackPlugins = splitColonList(v) })
	str("defaults", "stdout_callback", func(v string) { cfg.StdoutCallback = v })
	for _, k := range []string{"callback_whitelist", "callback_enabled", "callbacks_enabled"} {
		str("defaults", k, func(v string) { cfg.CallbacksEnabled = splitList(v) })
	}
	boolean("defaults", "display_ok_hosts", &cfg.DisplayOkHosts)
	boolean("defaults", "display_skipped_hosts", &cfg.DisplaySkippedHosts)
	boolean("defaults", "show_custom_stats", &cfg.ShowCustomStats)
	str("defaults", "roles_path", func(v string) { cfg.RolesPath = splitPathspec(v) })
	str("defaults", "duplicate_dict_key", func(v string) { cfg.DuplicateDictKey = strings.ToLower(strings.TrimSpace(v)) })
	boolean("defaults", "deprecation_warnings", &cfg.DeprecationWarnings)
	if _, ok := f.get("defaults", "inject_facts_as_vars"); ok {
		cfg.InjectFactsSet = true
	}
	boolean("defaults", "allow_broken_conditionals", &cfg.AllowBrokenConditionals)
	integer("defaults", "task_timeout", func(n int) { cfg.TaskTimeout = n })
	boolean("defaults", "localhost_warning", &cfg.LocalhostWarning)
	str("defaults", "transport", func(v string) { cfg.Transport = v })
	str("defaults", "module_name", func(v string) { cfg.ModuleName = v })
	integer("defaults", "poll_interval", func(n int) { cfg.PollInterval = n })
	integer("defaults", "verbosity", func(n int) { cfg.Verbosity = n })
	str("privilege_escalation", "become_method", func(v string) { cfg.BecomeMethod = v })
	str("privilege_escalation", "become_user", func(v string) { cfg.BecomeUser = v })
	boolean("inventory", "inventory_unparsed_warning", &cfg.InventoryUnparsedWarning)
	boolean("inventory", "unparsed_is_failed", &cfg.InventoryUnparsedIsFailed)
	boolean("inventory", "any_unparsed_is_failed", &cfg.InventoryAnyUnparsedIsFailed)
	str("inventory", "enable_plugins", func(v string) { cfg.InventoryEnabled = splitList(v) })
	for _, sk := range [][2]string{{"defaults", "inventory_ignore_extensions"}, {"inventory", "ignore_extensions"}} {
		str(sk[0], sk[1], func(v string) { cfg.InventoryIgnoreExts = splitList(v) })
	}
	for _, sk := range [][2]string{{"defaults", "inventory_ignore_patterns"}, {"inventory", "ignore_patterns"}} {
		str(sk[0], sk[1], func(v string) { cfg.InventoryIgnorePatterns = splitList(v) })
	}
	for _, sk := range [][2]string{{"defaults", "host_pattern_mismatch"}, {"inventory", "host_pattern_mismatch"}} {
		str(sk[0], sk[1], func(v string) { cfg.HostPatternMismatch = strings.ToLower(v) })
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
	for env, dst := range map[string]*string{
		"ANSIBLE_TRANSPORT": &cfg.Transport, "ANSIBLE_BECOME_METHOD": &cfg.BecomeMethod, "ANSIBLE_BECOME_USER": &cfg.BecomeUser,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("ANSIBLE_VERBOSITY"); v != "" {
		if n, ok := pyDecimalInt(v); ok {
			cfg.Verbosity = n
		}
	}
	if v := os.Getenv("ANSIBLE_POLL_INTERVAL"); v != "" {
		if n, ok := pyDecimalInt(v); ok {
			cfg.PollInterval = n
		}
	}
	if v := os.Getenv("ANSIBLE_FORKS"); v != "" {
		if n, ok := pyDecimalInt(v); ok { // the command line rejects one below 1
			cfg.Forks = n
		}
	}
	if v := os.Getenv("ANSIBLE_HOST_KEY_CHECKING"); v != "" {
		cfg.HostKeyChecking = pyBoolean(v)
	}
	if v := os.Getenv("ANSIBLE_PRIVATE_KEY_FILE"); v != "" {
		cfg.PrivateKeyFile = expandUser(v)
	}
	if v := os.Getenv("ANSIBLE_TIMEOUT"); v != "" {
		if n, ok := pyDecimalInt(v); ok && n > 0 {
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
		cfg.WorldReadableTemp = pyBoolean(v)
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
		cfg.DisplayOkHosts = pyBoolean(v)
	}
	if v := os.Getenv("ANSIBLE_DUPLICATE_YAML_DICT_KEY"); v != "" {
		cfg.DuplicateDictKey = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("ANSIBLE_DEPRECATION_WARNINGS"); v != "" {
		cfg.DeprecationWarnings = pyBoolean(v)
	}
	if os.Getenv("ANSIBLE_INJECT_FACT_VARS") != "" {
		cfg.InjectFactsSet = true
	}
	if v := os.Getenv("ANSIBLE_ALLOW_BROKEN_CONDITIONALS"); v != "" {
		cfg.AllowBrokenConditionals = pyBoolean(v)
	}
	if v := os.Getenv("ANSIBLE_TASK_TIMEOUT"); v != "" {
		if n, ok := pyDecimalInt(v); ok {
			cfg.TaskTimeout = n
		}
	}
	if v := os.Getenv("ANSIBLE_DISPLAY_SKIPPED_HOSTS"); v != "" {
		cfg.DisplaySkippedHosts = pyBoolean(v)
	}
	if v := os.Getenv("ANSIBLE_SHOW_CUSTOM_STATS"); v != "" {
		cfg.ShowCustomStats = pyBoolean(v)
	}
	if v := os.Getenv("ANSIBLE_DEPRECATION_WARNINGS"); v != "" {
		cfg.DeprecationWarnings = pyBoolean(v)
	}
	for env, dst := range map[string]*bool{
		"ANSIBLE_LOCALHOST_WARNING":                &cfg.LocalhostWarning,
		"ANSIBLE_INVENTORY_UNPARSED_WARNING":       &cfg.InventoryUnparsedWarning,
		"ANSIBLE_INVENTORY_UNPARSED_FAILED":        &cfg.InventoryUnparsedIsFailed,
		"ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED": &cfg.InventoryAnyUnparsedIsFailed,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = pyBoolean(v)
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
