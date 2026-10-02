package config

import (
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
)

// typedSetting is one of ansible-core's base settings (config/base.yml)
// whose value can be rejected when the constants load: an integer or
// float, or one with choices. ini entries are "section.key".
type typedSetting struct {
	name    string
	typ     string // base.yml's type, as ensure_type names it ("" = none)
	ini     []string
	env     []string
	choices []string
}

// typedSettings are in base.yml's order, which is the order the
// constants load (the first bad one is the error).
var typedSettings = []typedSetting{
	{"_CALLBACK_DISPATCH_ERROR_BEHAVIOR", "choices", nil, []string{"_ANSIBLE_CALLBACK_DISPATCH_ERROR_BEHAVIOR"}, []string{"error", "warning", "ignore"}},
	{"CACHE_PLUGIN_TIMEOUT", "integer", []string{"defaults.fact_caching_timeout"}, []string{"ANSIBLE_CACHE_PLUGIN_TIMEOUT"}, nil},
	{"COLLECTIONS_ON_ANSIBLE_VERSION_MISMATCH", "", []string{"defaults.collections_on_ansible_version_mismatch"}, []string{"ANSIBLE_COLLECTIONS_ON_ANSIBLE_VERSION_MISMATCH"}, []string{"error", "warning", "ignore"}},
	{"LOG_VERBOSITY", "int", []string{"defaults.log_verbosity"}, []string{"ANSIBLE_LOG_VERBOSITY"}, nil},
	{"DEFAULT_FORKS", "integer", []string{"defaults.forks"}, []string{"ANSIBLE_FORKS"}, nil},
	{"DEFAULT_GATHERING", "", []string{"defaults.gathering"}, []string{"ANSIBLE_GATHERING"}, []string{"implicit", "explicit", "smart"}},
	{"DEFAULT_HASH_BEHAVIOUR", "string", []string{"defaults.hash_behaviour"}, []string{"ANSIBLE_HASH_BEHAVIOUR"}, []string{"replace", "merge"}},
	{"DEFAULT_INTERNAL_POLL_INTERVAL", "float", []string{"defaults.internal_poll_interval"}, nil, nil},
	{"DEFAULT_POLL_INTERVAL", "integer", []string{"defaults.poll_interval"}, []string{"ANSIBLE_POLL_INTERVAL"}, nil},
	{"DEFAULT_REMOTE_PORT", "integer", []string{"defaults.remote_port"}, []string{"ANSIBLE_REMOTE_PORT"}, nil},
	{"DEFAULT_TIMEOUT", "integer", []string{"defaults.timeout"}, []string{"ANSIBLE_TIMEOUT"}, nil},
	{"DEFAULT_VERBOSITY", "integer", []string{"defaults.verbosity"}, []string{"ANSIBLE_VERBOSITY"}, nil},
	{"DIFF_CONTEXT", "integer", []string{"diff.context"}, []string{"ANSIBLE_DIFF_CONTEXT"}, nil},
	{"DISPLAY_TRACEBACK", "list", []string{"defaults.display_traceback"}, []string{"ANSIBLE_DISPLAY_TRACEBACK"}, []string{"error", "warning", "deprecated", "deprecated_value", "always", "never"}},
	{"DUPLICATE_YAML_DICT_KEY", "string", []string{"defaults.duplicate_dict_key"}, []string{"ANSIBLE_DUPLICATE_YAML_DICT_KEY"}, []string{"error", "warn", "ignore"}},
	{"GALAXY_SERVER_TIMEOUT", "int", []string{"galaxy.server_timeout"}, []string{"ANSIBLE_GALAXY_SERVER_TIMEOUT"}, nil},
	{"GALAXY_IGNORE_INVALID_SIGNATURE_STATUS_CODES", "list", []string{"galaxy.ignore_signature_status_codes"}, []string{"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES"}, []string{"EXPSIG", "EXPKEYSIG", "REVKEYSIG", "BADSIG", "ERRSIG", "NO_PUBKEY", "MISSING_PASSPHRASE", "BAD_PASSPHRASE", "NODATA", "UNEXPECTED", "ERROR", "FAILURE", "BADARMOR", "KEYEXPIRED", "KEYREVOKED", "NO_SECKEY"}},
	{"GALAXY_COLLECTION_IMPORT_POLL_INTERVAL", "float", nil, []string{"ANSIBLE_GALAXY_COLLECTION_IMPORT_POLL_INTERVAL"}, nil},
	{"GALAXY_COLLECTION_IMPORT_POLL_FACTOR", "float", nil, []string{"ANSIBLE_GALAXY_COLLECTION_IMPORT_POLL_FACTOR"}, nil},
	{"HOST_PATTERN_MISMATCH", "", []string{"inventory.host_pattern_mismatch"}, []string{"ANSIBLE_HOST_PATTERN_MISMATCH"}, []string{"error", "warning", "ignore"}},
	{"TRANSFORM_INVALID_GROUP_CHARS", "string", []string{"defaults.force_valid_group_names"}, []string{"ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS"}, []string{"always", "never", "ignore", "silently"}},
	{"MAX_FILE_SIZE_FOR_DIFF", "int", []string{"defaults.max_diff_size"}, []string{"ANSIBLE_MAX_DIFF_SIZE"}, nil},
	{"PERSISTENT_CONNECT_TIMEOUT", "integer", []string{"persistent_connection.connect_timeout"}, []string{"ANSIBLE_PERSISTENT_CONNECT_TIMEOUT"}, nil},
	{"PERSISTENT_CONNECT_RETRY_TIMEOUT", "integer", []string{"persistent_connection.connect_retry_timeout"}, []string{"ANSIBLE_PERSISTENT_CONNECT_RETRY_TIMEOUT"}, nil},
	{"PERSISTENT_COMMAND_TIMEOUT", "int", []string{"persistent_connection.command_timeout"}, []string{"ANSIBLE_PERSISTENT_COMMAND_TIMEOUT"}, nil},
	{"PLAYBOOK_VARS_ROOT", "", []string{"defaults.playbook_vars_root"}, []string{"ANSIBLE_PLAYBOOK_VARS_ROOT"}, []string{"top", "bottom", "all"}},
	{"RUN_VARS_PLUGINS", "str", []string{"defaults.run_vars_plugins"}, []string{"ANSIBLE_RUN_VARS_PLUGINS"}, []string{"demand", "start"}},
	{"SSH_AGENT_KEY_LIFETIME", "int", []string{"connection.ssh_agent_key_lifetime"}, []string{"ANSIBLE_SSH_AGENT_KEY_LIFETIME"}, nil},
	{"TASK_TIMEOUT", "integer", []string{"defaults.task_timeout"}, []string{"ANSIBLE_TASK_TIMEOUT"}, nil},
	{"_TEMPLAR_SANDBOX_MODE", "choices", nil, []string{"_ANSIBLE_TEMPLAR_SANDBOX_MODE"}, []string{"default", "allow_unsafe_attributes"}},
	{"_TEMPLAR_UNKNOWN_TYPE_CONVERSION", "choices", nil, []string{"_ANSIBLE_TEMPLAR_UNKNOWN_TYPE_CONVERSION"}, []string{"error", "warning", "ignore"}},
	{"_TEMPLAR_UNKNOWN_TYPE_ENCOUNTERED", "choices", nil, []string{"_ANSIBLE_TEMPLAR_UNKNOWN_TYPE_ENCOUNTERED"}, []string{"error", "warning", "ignore"}},
	{"_TEMPLAR_UNTRUSTED_TEMPLATE_BEHAVIOR", "choices", nil, []string{"_ANSIBLE_TEMPLAR_UNTRUSTED_TEMPLATE_BEHAVIOR"}, []string{"error", "warning", "ignore"}},
	{"WORKER_SHUTDOWN_POLL_COUNT", "integer", nil, []string{"ANSIBLE_WORKER_SHUTDOWN_POLL_COUNT"}, nil},
	{"WORKER_SHUTDOWN_POLL_DELAY", "float", nil, []string{"ANSIBLE_WORKER_SHUTDOWN_POLL_DELAY"}, nil},
	{"WIN_ASYNC_STARTUP_TIMEOUT", "integer", []string{"defaults.win_async_startup_timeout"}, []string{"ANSIBLE_WIN_ASYNC_STARTUP_TIMEOUT"}, nil},
}

