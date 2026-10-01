package executor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
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
	pause    time.Duration
}

// loopControlError is a loop_control field that failed post-validation:
// the task's result.
type loopControlError struct{ res *agentproto.Result }

func (e *loopControlError) Error() string { return e.res.Msg }

// loopControlFailure is the task result for a loop_control that failed
// to validate.
func loopControlFailure(err error) *agentproto.Result {
	var le *loopControlError
	if errors.As(err, &le) {
		return le.res
	}
	return agentproto.Fail("%v", err)
}

// newLoopControl post-validates loop_control for a loop over items, in
// the order LoopControl declares its fields: pause (a float, default 0),
// extended (default false) and extended_allitems (default true).
func newLoopControl(task *playbook.Task, vctx *vars.Context, items []any) (*loopControl, error) {
	lc := &loopControl{task: task, items: items, allItems: true}
	var err error
	if lc.pause, err = loopPause(task, vctx); err != nil {
		return nil, err
	}
	// LoopControl.post_validate then checks the variable names.
	for _, lv := range []struct{ key, name string }{{"loop_var", task.LoopVar}, {"index_var", task.IndexVar}} {
		if lv.name == "" || playbook.ValidVariableName(lv.name) {
			continue
		}
		msg, help := playbook.InvalidVariableName(lv.name)
		outer := fmt.Sprintf("Invalid '%s'.", lv.key)
		res := agentproto.Fail("Invalid '%s': %s", lv.key, msg)
		p := task.KeywordPos["loop_control."+lv.key]
		res.ErrorChain = &agentproto.ErrorChain{Outer: outer, OuterUnlocated: true, Inner: msg, Help: help,
			InnerFile: p.File, InnerLine: p.Line, InnerCol: p.Col}
		return nil, &loopControlError{res}
	}
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
	if lc.task.IndexVar == lc.task.LoopVar {
		return int64(i) // the index variable overwrote the item's
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

// loopPause is loop_control.pause: seconds, converted as a FieldAttribute
// of isa 'float' does.
func loopPause(task *playbook.Task, vctx *vars.Context) (time.Duration, error) {
	raw := task.LoopPause
	if raw == nil {
		return 0, nil
	}
	pos := task.KeywordPos["loop_control.pause"]
	v := raw
	if s, ok := raw.(string); ok && (strings.Contains(s, "{{") || strings.Contains(s, "{%")) {
		var err error
		if v, err = vctx.At(pos).TemplateValue(raw); err != nil {
			return 0, err
		}
		v = template.Undeprecate(v)
	}
	var f float64
	ok := true
	switch t := v.(type) {
	case nil:
	case bool:
		if t {
			f = 1
		}
	case int64:
		f = float64(t)
	case int:
		f = float64(t)
	case float64:
		f = t
	case yaml.UnsafeString:
		f, ok = pyFloat(string(t))
	case string:
		f, ok = pyFloat(t)
	default:
		ok = false
	}
	if !ok {
		inner := fmt.Sprintf("The value %s could not be converted to 'float'.", template.PyRepr(v))
		res := agentproto.Fail("Error processing keyword 'pause': %s", inner)
		res.ErrorChain = &agentproto.ErrorChain{
			Outer: "Error processing keyword 'pause'.", OuterFile: pos.File, OuterLine: pos.Line, OuterCol: pos.Col,
			Inner: inner, InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col,
		}
		return 0, &loopControlError{res}
	}
	if f <= 0 {
		return 0, nil
	}
	return time.Duration(f * float64(time.Second)), nil
}

// pyFloat is Python's float() of a string.
func pyFloat(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "_", "")
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// connectionVars is ansible-core's COMMON_CONNECTION_VARS.
var connectionVars = map[string]bool{
	"ansible_connection": true, "ansible_host": true, "ansible_user": true, "ansible_shell_executable": true,
	"ansible_port": true, "ansible_pipelining": true, "ansible_password": true, "ansible_timeout": true,
	"ansible_shell_type": true, "ansible_module_compression": true, "ansible_private_key_file": true,
}

// checkLoopControl is TaskExecutor._check_loop_control: a loop or index
// variable that shadows a variable already defined for the task, is
// reserved, or names both is warned about.
func (r *Runner) checkLoopControl(task *playbook.Task, vctx *vars.Context) {
	type loopVariable struct{ key, name string }
	vars := []loopVariable{{"loop_var", task.LoopVar}, {"index_var", task.IndexVar}}
	for _, lv := range vars {
		if lv.name == "" {
			continue
		}
		var conflict string
		_, defined := vctx.Get(lv.name)
		switch {
		case defined:
			conflict = "already in use"
		case lv.name == "ansible_index_var" || lv.name == "ansible_loop" || lv.name == "ansible_loop_var":
			conflict = "reserved"
		case connectionVars[lv.name]:
			conflict = "reserved"
		case task.LoopVar == task.IndexVar:
			conflict = "used more than once"
		default:
			continue
		}
		msg := fmt.Sprintf("The variable %s is %s.", template.PyRepr(lv.name), conflict)
		help := fmt.Sprintf("You should set the `%s` value in the `loop_control` option for the task "+
			"to something else to avoid variable collisions and unexpected behavior.", lv.key)
		var block string
		if pos, ok := task.KeywordPos["loop_control."+lv.key]; ok && pos.Line > 0 {
			block = fmt.Sprintf("[WARNING]: %s\nOrigin: %s:%d:%d\n\n%s\n%s\n\n", msg, pos.File, pos.Line, pos.Col,
				template.SourceExcerpt(pos.File, pos.Line, pos.Col), help)
		} else {
			// The default loop_var has no origin: its value stands in.
			block = fmt.Sprintf("[WARNING]: %s\nOrigin: <unknown>\n\n%s\n\n%s\n\n", msg, lv.name, help)
		}
		r.warnBlock(block)
	}
}

// breakWhen evaluates loop_control.break_when after an item ran (with the
// item's result registered), recording break_when_result on it; true
// ends the loop. A condition that fails to evaluate fails the item and
// ends the loop too.
func (r *Runner) breakWhen(task *playbook.Task, itemCtx *vars.Context, res *agentproto.Result) bool {
	if len(task.BreakWhen) == 0 {
		return false
	}
	ctx := itemCtx
	if task.Register != "" {
		ctx = ctx.WithOverlay(registerOverlay(task, res))
	}
	for _, cond := range task.BreakWhen {
		pos := task.BreakWhenPos[cond]
		ok, err := ctx.At(template.Position{File: pos.File, Line: pos.Line, Col: pos.Col}).EvalWhen([]string{cond})
		if err != nil {
			msg := conditionalMessage(err)
			setExtra(res, "break_when_result", msg)
			if res.Failed {
				setExtra(res, "break_when_suppressed_exception", "(traceback unavailable)")
			}
			res.Failed = true
			res.ErrorText = "A 'break_when' expression failed: " + msg
			res.ErrorFile, res.ErrorLine, res.ErrorCol = pos.File, pos.Line, pos.Col
			setExtra(res, "exception", "(traceback unavailable)")
			return true
		}
		if !ok {
			setExtra(res, "break_when_result", false)
			return false
		}
	}
	setExtra(res, "break_when_result", true)
	return true
}
