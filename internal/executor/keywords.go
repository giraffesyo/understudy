package executor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// resolveKeywords templates a task's templated keywords (no_log:
// "{{ x }}", retries: "{{ n }}", ...) for one host, returning a copy of the
// task with the resolved values. Ansible post-validates these per host.
func resolveKeywords(task *playbook.Task, vctx *vars.Context) (*playbook.Task, error) {
	if len(task.KeywordTemplates) == 0 {
		return task, nil
	}
	t := *task
	t.Orig = task.Identity()
	for key, src := range task.KeywordTemplates {
		v, err := vctx.TemplateString(src)
		if err != nil {
			return task, fmt.Errorf("error templating %s: %v", key, err)
		}
		switch key {
		case "retries", "delay":
			n, err := keywordInt(v)
			if err != nil {
				return task, fmt.Errorf("the field '%s' has an invalid value (%s), and could not be converted to int", key, template.PyStr(v))
			}
			if key == "retries" {
				t.Retries = n
				t.RetriesSet = true
			} else {
				t.Delay = n
			}
		default:
			b, ok := playbook.ParseBool(v)
			if !ok {
				return task, fmt.Errorf("the field '%s' has an invalid value (%s), and could not be converted to bool", key, template.PyStr(v))
			}
			switch key {
			case "no_log":
				t.NoLog = b
			case "ignore_errors":
				t.IgnoreErrors = b
			case "become":
				t.Become.Become = &b
			case "check_mode":
				t.CheckMode = &b
			case "diff":
				t.Diff = &b
			}
		}
	}
	return &t, nil
}

func keywordInt(v any) (int, error) {
	switch t := v.(type) {
	case int64:
		return int(t), nil
	case int:
		return t, nil
	case float64:
		return int(t), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err
	}
	return 0, fmt.Errorf("not an int")
}

// shown is the result as the callback may display it: censored when the
// task has no_log (or the action asked for it, as include_vars does for
// vault-encrypted files). Registered values keep the full result.
func shown(task *playbook.Task, res *agentproto.Result) *agentproto.Result {
	if res == nil {
		return res
	}
	if !task.NoLog && !(res.Extra != nil && res.Extra["_ansible_no_log"] == true) {
		return res
	}
	c := *res
	c.Censored = true
	c.VerboseAlways = false
	return &c
}

// argTemplateError is the failed result for a module argument that did
// not template (TaskArgsFinalizer): "Finalization of task args for
// '<action>' failed", caused by the argument's error at its origin.
func argTemplateError(task *playbook.Task, key string, pos template.Position, err error) *agentproto.Result {
	cause, ok := template.Cause(err)
	if !ok {
		return agentproto.Fail("error templating argument %q: %v", key, err)
	}
	mid := fmt.Sprintf("Finalization of task args for '%s' failed.", resolvedAction(task))
	inner := fmt.Sprintf("Error while resolving value for '%s': %s", key, cause)
	res := agentproto.Fail("Task failed: %s: %s", strings.TrimSuffix(mid, "."), inner)
	res.Origin = "verbatim"
	chain := &agentproto.ErrorChain{Outer: "Task failed.", Inner: inner,
		InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col}
	if file, line, col, ok := template.FileErrorOrigin(err); ok {
		// Raised rendering a template file (the template lookup): the
		// cause keeps the file's origin.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s'.", key)
		chain.Root = &agentproto.ErrorChain{Inner: cause, InnerFile: file, InnerLine: line, InnerCol: col, InnerPathOnly: line == 0}
	} else if re := (*template.RecursionError)(nil); errors.As(err, &re) && re.Pos.File != "" {
		// A lazy container's item or a variable that recursed: raised
		// where its template is.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s'.", key)
		chain.Root = &agentproto.ErrorChain{Inner: cause, InnerFile: re.Pos.File, InnerLine: re.Pos.Line, InnerCol: re.Pos.Col}
	} else if at, ok := template.UndefinedElsewhere(err, pos); ok {
		// An undefined value a variable's own template used: raised
		// where that template is.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s'.", key)
		chain.Root = &agentproto.ErrorChain{Inner: cause, InnerFile: at.File, InnerLine: at.Line, InnerCol: at.Col}
	} else if at, ok := syntaxElsewhere(err, pos); ok {
		// A variable's own template that does not parse: raised where
		// that template is.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s'.", key)
		chain.Root = &agentproto.ErrorChain{Inner: cause, InnerFile: at.File, InnerLine: at.Line, InnerCol: at.Col}
	} else if msg, at, value, ok := template.RenderingCause(err); ok {
		// A value variable storage does not support (a timedelta, a
		// method), shown apart as its own value, or one that cannot be
		// decrypted, at its origin.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s': Error rendering template.", key)
		chain.Root = &agentproto.ErrorChain{Inner: msg, InnerValue: value,
			InnerFile: at.File, InnerLine: at.Line, InnerCol: at.Col}
	} else if head, detail, value, ok := template.SplitCause(err); ok {
		// A plugin's exception raised while handling another shows apart.
		chain.Inner = fmt.Sprintf("Error while resolving value for '%s': %s", key, head)
		chain.Root = &agentproto.ErrorChain{Inner: detail, InnerValue: value}
		if at, ok := template.PluginValueOrigin(err); ok {
			// The value is shown where it was written.
			chain.Root.InnerValue, chain.Root.InnerFile, chain.Root.InnerLine, chain.Root.InnerCol = "", at.File, at.Line, at.Col
		}
	}
	switch a := task.ActionPos; {
	case a.Line == 0 || (a.Line == task.Src.Line && a.Col == task.Src.Col):
		// The action is where the task starts: one error, one origin.
		chain.Outer = "Task failed: " + mid
	case a.File == pos.File && a.Line == pos.Line && a.Col == pos.Col && chain.Root == nil:
		// The argument is in the action's own value: one event there.
		chain.Inner = strings.TrimSuffix(mid, ".") + ": " + chain.Inner
	default:
		chain.Mid, chain.MidFile, chain.MidLine, chain.MidCol = mid, a.File, a.Line, a.Col
	}
	res.ErrorChain = chain
	return res
}

// resolvedAction is the task's action as ansible-core resolves it: a
// builtin module's fully qualified name.
func resolvedAction(task *playbook.Task) string {
	a := task.DisplayAction()
	if strings.Count(a, ".") >= 2 && !strings.HasPrefix(a, "ansible.builtin.") && !strings.HasPrefix(a, "ansible.legacy.") {
		return a
	}
	return "ansible.builtin." + task.Module
}

// syntaxElsewhere is where a syntax error err raised from a template
// other than the argument's at pos is (a variable's value).
func syntaxElsewhere(err error, pos template.Position) (template.Position, bool) {
	var te *template.TemplateError
	if !errors.As(err, &te) || !te.Syntax || te.Pos.File == "" {
		return template.Position{}, false
	}
	if te.Pos.File == pos.File && te.Pos.Line == pos.Line && te.Pos.Col == pos.Col {
		return template.Position{}, false
	}
	return te.Pos, true
}
