package executor

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// bypassesHostLoop reports an action whose plugin sets BYPASS_HOST_LOOP:
// the linear strategy sends the task to the first host only (as it does
// a run_once task), but its result, register and failure stay that
// host's.
func bypassesHostLoop(task *playbook.Task) bool {
	return task.Module == "add_host"
}

// noConnectionActions are the actions whose plugins set
// _requires_connection to False: they run on the controller without
// connecting to the host (or its delegate).
var noConnectionActions = map[string]bool{
	"add_host": true, "assert": true, "debug": true, "fail": true, "group_by": true,
	"include_vars": true, "set_fact": true, "set_stats": true, "validate_argument_spec": true,
}

// bypassLoopError is the free strategy's refusal of an action that
// bypasses the host loop, at the task.
type bypassLoopError struct{ task *playbook.Task }

func (e *bypassLoopError) Error() string { return e.Message() }

func (e *bypassLoopError) Message() string {
	return fmt.Sprintf("The '%s' module bypasses the host loop, which is currently not supported in the free strategy "+
		"and would instead execute for every host in the inventory list.", e.task.DisplayAction())
}

func (e *bypassLoopError) Origin() (string, int, int) {
	return e.task.Src.File, e.task.Src.Line, e.task.Src.Col
}

// freeStrategyCheck is the free strategy's check as a host queues task:
// an action bypassing the host loop ends the run; run_once warns that it
// is ignored. It reports whether the task may run.
func (r *Runner) freeStrategyCheck(play *playbook.Play, task *playbook.Task) bool {
	if !freeStrategy(play) {
		return true
	}
	if bypassesHostLoop(task) {
		r.fatal(&bypassLoopError{task: task})
		return false
	}
	if task.RunOnce {
		r.warn("Using run_once with the free strategy is not currently supported. This task will still be executed for every host in the inventory list.")
	}
	return true
}

// addHostSpecialArgs are the add_host arguments that are not host
// variables (host and group are, as in ansible-core).
var addHostSpecialArgs = map[string]bool{"name": true, "hostname": true, "groupname": true, "groups": true}

// runAddHost is the add_host action: the host (with its variables and
// groups) is added to the run's inventory at once.
func (r *Runner) runAddHost(task *playbook.Task, actx *actions.Context, args map[string]any) *agentproto.Result {
	newName, ok := firstArg(args, "name", "hostname", "host")
	if !ok || newName == nil {
		res := agentproto.Fail("name, host or hostname needs to be provided")
		res.Origin = "raised"
		return res
	}
	r.displayVerbose(2, "creating host via 'add_host': hostname="+template.PyStr(newName))
	name := template.PyStr(newName)
	port := -1
	if s, isStr := newName.(string); isStr {
		if host, p, err := inventory.ParseAddress(s, false); err == nil {
			name, port = host, p
		}
	}
	keys := orderedArgKeys(task, args)
	if port > 0 {
		if _, set := args["ansible_ssh_port"]; !set {
			keys = append(keys, "ansible_ssh_port")
		}
		args["ansible_ssh_port"] = int64(port)
	}

	var groups []string
	if raw, key := firstArgKey(args, "groupname", "groups", "group"); raw != nil && template.Truthy(raw) {
		var list []string
		switch t := raw.(type) {
		case []any:
			for _, g := range t {
				list = append(list, template.PyStr(g))
			}
		case string:
			list = strings.Split(t, ",")
		default:
			msg := "Groups must be specified as a list."
			res := agentproto.Fail("%s", msg)
			res.Origin = "verbatim"
			res.Msg = msg
			res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: msg}
			if p, ok := task.ArgPos[key]; ok {
				res.ErrorChain.InnerFile, res.ErrorChain.InnerLine, res.ErrorChain.InnerCol = p.File, p.Line, p.Col
			}
			return res
		}
		for _, g := range list {
			// The name as written is checked, the stripped one kept.
			if !slices.Contains(groups, g) {
				groups = append(groups, strings.TrimSpace(g))
			}
		}
	}
	if groups == nil {
		groups = []string{}
	}

	hostVars := yaml.NewOMap()
	var varKeys []string
	values := map[string]any{}
	for _, k := range keys {
		if addHostSpecialArgs[k] {
			continue
		}
		hostVars.Set(k, args[k])
		varKeys = append(varKeys, k)
		values[k] = args[k]
	}
	if name == "" {
		return raisedFailure("Invalid empty host name provided:")
	}
	changed, affected, err := r.Inv.AddDynamicHost(name, varKeys, values, groups)
	r.refreshInventoryVars(affected)
	if err != nil {
		return raisedFailure(err.Error())
	}
	if changed {
		r.addDynamicPlayHost(name)
	}
	groupList := make([]any, len(groups))
	for i, g := range groups {
		groupList[i] = g
	}
	added := yaml.NewOMap()
	added.Set("host_name", name)
	added.Set("groups", groupList)
	added.Set("host_vars", hostVars)
	return &agentproto.Result{Changed: changed, Extra: map[string]any{"add_host": added}}
}

