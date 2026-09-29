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
		return r.newHostContext(host, pos, playHosts).EvalWhen(task.When)
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
				r.Callback.HostResult(host, task, &agentproto.Result{Skipped: true}, false, nil)
				continue
			}
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
		r.Callback.HostResult(active[0], task, &agentproto.Result{Skipped: true}, false, nil)
		return nil
	}
	switch action {
	case "end_play":
		r.playEnded = true
	case "end_batch":
		r.batchEnded = true
	case "clear_host_errors":
		r.mu.Lock()
		for _, h := range active {
			delete(r.failed, h)
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
