package executor

import (
	"fmt"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// A task's args may come whole from a template: the args keyword
// ("args: '{{ dict }}'", _variable_params) or the module's string being
// one ("pip: '{{ dict }}'", its _raw_params). TaskArgsFinalizer resolves
// each such layer to a dict, warning that templated args are unsafe, and
// merges them under the args written out (args keyword, then the module's
// raw params, then the explicit args, each overriding the one before).

// templatedArgs resolves a task's templated args layers. done is the
// failed result when they did not resolve.
func (r *Runner) templatedArgs(task *playbook.Task, vctx *vars.Context) (map[string]any, *agentproto.Result) {
	if task.RawArgs != "" && !playbook.IsAllTemplate(task.RawArgs) {
		inner := fmt.Sprintf("Action '%s' does not support raw params.", resolvedAction(task))
		res := agentproto.Fail("Task failed: %s", inner)
		res.Origin = "verbatim"
		if a := task.ActionPos; a.Line != 0 && (a.Line != task.Src.Line || a.Col != task.Src.Col) {
			res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: inner,
				InnerFile: a.File, InnerLine: a.Line, InnerCol: a.Col}
		}
		return nil, res
	}
	type layer struct {
		src string
		pos template.Position
	}
	var layers []layer
	if task.VarArgs != "" {
		layers = append(layers, layer{task.VarArgs, task.VarArgsPos})
	}
	if task.RawArgs != "" {
		layers = append(layers, layer{task.RawArgs, task.ArgsPos})
	}
	if len(layers) == 0 {
		return nil, nil
	}
	merged := map[string]any{}
	for _, l := range layers {
		r.warnBlock(fmt.Sprintf("[WARNING]: Using a template for task args is unsafe in some situations "+
			"(see https://docs.ansible.com/ansible/devel/reference_appendices/faq.html#argsplat-unsafe).\n"+
			"Origin: %s:%d:%d\n\n%s\n", l.pos.File, l.pos.Line, l.pos.Col, template.SourceExcerpt(l.pos.File, l.pos.Line, l.pos.Col)))
		v, err := vctx.At(l.pos).Sourced().TemplateValue(l.src)
		if err != nil {
			cause, ok := template.Cause(err)
			if !ok {
				return nil, agentproto.Fail("error templating task args: %v", err)
			}
			return nil, r.finalizeArgsError(task, cause, l.pos)
		}
		v = template.Undeprecate(v)
		if _, isOmit := v.(template.Omit); isOmit {
			v = map[string]any{} // value_for_omit={}
		}
		m, ok := template.AsDict(v)
		if !ok {
			return nil, r.finalizeArgsError(task, fmt.Sprintf("Task args must resolve to a 'dict' not '%s'.", template.NativeTypeName(v)), l.pos)
		}
		for k, val := range m {
			if _, isOmit := val.(template.Omit); isOmit {
				delete(merged, k)
				continue
			}
			merged[k] = val
		}
	}
	return merged, nil
}

// finalizeArgsError is "Finalization of task args for '<action>' failed"
// caused by inner at pos.
func (r *Runner) finalizeArgsError(task *playbook.Task, inner string, pos template.Position) *agentproto.Result {
	mid := fmt.Sprintf("Finalization of task args for '%s' failed.", resolvedAction(task))
	res := agentproto.Fail("Task failed: %s: %s", strings.TrimSuffix(mid, "."), inner)
	res.Origin = "verbatim"
	chain := &agentproto.ErrorChain{Outer: "Task failed.", Inner: inner,
		InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col}
	a := task.ActionPos
	switch {
	case a.Line == 0 || (a.Line == task.Src.Line && a.Col == task.Src.Col):
		// The action is where the task starts: one error, one origin.
		chain.Outer = "Task failed: " + mid
	case a.File == pos.File && a.Line == pos.Line && a.Col == pos.Col:
		// The layer is the action's own value: one event there.
		chain.Inner = strings.TrimSuffix(mid, ".") + ": " + inner
	default:
		chain.Mid, chain.MidFile, chain.MidLine, chain.MidCol = mid, a.File, a.Line, a.Col
	}
	res.ErrorChain = chain
	return res
}
