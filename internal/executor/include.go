package executor

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// includeUnit is one IncludedFile in Ansible's sense: a resolved task file
// (or role) plus the loop item it was included for, and the hosts that
// resolved to exactly that pair. Each unit's tasks run as their own block.
type includeUnit struct {
	target  string // absolute task-file path, or role name
	item    any
	index   int
	label   any
	hasItem bool
	hosts   []string
}

// runDynamicInclude implements include_tasks and include_role: per host
// (and per loop item) evaluate when:, resolve the target, group hosts by
// (target, item) in first-seen order, announce each group with an
// "included:" line, then run each group's tasks in turn — Ansible's
// process_include_results flow.
func (r *Runner) runDynamicInclude(ctx context.Context, play *playbook.Play, task *playbook.Task, active, playHosts []string, depth int) error {
	isRole := task.Module == "include_role"
	argKey := "file"
	if isRole {
		argKey = "name"
	}
	raw, _ := task.Args[argKey].(string)
	if raw == "" {
		return fmt.Errorf("%s:%d: %s requires a %s", task.Src.File, task.Src.Line, task.Module, argKey)
	}

	name := task.Name
	if name == "" && isRole {
		name = "include_role : " + raw
	}
	if name != "" {
		name = r.taskDisplayName(&playbook.Task{Name: name, RoleName: task.RoleName, Src: task.Src}, active)
	}
	r.Callback.TaskStart(task, name, false)

	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	var units []*includeUnit
	addUnit := func(u includeUnit, host string) {
		for _, existing := range units {
			if existing.target == u.target && existing.hasItem == u.hasItem &&
				existing.index == u.index && reflect.DeepEqual(existing.item, u.item) {
				existing.hosts = append(existing.hosts, host)
				return
			}
		}
		u.hosts = []string{host}
		units = append(units, &u)
	}

	for _, host := range active {
		base := r.newHostContext(host, pos, playHosts)
		if len(task.Vars) > 0 {
			base = base.WithOverlay(task.Vars)
		}
		items, isLoop, err := r.resolveLoop(task, base)
		if err != nil {
			r.recordFailure(host, task, agentproto.Fail("error templating loop: %v", err))
			continue
		}
		if !isLoop {
			items = []any{nil}
		}
		included := 0
		for i, item := range items {
			ictx := base
			if isLoop {
				overlay := map[string]any{task.LoopVar: vars.Final{V: item}}
				if task.IndexVar != "" {
					overlay[task.IndexVar] = int64(i)
				}
				ictx = base.WithOverlay(overlay)
			}
			if len(task.When) > 0 {
				ok, err := ictx.EvalWhen(task.When)
				if err != nil {
					r.record(host, task, agentproto.Fail("The conditional check failed: %v", err), nil)
					continue
				}
				if !ok {
					if isLoop {
						r.Callback.HostResult(host, task, &agentproto.Result{Skipped: true}, false, item)
					}
					continue
				}
			}
			rendered, err := ictx.TemplateString(raw)
			if err != nil {
				r.record(host, task, agentproto.Fail("error templating %s %s: %v", task.Module, argKey, err), nil)
				continue
			}
			target := fmt.Sprintf("%v", rendered)
			if !isRole {
				if target, err = r.resolveIncludePath(target, task); err != nil {
					r.record(host, task, agentproto.Fail("%v", err), nil)
					continue
				}
			}
			u := includeUnit{target: target, hasItem: isLoop}
			if isLoop {
				u.item, u.index, u.label = item, i, item
				if task.LoopLabel != nil {
					if l, err := ictx.TemplateValue(task.LoopLabel); err == nil {
						u.label = l
					}
				}
			}
			addUnit(u, host)
			included++
		}
		// Each inclusion is an ok result for the host; an include whose
		// when: excluded the host (every item) is a skip.
		if included == 0 {
			if !isLoop || len(items) > 0 {
				r.record(host, task, &agentproto.Result{Skipped: true}, nil)
			}
			continue
		}
		r.mu.Lock()
		r.stats[host].OK += included
		r.mu.Unlock()
	}

	for _, u := range units {
		r.Callback.Included(task, u.target, u.hosts, u.label, u.hasItem)
	}
	for _, u := range units {
		if r.playEnded {
			return nil
		}
		// Loop variables (and the include's vars:) scope the included tasks.
		scope := maps.Clone(task.Vars)
		if u.hasItem {
			if scope == nil {
				scope = map[string]any{}
			}
			scope[task.LoopVar] = vars.Final{V: u.item}
			if task.IndexVar != "" {
				scope[task.IndexVar] = int64(u.index)
			}
		}
		var tasks []*playbook.Task
		if isRole {
			ri, err := r.loadIncludedRole(play, task, u.target)
			if err != nil {
				for _, host := range u.hosts {
					r.record(host, task, agentproto.Fail("%s: %v", task.Module, err), nil)
				}
				continue
			}
			tasks = ri
		} else {
			loaded, err := playbook.LoadTaskFile(u.target, task.SrcDir)
			if err != nil {
				for _, host := range u.hosts {
					r.record(host, task, agentproto.Fail("include_tasks: %v", err), nil)
				}
				continue
			}
			tasks = loaded
		}
		for _, t := range tasks {
			if len(scope) > 0 {
				merged := maps.Clone(scope)
				maps.Copy(merged, t.Vars)
				t.Vars = merged
			}
			t.Tags = append(append([]string{}, task.Tags...), t.Tags...)
		}
		if err := r.runTaskList(ctx, play, tasks, playHosts, u.hosts, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// loadIncludedRole loads a role for include_role, layering its defaults and
// vars and registering its handlers, and returns its tasks.
func (r *Runner) loadIncludedRole(play *playbook.Play, task *playbook.Task, name string) ([]*playbook.Task, error) {
	tasksFrom, _ := task.Args["tasks_from"].(string)
	ri, err := playbook.LoadRoleForInclude(name, r.Opts.BaseDir, nil, tasksFrom)
	if err != nil {
		return nil, err
	}
	if len(ri.Defaults) > 0 {
		r.Store.AddRoleDefaults(ri.Defaults)
	}
	if len(ri.Vars) > 0 {
		r.Store.AddRoleVars(ri.Vars)
	}
	play.Handlers = append(play.Handlers, ri.Handlers...)
	return ri.Tasks, nil
}
