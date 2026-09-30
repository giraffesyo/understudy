package executor

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// loopControl is a task's loop_control for one host's loop, post-validated
// (extended and extended_allitems templated and made booleans).
type loopControl struct {
	task     *playbook.Task
	items    []any
	extended bool
	allItems bool
}

// newLoopControl resolves loop_control.extended (default false) and
// extended_allitems (default true) for a loop over items.
func newLoopControl(task *playbook.Task, vctx *vars.Context, items []any) (*loopControl, error) {
	lc := &loopControl{task: task, items: items, allItems: true}
	var err error
	if lc.extended, err = loopFlag(task.LoopExtended, false, "extended", vctx); err != nil {
		return nil, err
	}
	if lc.allItems, err = loopFlag(task.LoopAllItems, true, "extended_allitems", vctx); err != nil {
		return nil, err
	}
	return lc, nil
}

// loopFlag is a boolean loop_control field: a bool, a boolean spelling or
// a template resolving to one.
func loopFlag(raw any, def bool, key string, vctx *vars.Context) (bool, error) {
	if raw == nil {
		return def, nil
	}
	v := raw
	if s, ok := raw.(string); ok && (strings.Contains(s, "{{") || strings.Contains(s, "{%")) {
		var err error
		if v, err = vctx.TemplateValue(raw); err != nil {
			return false, err
		}
		v = template.Undeprecate(v)
	}
	if v == nil {
		return def, nil
	}
	if b, ok := playbook.ParseBool(v); ok {
		return b, nil
	}
	if s, ok := v.(string); ok {
		if b, ok := playbook.ParseBool(strings.TrimSpace(s)); ok {
			return b, nil
		}
	}
	return false, fmt.Errorf("the field '%s' has an invalid value (%s), and could not be converted to bool", key, template.PyStr(v))
}

// vars are the loop variables TaskContext.start_loop sets for item i:
// the loop and index variables, ansible_loop_var/ansible_index_var and,
// when extended, ansible_loop.
func (lc *loopControl) vars(i int) map[string]any {
	t := lc.task
	out := map[string]any{t.LoopVar: vars.Final{V: lc.items[i]}, "ansible_loop_var": t.LoopVar}
	if t.IndexVar != "" {
		out["ansible_index_var"] = t.IndexVar
		out[t.IndexVar] = int64(i)
	}
	if lp := lc.ansibleLoop(i); lp != nil {
		out["ansible_loop"] = vars.Final{V: lp}
	}
	return out
}

// annotate adds the loop variables to item i's result, as
// UnifiedTaskResult carries them.
func (lc *loopControl) annotate(extra map[string]any, i int) {
	t := lc.task
	extra["ansible_loop_var"] = t.LoopVar
	extra[t.LoopVar] = lc.items[i]
	if t.IndexVar != "" {
		extra["ansible_index_var"] = t.IndexVar
		extra[t.IndexVar] = int64(i)
	}
	if lp := lc.ansibleLoop(i); lp != nil {
		extra["ansible_loop"] = lp
	}
}

// label is item i's display label: loop_control.label templated with the
// item's variables, else the item.
func (lc *loopControl) label(itemCtx *vars.Context, i int) any {
	if lc.task.LoopLabel != nil {
		if l, err := itemCtx.TemplateValue(lc.task.LoopLabel); err == nil {
			return l
		}
	}
	return lc.items[i]
}

// ansibleLoop is the extended loop variable for item i (nil unless
// loop_control.extended), with start_loop's key order.
func (lc *loopControl) ansibleLoop(i int) *yaml.OMap {
	if !lc.extended {
		return nil
	}
	n := len(lc.items)
	m := yaml.NewOMap()
	m.Set("index", int64(i+1))
	m.Set("index0", int64(i))
	m.Set("first", i == 0)
	m.Set("last", i+1 == n)
	m.Set("length", int64(n))
	m.Set("revindex", int64(n-i))
	m.Set("revindex0", int64(n-i-1))
	if lc.allItems {
		m.Set("allitems", lc.items)
	}
	if i+1 < n {
		m.Set("nextitem", lc.items[i+1])
	}
	if i > 0 {
		m.Set("previtem", lc.items[i-1])
	}
	return m
}
