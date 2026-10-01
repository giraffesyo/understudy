package executor

import (
	"context"
	"fmt"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
)

// perHostMetas run (and print their banner) once per host under the linear
// strategy; every other meta runs once for the whole batch.
var perHostMetas = map[string]bool{
	"noop": true, "reset_connection": true, "end_host": true,
	"role_complete": true, "flush_handlers": true, "end_role": true,
}

// runMeta executes a meta task. Metas never touch the recap stats; a
// false when: prints a skipping line for that host.
func (r *Runner) runMeta(ctx context.Context, play *playbook.Play, task *playbook.Task, playHosts []string, restrict []string) error {
	action := task.FreeForm
	if action == "" {
		action = "noop"
	}
	switch action {
	case "noop", "flush_handlers", "end_host", "end_play", "end_batch",
		"clear_host_errors", "clear_facts", "reset_connection", "refresh_inventory":
	default:
		return fmt.Errorf("%s:%d: meta: %s is not supported yet", task.Src.File, task.Src.Line, action)
	}

	active := r.activeOf(playHosts)
	if restrict != nil {
		active = intersect(active, restrict)
	}
	if action == "clear_host_errors" {
		// Failed hosts are exactly the ones this meta brings back.
		active = r.notEnded(playHosts, restrict)
	}
	if len(active) == 0 {
		return nil
	}
	name := r.taskDisplayName(task, active)
	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	applies := func(host string) (bool, error) {
		if len(task.When) == 0 {
			return true, nil
		}
		return r.newHostContext(host, pos, playHosts).WithRoleScope(task.ScopeDefaults, task.ScopeVars).EvalWhen(task.When)
	}

	if perHostMetas[action] {
		var targets []string
		for _, host := range active {
			r.taskStart(task, name, false)
			ok, err := applies(host)
			if err != nil {
				return fmt.Errorf("%s:%d: meta %s: %v", task.Src.File, task.Src.Line, action, err)
			}
			if !ok {
				r.displayVerbose(2, "META: "+metaSkipHeader(action, host))
				r.Callback.HostResult(host, task, metaSkip(action, host), false, nil)
				continue
			}
			if action == "flush_handlers" {
				r.announceNotified(host)
			}
			r.displayVerbose(2, "META: "+metaMsg(action, host))
			targets = append(targets, host)
		}
		switch action {
		case "flush_handlers":
			if len(targets) > 0 {
				return r.flushHandlers(ctx, play, playHosts)
			}
		case "end_host":
			r.mu.Lock()
			for _, h := range targets {
				r.ended[h] = true
			}
			r.mu.Unlock()
		case "reset_connection":
			if r.Conns != nil {
				for _, h := range targets {
					r.Conns.Reset(h)
				}
			}
		}
		return nil
	}

	// Run-once metas: one banner, conditional evaluated on the first host.
	r.taskStart(task, name, false)
	ok, err := applies(active[0])
	if err != nil {
		return fmt.Errorf("%s:%d: meta %s: %v", task.Src.File, task.Src.Line, action, err)
	}
	if !ok {
		r.displayVerbose(2, "META: "+metaSkipHeader(action, active[0]))
		r.Callback.HostResult(active[0], task, metaSkip(action, active[0]), false, nil)
		return nil
	}
	r.displayVerbose(2, "META: "+metaMsg(action, active[0]))
	switch action {
	case "end_play":
		r.playEnded = true
	case "end_batch":
		r.batchEnded = true
	case "clear_host_errors":
		// Every play host's failed and unreachable state clears (so
		// they no longer count toward the exit code either). A failed
		// host has no tasks left in this play, though (its iterator
		// state is complete): it runs again from the next play.
		r.mu.Lock()
		for _, h := range playHosts {
			if r.failed[h] {
				r.ended[h] = true
			}
			delete(r.failed, h)
			delete(r.unreachable, h)
		}
		r.mu.Unlock()
	case "clear_facts":
		for _, h := range active {
			r.Store.ClearFacts(h)
		}
	case "refresh_inventory":
		// Inventories are static sources; there is nothing to re-read.
	}
	return nil
}

// notEnded lists play hosts that have not hit end_host (failed or not).
func (r *Runner) notEnded(playHosts, restrict []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range playHosts {
		if !r.ended[h] {
			out = append(out, h)
		}
	}
	if restrict != nil {
		out = intersect(out, restrict)
	}
	return out
}

// metaMsg is _execute_meta's message for a meta task that ran (shown
// as "META: <msg>" at -vv).
func metaMsg(action, host string) string {
	switch action {
	case "flush_handlers":
		return "triggered running handlers for " + host
	case "refresh_inventory":
		return "inventory successfully refreshed"
	case "clear_facts":
		return "facts cleared"
	case "clear_host_errors":
		return "cleared host errors"
	case "end_batch":
		return "ending batch"
	case "end_play":
		return "ending play"
	case "end_host":
		return "ending play for " + host
	case "reset_connection":
		return "reset connection"
	}
	return action
}

// metaSkipHeader is _execute_meta's skip_reason for a meta task whose
// when: is false.
func metaSkipHeader(action, host string) string {
	reason := action + " conditional evaluated to False"
	switch action {
	case "flush_handlers":
		reason += ", not running handlers for " + host
	case "clear_facts":
		reason += ", not clearing facts and fact cache for " + host
	case "clear_host_errors":
		reason += ", not clearing host error state for " + host
	case "end_batch":
		reason += ", continuing current batch"
	case "end_play":
		reason += ", continuing play"
	case "end_host":
		reason += ", continuing execution for " + host
	}
	return reason
}

// metaSkip is ansible-core's result for a meta task whose when: is false.
func metaSkip(action, host string) *agentproto.Result {
	res := &agentproto.Result{Skipped: true, Msg: action, Extra: map[string]any{
		"skip_reason": metaSkipHeader(action, host),
	}}
	if action == "end_host" {
		res.Msg = "end_host conditional evaluated to false, continuing execution for " + host
	}
	return res
}