// checkTypedSettings is ConfigManager.get_config_value_and_origin's
// checks for each typed setting set (environment first, then the file):
// a value its type rejects ("Config 'X' from <origin> has an invalid
// value"; an empty environment variable falls back to the default
// instead), or one outside its choices.
func checkTypedSettings(f *iniFile, path string) error {
	for _, s := range typedSettings {
		val, origin, found, fromINI := "", "", false, false
		for _, e := range s.env {
			if v, ok := lookupEnv(e); ok {
				val, origin, found = v, "env: "+e, true
			}
		}
		if !found {
			for _, entry := range s.ini {
				section, key, _ := strings.Cut(entry, ".")
				if v, ok := f.get(section, key); ok {
					val, origin, found, fromINI = v, path, true, true
				}
			}
		}
		if !found {
			continue
		}
		invalid := func() error {
			return &Error{fmt.Sprintf("Config '%s' from %s has an invalid value: Invalid value provided for '%s': %s",
				s.name, pyRepr(origin), s.typ, pyRepr(val))}
		}
		switch s.typ {
		case "integer", "int":
			if _, ok := pyDecimalInt(val); !ok {
				if strings.HasPrefix(origin, "env:") && val == "" {
					continue // an empty variable: the default
				}
				return invalid()
			}
			continue
		case "float":
			if !pyFloat(val) {
				if strings.HasPrefix(origin, "env:") && val == "" {
					continue
				}
				return invalid()
			}
			continue
		}
		if len(s.choices) == 0 {
			continue
		}
		if s.typ == "list" {
			var items []string
			for _, x := range strings.Split(val, ",") {
				items = append(items, unquote(strings.TrimSpace(x)))
			}
			for _, x := range items {
				if !slices.Contains(s.choices, x) {
					reprs := make([]string, len(items))
					for i, it := range items {
						reprs[i] = pyRepr(it)
					}
					return choiceError("["+strings.Join(reprs, ", ")+"]", s)
				}
			}
			continue
		}
		if fromINI {
			val = unquote(val)
		}
		if !slices.Contains(s.choices, val) {
			return choiceError(pyRepr(val), s)
		}
	}
	return nil
}

