package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// magicVariableMapping is ansible-core's MAGIC_VARIABLE_MAPPING without
// its become entries, in its order: each PlayContext field and the
// variables that set it (the first one defined wins), which
// PlayContext.update_vars sets back under all of their names.
var magicVariableMapping = []struct {
	field string
	names []string
}{
	{"connection", []string{"ansible_connection"}},
	{"module_compression", []string{"ansible_module_compression"}},
	{"shell", []string{"ansible_shell_type"}},
	{"executable", []string{"ansible_shell_executable"}},
	{"remote_addr", []string{"ansible_ssh_host", "ansible_host"}},
	{"remote_user", []string{"ansible_ssh_user", "ansible_user"}},
	{"password", []string{"ansible_ssh_pass", "ansible_password"}},
	{"port", []string{"ansible_ssh_port", "ansible_port"}},
	{"pipelining", []string{"ansible_ssh_pipelining", "ansible_pipelining"}},
	{"timeout", []string{"ansible_ssh_timeout", "ansible_timeout"}},
	{"private_key_file", []string{"ansible_ssh_private_key_file", "ansible_private_key_file"}},
	{"network_os", []string{"ansible_network_os"}},
	{"connection_user", []string{"ansible_connection_user"}},
	{"docker_extra_args", []string{"ansible_docker_extra_args"}},
}

// commonConnectionVars are COMMON_CONNECTION_VARS (ansible_password,
// never set back, and ansible_shell_type, which the shell plugin leaves
// unset, left out) with the PlayContext field each falls back to.
var commonConnectionVars = []struct{ name, field string }{
	{"ansible_host", "remote_addr"},
	{"ansible_user", "remote_user"},
	{"ansible_port", "port"},
	{"ansible_pipelining", "pipelining"},
	{"ansible_timeout", "timeout"},
	{"ansible_module_compression", "module_compression"},
	{"ansible_shell_executable", "executable"},
	{"ansible_private_key_file", "private_key_file"},
}

// localhostNames is C.LOCALHOST.
var localhostNames = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}

// hostOptionConnections are the connection plugins with a host option
// (ansible_host among its variables): the default callback names a
// delegated host's address after it when it differs from the name.
var hostOptionConnections = map[string]bool{"ssh": true, "paramiko": true, "paramiko_ssh": true, "winrm": true, "psrp": true}

// connectionContext is what one task execution's connection settings
// add to its variables.
type connectionContext struct {
	// magic: PlayContext.update_vars, for the when conditional and the
	// task's arguments and keywords.
	magic map[string]any
	// common: ConnectionBase.update_vars, for what the action templates
	// as it runs (debug's var, assert's that) and changed_when,
	// failed_when and until.
	common map[string]any
	// address is the delegated host's address in the result's label
	// ("h1 -> d1(192.0.2.1)"), when it differs from its name.
	address string
}

