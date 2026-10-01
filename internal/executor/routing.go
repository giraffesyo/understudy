package executor

import (
	"fmt"
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

// RoutingDeprecationWarnings renders the deprecation warnings ansible-core's
// plugin loader prints (to stderr, once each) while it resolves the plays'
// tasks: an FQCN through a deprecated redirect is reported at the task's
// origin, a short name routed through ansible.builtin at an unknown one.
// The first is preceded by the "can be disabled" hint.
func RoutingDeprecationWarnings(plays []*playbook.Play) []string {
	var tasks []*playbook.Task
	for _, pl := range plays {
		for _, list := range [][]*playbook.Task{pl.PreTasks, pl.Tasks, pl.PostTasks, pl.Handlers} {
			tasks = append(tasks, list...)
		}
	}
	return TaskDeprecationWarnings(tasks)
}

// TaskDeprecationWarnings are the deprecation warnings loading and
// resolving tasks raises, in their order (those a playbook's load began
// before it failed print before its error).
func TaskDeprecationWarnings(tasks []*playbook.Task) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tasks {
		for _, d := range t.LoadDeprecations {
			a := d.Pos
			msg := fmt.Sprintf("[DEPRECATION WARNING]: %s This feature will be removed from ansible-core version %s.\n"+
				"Origin: %s:%d:%d\n\n%s\n%s\n\n", d.Msg, d.Version, a.File, a.Line, a.Col, playbook.SourceContext(a.File, a.Line, a.Col), d.Help)
			if !seen[msg] {
				seen[msg] = true
				out = append(out, DeprecationHint()+msg)
			}
		}
		d, ok := routingDeprecations[t.Module]
		if !ok {
			continue
		}
		old := d.collection + "." + t.Module
		head := "[DEPRECATION WARNING]: " + old + " has been deprecated. Use " + d.redirectTo +
			" instead. This feature will be removed from collection '" + d.collection + "' version " + d.version + ".\n"
		var msg string
		switch t.Action {
		case old:
			msg = fmt.Sprintf("%sOrigin: %s:%d:%d\n\n%s\n", head, t.Src.File, t.Src.Line, t.Src.Col,
				playbook.SourceContext(t.Src.File, t.Src.Line, t.Src.Col))
		case t.Module, "ansible.legacy." + t.Module:
			msg = head + "Origin: <unknown>\n\n" + old + "\n\n"
		default:
			continue
		}
		if seen[msg] {
			continue
		}
		seen[msg] = true
		out = append(out, DeprecationHint()+msg)
	}
	return out
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

// nameUnsupportedParams names the module in AnsibleModule's "Unsupported
// parameters for (...) module" failure as the task wrote it; modules
// report their short name (a backend run through an action names itself
// ansible.legacy.<module> and is left alone).
func nameUnsupportedParams(task *playbook.Task, res *agentproto.Result) {
	pre := "Unsupported parameters for (" + task.Module + ") module: "
	if res == nil || !res.Failed || task.Action == "" || task.Action == task.Module || !strings.HasPrefix(res.Msg, pre) {
		return
	}
	res.Msg = "Unsupported parameters for (" + task.Action + ") module: " + res.Msg[len(pre):]
}
