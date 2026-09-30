package playbook

import "strings"

// builtinModuleRedirects is ansible-core's ansible_builtin_runtime.yml
// plugin_routing for the modules understudy implements that moved to
// collections: a short name (or ansible.builtin./ansible.legacy.) resolves
// through the redirect, which the plugin loader announces at -vv.
var builtinModuleRedirects = map[string]string{
	"alternatives":    "community.general.alternatives",
	"apk":             "community.general.apk",
	"authorized_key":  "ansible.posix.authorized_key",
	"filesystem":      "community.general.filesystem",
	"firewalld":       "ansible.posix.firewalld",
	"ini_file":        "community.general.ini_file",
	"modprobe":        "community.general.modprobe",
	"mount":           "ansible.posix.mount",
	"mysql_db":        "community.mysql.mysql_db",
	"mysql_info":      "community.mysql.mysql_info",
	"mysql_query":     "community.mysql.mysql_query",
	"mysql_user":      "community.mysql.mysql_user",
	"mysql_variables": "community.mysql.mysql_variables",
	"openssh_keypair": "community.crypto.openssh_keypair",
	"parted":          "community.general.parted",
	"selinux":         "ansible.posix.selinux",
	"sysctl":          "ansible.posix.sysctl",
	"timezone":        "community.general.timezone",
}

// collectionModuleRedirects are collection runtime.yml redirects the
// plugin loader follows next (community.mysql moved to ansible.mysql).
var collectionModuleRedirects = map[string]string{
	"community.mysql.mysql_db":        "ansible.mysql.mysql_db",
	"community.mysql.mysql_info":      "ansible.mysql.mysql_info",
	"community.mysql.mysql_query":     "ansible.mysql.mysql_query",
	"community.mysql.mysql_user":      "ansible.mysql.mysql_user",
	"community.mysql.mysql_variables": "ansible.mysql.mysql_variables",
}

// builtinActionRedirects is the routing for action plugins.
var builtinActionRedirects = map[string]string{
	"yum": "ansible.builtin.dnf",
}

// builtinLookupRedirects is the same routing for lookup plugins.
var builtinLookupRedirects = map[string]string{
	"cartesian": "community.general.cartesian",
	"dig":       "community.general.dig",
	"flattened": "community.general.flattened",
}

// builtinShort is a builtin-routed plugin name's short form ("" for a
// collection FQCN).
func builtinShort(name string) string {
	short := strings.TrimPrefix(strings.TrimPrefix(name, "ansible.builtin."), "ansible.legacy.")
	if strings.Contains(short, ".") {
		return ""
	}
	return short
}

// ModuleRedirects are the plugin loader's -vv "redirecting" lines for a
// module resolved from its name as written: through the builtin routing,
// then any collection redirect (nil when it resolves directly).
func ModuleRedirects(action string) []string {
	var out []string
	name := action
	if short := builtinShort(action); short != "" {
		to, ok := builtinModuleRedirects[short]
		if !ok {
			return nil
		}
		out = append(out, "redirecting (type: modules) ansible.builtin."+short+" to "+to)
		name = to
	}
	if to, ok := collectionModuleRedirects[name]; ok {
		out = append(out, "redirecting (type: modules) "+name+" to "+to)
	}
	return out
}

// ActionRedirect is the -vv "redirecting" line for an action plugin
// resolved from its name as written ("" when it resolves directly).
func ActionRedirect(action string) string {
	short := builtinShort(action)
	to, ok := builtinActionRedirects[short]
	if short == "" || !ok {
		return ""
	}
	return "redirecting (type: action) ansible.builtin." + short + " to " + to
}

// LookupRedirect is the -vv "redirecting" line for a lookup plugin name
// as written ("" when it resolves directly).
func LookupRedirect(name string) string {
	short := builtinShort(name)
	to, ok := builtinLookupRedirects[short]
	if short == "" || !ok {
		return ""
	}
	return "redirecting (type: lookup) ansible.builtin." + short + " to " + to
}

// loadNotes are the -vv lines loading a task prints: its action's
// redirects (the action plugin's, else the module's), then its
// with_<lookup>'s.
func loadNotes(t *Task) []string {
	var out []string
	if a := ActionRedirect(t.Action); a != "" {
		out = append(out, a)
	} else {
		out = append(out, ModuleRedirects(t.Action)...)
	}
	if t.LoopWith != "" {
		if l := LookupRedirect(t.LoopWith); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ModuleRedirectNames lists the modules the builtin routing covers.
func ModuleRedirectNames() []string {
	out := make([]string, 0, len(builtinModuleRedirects))
	for k := range builtinModuleRedirects {
		out = append(out, k)
	}
	return out
}