// connectionVars resolves the PlayContext of task on host (delegated to
// target when that differs) as set_task_and_variable_override does: the
// play's and task's keywords and the command line, then the variables
// (the delegated host's when delegating) of MAGIC_VARIABLE_MAPPING.
func (r *Runner) connectionVars(play *playbook.Play, task *playbook.Task, host, target string, vctx *vars.Context, playHosts []string) connectionContext {
	pc := map[string]any{
		"module_compression": "ZIP_DEFLATED",
		"executable":         "/bin/sh",
		"pipelining":         false,
	}
	defaultTransport := "ssh"
	keywordConn := firstNonEmpty(task.Connection, play.Connection)
	cliConn := ""
	timeout := 10
	if r.Conns != nil {
		cliConn = r.Conns.Opts.Connection
		if t := r.Conns.Opts.Timeout; t > 0 {
			timeout = int(t / time.Second)
		}
		if k := r.Conns.Opts.PrivateKey; k != "" {
			pc["private_key_file"] = k
		}
		if pw := r.Conns.Opts.Password; pw != "" {
			pc["password"] = pw
		}
	}
	taskConn := firstNonEmpty(keywordConn, cliConn, defaultTransport)
	if taskConn == "smart" {
		taskConn = defaultTransport
	}
	pc["connection"] = taskConn
	pc["timeout"] = timeout
	remoteUser := firstNonEmpty(task.RemoteUser, play.RemoteUser)
	if remoteUser == "" && r.Conns != nil {
		remoteUser = r.Conns.Opts.RemoteUser
	}
	if remoteUser != "" {
		pc["remote_user"] = remoteUser
	}

	get := func(c *vars.Context, name string) (any, bool) {
		return c.AsMapping().GetItem(name)
	}
	src := vctx
	delegated := target != host
	extra := map[string]any{} // the delegated variables set_task_and_variable_override adds
	if delegated {
		pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
		src = r.newHostContext(target, pos, playHosts).WithRoleScope(task.ScopeDefaults, task.ScopeVars)
		if len(task.Vars) > 0 {
			src = src.WithOverlay(task.Vars)
		}
		transport := defaultTransport
		if v, ok := get(src, "ansible_connection"); ok {
			transport = fmt.Sprint(v)
		}
		if !anyDefined(src, "ansible_"+transport+"_host", "ansible_ssh_host", "ansible_host") {
			extra["ansible_host"] = target
		}
		if !anyDefined(src, "ansible_"+transport+"_port", "ansible_ssh_port", "ansible_port") {
			if transport == "winrm" {
				extra["ansible_port"] = int64(5986)
			} else {
				extra["ansible_port"] = nil
			}
		}
		userSet := false
		for _, n := range []string{"ansible_" + transport + "_user", "ansible_ssh_user", "ansible_user"} {
			if v, ok := get(src, n); ok && template.Truthy(v) {
				userSet = true
				break
			}
		}
		if !userSet {
			if remoteUser != "" {
				extra["ansible_user"] = remoteUser
			} else {
				extra["ansible_user"] = nil
			}
		}
	}
	lookup := func(name string) (any, bool) {
		if v, ok := extra[name]; ok {
			return v, true
		}
		return get(src, name)
	}
	for _, m := range magicVariableMapping {
		for _, n := range m.names {
			if v, ok := lookup(n); ok {
				pc[m.field] = v
				break
			}
		}
	}
	if delegated {
		if _, ok := lookup("ansible_connection"); !ok {
			remoteLocal := localhostNames[fmt.Sprint(pc["remote_addr"])]
			invLocal := localhostNames[host]
			if remoteLocal && invLocal {
				pc["connection"] = "local"
			} else if pc["connection"] == "local" {
				pc["connection"] = defaultTransport
			}
		}
	}
	if pc["connection"] == "local" {
		if cu, ok := pc["connection_user"]; !ok || !template.Truthy(cu) {
			pc["connection_user"] = pc["remote_user"]
		}
	}
	if a, ok := pc["remote_addr"]; !ok || a == nil || a == "" {
		pc["remote_addr"] = host
	}

	out := connectionContext{magic: map[string]any{}, common: map[string]any{}}
	for _, m := range magicVariableMapping {
		v, ok := pc[m.field]
		if !ok || v == nil {
			continue
		}
		for _, n := range m.names {
			out.magic[n] = v
		}
	}
	// The connection plugin's name: the connection variable of the host
	// it connects to, else the task's connection.
	current := taskConn
	if v, ok := get(src, "ansible_connection"); ok && v != nil {
		current = fmt.Sprint(v)
	}
	out.common["ansible_connection"] = current
	for _, c := range commonConnectionVars {
		if v, ok := pc[c.field]; ok && v != nil {
			out.common[c.name] = v
		}
	}
	if delegated && hostOptionConnections[pluginShortName(current)] {
		// The host option: the last of inventory_hostname, ansible_host
		// and ansible_ssh_host defined.
		addr := target
		for _, n := range []string{"ansible_host", "ansible_ssh_host"} {
			if v, ok := lookup(n); ok && v != nil {
				addr = fmt.Sprint(v)
			}
		}
		if addr != target {
			out.address = addr
		}
	}
	return out
}

// delegatedLabel labels a result that failed before its connection was
// set up with the host the task delegates to (target), as the callback
// labels it from the task's delegate_to.
func delegatedLabel(res *agentproto.Result, host, target string) *agentproto.Result {
	if res != nil && target != host {
		res.DelegatedTo = target
	}
	return res
}

// delegateFailure is the task's result when its delegate_to (at pos)
// does not template: the task fails, raised at delegate_to, its result
// naming the delegate as written (nil: not a template error).
func delegateFailure(task *playbook.Task, host string, pos template.Position, err error) *agentproto.Result {
	cause, ok := template.Cause(err)
	if !ok {
		return nil
	}
	res := agentproto.Fail("Task failed: %s", cause)
	res.Origin = "verbatim"
	res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: cause,
		InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col}
	return delegatedLabel(res, host, task.Delegate)
}

// anyDefined reports whether any of names is defined in c.
func anyDefined(c *vars.Context, names ...string) bool {
	for _, n := range names {
		if c.Has(n) {
			return true
		}
	}
	return false
}

// pluginShortName is a plugin's name without its collection.
func pluginShortName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}