// groupByValidArgs is group_by's _VALID_ARGS.
var groupByValidArgs = map[string]bool{"key": true, "parents": true}

// runGroupBy is the group_by action: the host joins the group named by
// key (created, under its parents, when new).
func (r *Runner) runGroupBy(actx *actions.Context, args map[string]any) *agentproto.Result {
	var bad []string
	for k := range args {
		if !groupByValidArgs[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		res := agentproto.Fail("Invalid options for group_by: %s", strings.Join(bad, ","))
		res.Origin = "raised"
		return res
	}
	key, ok := args["key"]
	if !ok {
		return &agentproto.Result{Failed: true, Msg: "the 'key' param is required when using group_by", Origin: "action"}
	}
	groupName := template.PyStr(key)
	var parents []string
	switch t := args["parents"].(type) {
	case nil:
		if _, set := args["parents"]; set {
			parents = []string{"None"}
		} else {
			parents = []string{"all"}
		}
	case []any:
		for _, p := range t {
			parents = append(parents, strings.ReplaceAll(template.PyStr(p), " ", "-"))
		}
	default:
		parents = []string{strings.ReplaceAll(template.PyStr(t), " ", "-")}
	}
	if parents == nil {
		parents = []string{}
	}
	changed, affected, err := r.Inv.AddDynamicGroup(actx.Host, groupName, parents)
	r.refreshInventoryVars(affected)
	if err != nil {
		return raisedFailure(err.Error())
	}
	parentList := make([]any, len(parents))
	for i, p := range parents {
		parentList[i] = p
	}
	return &agentproto.Result{Changed: changed, Extra: map[string]any{
		"add_group":     strings.ReplaceAll(groupName, " ", "-"),
		"parent_groups": parentList,
	}}
}

// raisedFailure is an action's unexpected exception: "Task failed: ..."
// is both the error and the result's msg.
func raisedFailure(msg string) *agentproto.Result {
	return &agentproto.Result{Failed: true, Msg: "Task failed: " + strings.TrimSpace(msg), Origin: "verbatim"}
}

// refreshInventoryVars installs the inventory variables of hosts an
// add_host or group_by changed.
func (r *Runner) refreshInventoryVars(hosts []string) {
	for _, name := range hosts {
		if h := r.Inv.Host(name); h != nil {
			r.Store.SetInventoryVars(name, r.Inv.EffectiveVars(h))
		}
	}
}

// addDynamicPlayHost records a host add_host added (or changed) during
// the batch: the strategy's play hosts cache takes it, so
// ansible_play_hosts(_all) list it from then on.
func (r *Runner) addDynamicPlayHost(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.dynPlayHosts, name) {
		r.dynPlayHosts = append(r.dynPlayHosts, name)
	}
}

// withDynamicPlayHosts is the play's hosts with those add_host added
// after them.
func (r *Runner) withDynamicPlayHosts(hosts []any) []any {
	r.mu.Lock()
	dyn := r.dynPlayHosts
	r.mu.Unlock()
	if len(dyn) == 0 {
		return hosts
	}
	out := append([]any(nil), hosts...)
	for _, h := range dyn {
		if !slices.Contains(out, any(h)) {
			out = append(out, h)
		}
	}
	return out
}

// firstArg is args.get(a, args.get(b, ...)): the first name set.
func firstArg(args map[string]any, names ...string) (any, bool) {
	v, key := firstArgKey(args, names...)
	return v, key != ""
}

func firstArgKey(args map[string]any, names ...string) (any, string) {
	for _, n := range names {
		if v, ok := args[n]; ok {
			return v, n
		}
	}
	return nil, ""
}

// orderedArgKeys lists the templated arguments in the order the task
// wrote them (k=v arguments, whose order is not kept, sorted).
func orderedArgKeys(task *playbook.Task, args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		pi, iok := task.ArgKeyPos[keys[i]]
		pj, jok := task.ArgKeyPos[keys[j]]
		if iok && jok && (pi.Line != pj.Line || pi.Col != pj.Col) {
			if pi.Line != pj.Line {
				return pi.Line < pj.Line
			}
			return pi.Col < pj.Col
		}
		if iok != jok {
			return iok
		}
		return keys[i] < keys[j]
	})
	return keys
}
