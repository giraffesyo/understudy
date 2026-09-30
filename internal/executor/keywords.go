package executor

import (
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
