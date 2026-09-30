package executor

import (
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// routingDeprecation is a collection's plugin_routing entry that
// redirects a module elsewhere with a deprecation notice.
type routingDeprecation struct {
	collection, version, redirectTo string
}

// routingDeprecations are the deprecated module redirects ansible-core
// follows for implemented modules: the short (builtin-routed) name and
// the old collection FQCN resolve through them; the new FQCN does not.
var routingDeprecations = map[string]routingDeprecation{}

func init() {
	for _, m := range []string{"mysql_db", "mysql_user", "mysql_query", "mysql_variables", "mysql_info"} {
		routingDeprecations[m] = routingDeprecation{collection: "community.mysql", version: "6.0.0", redirectTo: "ansible.mysql." + m}
	}
}

// addRoutingDeprecation records the redirect's deprecation on the module
// result, as ansible-core's plugin loader does for the task.
func addRoutingDeprecation(task *playbook.Task, res *agentproto.Result) {
	d, ok := routingDeprecations[task.Module]
	if !ok || res == nil {
		return
	}
	action := task.Action
	if action == "" {
		action = task.Module
	}
	if action != task.Module && action != d.collection+"."+task.Module && action != "ansible.legacy."+task.Module {
		return
	}
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	list, _ := res.Extra["deprecations"].([]any)
	res.Extra["deprecations"] = append(list, map[string]any{
		"collection_name": d.collection,
		"deprecator":      map[string]any{"resolved_name": d.collection, "type": nil},
		"msg":             d.collection + "." + task.Module + " has been deprecated. Use " + d.redirectTo + " instead.",
		"version":         d.version,
	})
}

// nameCheckModeSkip names the module in "remote module (...) does not
// support check mode" as the task wrote it (AnsibleModule reports its
// _ansible_module_name: the action as written, FQCN or not); modules only
// see the short name.
func nameCheckModeSkip(task *playbook.Task, res *agentproto.Result) {
	const pre, suf = "remote module (", ") does not support check mode"
	if res == nil || !res.Skipped || task.Action == "" || !strings.HasPrefix(res.Msg, pre) {
		return
	}
	rest := res.Msg[len(pre):]
	if i := strings.Index(rest, suf); i >= 0 {
		res.Msg = pre + task.Action + rest[i:]
	}
}