func choiceError(repr string, s typedSetting) error {
	return &Error{fmt.Sprintf("Invalid value %s for config '%s'. Valid values are: %s", repr, s.name, strings.Join(s.choices, ", "))}
}

// unquote is ansible's unquote: one layer of matching quotes removed.
func unquote(s string) string {
	if len(s) > 1 && s[0] == s[len(s)-1] && (s[0] == '"' || s[0] == '\'') && s[len(s)-2] != '\\' {
		return s[1 : len(s)-1]
	}
	return s
}

// pyBoolean is boolean(value, strict=False): true for the true strings,
// false for anything else.
func pyBoolean(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "on", "1", "true", "t", "1.0":
		return true
	}
	return false
}

// pyDecimalRe is the decimal number syntax Python's Decimal() and float()
// accept (PEP 515 underscores included).
var pyDecimalRe = regexp.MustCompile(`^[+-]?(\d(_?\d)*(\.(\d(_?\d)*)?)?|\.\d(_?\d)*)([eE][+-]?\d(_?\d)*)?$`)

// pyDecimalInt is ensure_type's integer conversion of a string: a decimal
// number whose fraction is zero.
func pyDecimalInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if !pyDecimalRe.MatchString(s) {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(s, "_", ""))
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return int(r.Num().Int64()), true
}

// pyFloat reports whether float(s) accepts s.
func pyFloat(s string) bool {
	s = strings.TrimSpace(s)
	switch strings.ToLower(strings.TrimLeft(s, "+-")) {
	case "inf", "infinity", "nan":
		return true
	}
	return pyDecimalRe.MatchString(s)
}
