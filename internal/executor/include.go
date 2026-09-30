package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
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
	vars    map[string]any // the item's loop variables
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
	if name == "" {
		name = task.DisplayAction()
	}
	name = r.taskDisplayName(&playbook.Task{Name: name, RoleName: task.RoleName, Src: task.Src}, active)
	r.taskStart(task, name, false)

	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	var units []*includeUnit
	addUnit := func(u includeUnit, host string) {
		for _, existing := range units {
			if existing.target == u.target && existing.hasItem == u.hasItem &&
				existing.index == u.index && reflect.DeepEqual(existing.vars, u.vars) {
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
		var lc *loopControl
		if isLoop {
			if lc, err = newLoopControl(task, base, items); err != nil {
				r.recordFailure(host, task, agentproto.Fail("%v", err))
				continue
			}
		} else {
			items = []any{nil}
		}
		included := 0
		var lastSkip *agentproto.Result
		var itemResults []any
		for i, item := range items {
			ictx := base
			var loopVars map[string]any
			if isLoop {
				loopVars = lc.vars(i)
				ictx = base.WithOverlay(loopVars)
			}
			skip, err := whenSkip(ictx, task.When, task.WhenPos)
			if err != nil {
				r.record(host, task, agentproto.Fail("The conditional check failed: %v", err), nil)
				continue
			}
			if skip != nil {
				if isLoop {
					lc.annotate(skip.Extra, i)
					r.Callback.HostResult(host, task, shown(task, skip), false, lc.label(ictx, i))
					itemResults = append(itemResults, orderedResult(task, task.Module, skip.ToVars()))
				} else {
					lastSkip = skip
				}
				continue
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
				if abs, err := filepath.Abs(target); err == nil {
					target = abs // "included:" lines show absolute paths
				}
			}
			u := includeUnit{target: target, hasItem: isLoop, vars: loopVars}
			if isLoop {
				u.item, u.index, u.label = item, i, lc.label(ictx, i)
				// An inclusion is an ok result for its item.
				ok := &agentproto.Result{Extra: map[string]any{}}
				lc.annotate(ok.Extra, i)
				itemResults = append(itemResults, orderedResult(task, task.Module, ok.ToVars()))
			}
			addUnit(u, host)
			included++
		}
		if isLoop && itemResults == nil {
			itemResults = []any{}
		}
		// Each inclusion is an ok result for the host; an include whose
		// when: excluded the host (every item) is a skip.
		if included == 0 {
			switch {
			case isLoop:
				r.record(host, task, loopResult(itemResults, false, false, true), itemResults)
			default:
				if lastSkip == nil {
					lastSkip = &agentproto.Result{Skipped: true}
				}
				r.record(host, task, lastSkip, nil)
			}
			continue
		}
		r.mu.Lock()
		r.stats[host].OK += included
		r.mu.Unlock()
		if task.Register != "" {
			// An include's result holds only the flags (its file and args
			// are not result fields); a loop's, its items' too.
			reg := yaml.NewOMap()
			reg.Set("changed", false)
			reg.Set("failed", false)
			if isLoop {
				reg = orderedResult(task, task.Module, loopResult(itemResults, false, false, false).ToVars())
			}
			r.Store.SetHostFact(host, task.Register, reg)
		}
	}

	// Load the included files first: one that fails to parse ends the run
	// (ansible-core re-raises parser errors from includes), before any
	// "included:" line.
	loaded := make([][]*playbook.Task, len(units))
	loadErrs := make([]error, len(units))
	for i, u := range units {
		if isRole {
			loaded[i], loadErrs[i] = r.loadIncludedRole(play, task, u.target)
		} else {
			loaded[i], loadErrs[i] = playbook.LoadTaskFile(u.target, task.SrcDir)
		}
		var ye *yaml.Error
		if errors.As(loadErrs[i], &ye) {
			return loadErrs[i]
		}
		if loadErrs[i] == nil && task.ApplyErr != nil {
			return task.ApplyErr // apply: loads with the included file
		}
	}
	for _, u := range units {
		if task.NoLog {
			// The included file's vars carry _ansible_no_log, which the
			// callback's item label censors, loop or not.
			r.Callback.Included(task, u.target, u.hosts, "(censored due to no_log)", true)
			continue
		}
		r.Callback.Included(task, u.target, u.hosts, u.label, u.hasItem)
	}
	for ui, u := range units {
		if r.playEnded {
			return nil
		}
		// Loop variables (and the include's vars:) scope the included tasks.
		scope := maps.Clone(task.Vars)
		if len(u.vars) > 0 {
			if scope == nil {
				scope = map[string]any{}
			}
			maps.Copy(scope, u.vars)
		}
		tasks, err := loaded[ui], loadErrs[ui]
		if err != nil {
			for _, host := range u.hosts {
				if isRole {
					r.record(host, task, agentproto.Fail("%s: %v", task.Module, err), nil)
				} else {
					r.record(host, task, agentproto.Fail("include_tasks: %v", err), nil)
				}
			}
			continue
		}
		r.adoptBlocks(task, tasks)
		for _, t := range tasks {
			if !isRole && t.RoleName == "" {
				// Tasks included from a role belong to that role.
				t.RoleName = task.RoleName
			}
			if len(scope) > 0 {
				merged := maps.Clone(scope)
				maps.Copy(merged, t.Vars)
				t.Vars = merged
			}
			// The include's own keywords stay on the include; its tasks
			// inherit its apply: keywords, then what its enclosing blocks
			// and role pass down.
			if task.Apply != nil {
				playbook.Inherit(t, task.Apply)
			}
			if task.Parents != nil {
				playbook.Inherit(t, task.Parents)
			}
		}
		if err := r.runTaskList(ctx, play, tasks, playHosts, u.hosts, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// adoptBlocks places included tasks inside the include's enclosing blocks,
// so a failure in an included file or role is rescued (and its always
// runs) like any task of the block. The included file's own block IDs
// are renumbered: each file numbers its blocks from 0, which would
// otherwise collide with the including file's blocks.
func (r *Runner) adoptBlocks(include *playbook.Task, tasks []*playbook.Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	remap := map[int]int{}
	for _, t := range tasks {
		refs := make([]playbook.BlockRef, 0, len(include.Blocks)+len(t.Blocks))
		refs = append(refs, include.Blocks...)
		for _, b := range t.Blocks {
			id, ok := remap[b.ID]
			if !ok {
				r.nextBlockID++
				id = r.nextBlockID
				remap[b.ID] = id
			}
			b.ID = id
			refs = append(refs, b)
		}
		t.Blocks = refs
	}
}

// loadIncludedRole loads a role for include_role, layering its defaults and
// vars and registering its handlers, and returns its tasks.
func (r *Runner) loadIncludedRole(play *playbook.Play, task *playbook.Task, name string) ([]*playbook.Task, error) {
	tasksFrom, _ := task.Args["tasks_from"].(string)
	ri, err := playbook.LoadRoleForInclude(name, r.Opts.BaseDir, r.Opts.RolesPath, tasksFrom)
	if err != nil {
		return nil, err
	}
	if len(ri.Defaults) > 0 {
		r.Store.AddRoleDefaults(ri.Defaults)
	}
	if len(ri.Vars) > 0 {
		r.Store.AddRoleVars(ri.Vars)
	}
	r.mu.Lock() // include_role may run concurrently (parallel blocks)
	play.Handlers = append(play.Handlers, ri.Handlers...)
	r.mu.Unlock()
	return ri.Tasks, nil
}
