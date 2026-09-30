package executor

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// maxTaskTimeout is AnsibleTimeoutError._MAX_TIMEOUT (BSD's alarm limit).
const maxTaskTimeout = 100_000_000

// deprecatedTimedoutFrame is the timed-out result's timedout.frame
// (TaskTimeoutError.as_task_result).
var deprecatedTimedoutFrame = template.Deprecated{
	Msg:     "The `timedout.frame` task result key is deprecated.",
	Help:    "Configure `DISPLAY_TRACEBACK` to see a traceback on timeout errors.",
	Version: "2.23"}

// taskTimeout resolves the timeout keyword for one run of a task (a loop
// item's run templates it with the item): the task's own, else its
// blocks' or role's (merged at parse time), else the play's, else
// TASK_TIMEOUT. The value converts as a FieldAttribute of isa 'int' does;
// a failed result reports a value that does not.
func (r *Runner) taskTimeout(play *playbook.Play, task *playbook.Task, vctx *vars.Context) (int, *agentproto.Result) {
	raw := task.Timeout
	pos, ok := task.KeywordPos["timeout"]
	if !ok {
		pos = task.Src
	}
	if raw == nil && play != nil {
		raw = play.Timeout
	}
	if raw == nil {
		return r.Opts.TaskTimeout, nil
	}
	v := raw
	if s, isStr := raw.(string); isStr && (strings.Contains(s, "{{") || strings.Contains(s, "{%")) {
		var err error
		if v, err = vctx.At(pos).TemplateValue(raw); err != nil {
			return 0, agentproto.Fail("Error processing keyword 'timeout': %v", err)
		}
		v = template.Undeprecate(v)
	}
	if v == nil {
		return r.Opts.TaskTimeout, nil
	}
	n, ok := keywordIntValue(v)
	if !ok {
		inner := fmt.Sprintf("The value %s could not be converted to 'int'.", template.PyRepr(v))
		res := agentproto.Fail("Task failed: Error processing keyword 'timeout': %s", inner)
		res.Origin = "verbatim"
		res.ErrorChain = &agentproto.ErrorChain{
			Outer: "Task failed.",
			Mid:   "Error processing keyword 'timeout'.", MidFile: pos.File, MidLine: pos.Line, MidCol: pos.Col,
			Inner: inner, InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col,
		}
		return 0, res
	}
	if n < 0 || n > maxTaskTimeout {
		res := agentproto.Fail("Task failed: Timeout %d is invalid, it must be between 0 and %d.", n, maxTaskTimeout)
		res.Origin = "verbatim"
		return 0, res
	}
	return int(n), nil
}

// keywordIntValue converts a keyword value as FieldAttribute isa 'int'
// does: an int (a bool is one) as is, anything else through
// decimal.Decimal, when that is a whole number.
func keywordIntValue(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		if t != float64(int64(t)) {
			return 0, false
		}
		return int64(t), true
	case yaml.UnsafeString:
		return keywordIntValue(string(t))
	case string:
		rat, ok := new(big.Rat).SetString(strings.ReplaceAll(strings.TrimSpace(t), "_", ""))
		if !ok || !rat.IsInt() || !rat.Num().IsInt64() {
			return 0, false
		}
		return rat.Num().Int64(), true
	}
	return 0, false
}

// dispatchTimed is dispatch under the task's timeout (seconds; 0 = none),
// as TaskExecutor runs the action handler inside alarm_timeout: when the
// time is up the action is abandoned and the run's result is the timeout
// error, which bypasses failed_when and until retries.
func (r *Runner) dispatchTimed(ctx context.Context, task *playbook.Task, actx *actions.Context, args map[string]any, freeForm string, timeout int) (*agentproto.Result, bool) {
	if timeout <= 0 {
		return r.dispatch(ctx, task, actx, args, freeForm), false
	}
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan *agentproto.Result, 1)
	go func() { done <- r.dispatch(tctx, task, actx, args, freeForm) }()
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	select {
	case res := <-done:
		return res, false
	case <-timer.C:
		return timedOut(timeout), true
	}
}

// timedOut is TaskTimeoutError's task result.
func timedOut(timeout int) *agentproto.Result {
	return &agentproto.Result{
		Failed: true,
		Msg:    fmt.Sprintf("Task failed: Timed out after %d second(s).", timeout),
		Origin: "verbatim",
		Extra: map[string]any{
			"exception": "(traceback unavailable)",
			"timedout": map[string]any{
				"frame":  deprecate(deprecatedTimedoutFrame, deprecatedTimedoutFrame.Help),
				"period": int64(timeout),
			},
		},
	}
}
