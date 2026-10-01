package callback

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// noJSONModules print their raw output ("rc=N >>") under the minimal
// callback instead of a JSON dump (Ansible's C.MODULE_NO_JSON).
var noJSONModules = map[string]bool{
	"command": true, "shell": true, "raw": true,
	"ansible.builtin.command": true, "ansible.builtin.shell": true, "ansible.builtin.raw": true,
	"ansible.legacy.command": true, "ansible.legacy.shell": true, "ansible.legacy.raw": true,
}

// Minimal is ansible-core's "minimal" stdout callback, which the ad-hoc
// `ansible` command uses: one "host | STATUS" block per result and no
// banners or recap.
type Minimal struct {
	Default // colors, error de-duplication, JSON rendering

	// ArgOrder is the ad-hoc -a argument key order, for the task repr in
	// error origins (Python dicts keep insertion order).
	ArgOrder []string
	// TaskTimeout, Async and Poll are the ad-hoc --task-timeout, -B and
	// -P the task repr shows (async_val and poll only when either is set).
	TaskTimeout, Async, Poll int
	// OneLine is the oneline callback (ad-hoc -o): each result on one
	// line, and no error blocks.
	OneLine bool
}

// NewMinimal builds the ad-hoc callback.
func NewMinimal(verbosity int) *Minimal {
	d := New(verbosity)
	return &Minimal{Default: Default{Out: d.Out, Verbosity: verbosity, NoColor: d.NoColor, Columns: d.Columns}, Poll: 15}
}

var _ executor.Callback = (*Minimal)(nil)

func (m *Minimal) PlayStart(*playbook.Play)                                    {}
func (m *Minimal) NoHostsRemaining()                                           {}
func (m *Minimal) TaskStart(*playbook.Task, string, bool)                      {}
func (m *Minimal) Included(*playbook.Task, string, []string, any, bool)        {}
func (m *Minimal) LoopResult(string, *playbook.Task, *agentproto.Result, bool) {}
func (m *Minimal) Recap(map[string]*executor.HostStats, []string)              {}
func (m *Minimal) PlaybookStart(string)                                        {}
func (m *Minimal) HandlerNotified(*playbook.Task, string)                      {}
func (m *Minimal) NoHostsMatched()                                             {}

func (m *Minimal) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OneLine {
		m.oneLine(host, task, res)
		return
	}
	if res.Failed {
		m.adhocError(task, res)
	}
	_, async := res.Extra["ansible_job_id"]
	switch {
	case res.Skipped:
		m.display(cCyan, host+" | SKIPPED")
	case noJSONModules[task.Module] && (res.RC != nil || res.Failed) && (res.Failed || !async):
		caption, c := "SUCCESS", cGreen
		switch {
		case res.Failed:
			caption, c = "FAILED", cRed
		case res.Changed:
			caption, c = "CHANGED", cYellow
		}
		rc := -1
		if res.RC != nil {
			rc = *res.RC
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s | %s | rc=%d >>\n", host, caption, rc)
		b.WriteString(res.Stdout)
		b.WriteString(res.Stderr)
		b.WriteString(res.Msg)
		m.display(c, b.String())
	default:
		state, c := "SUCCESS", cGreen
		switch {
		case res.Failed:
			state, c = "FAILED!", cRed
		case res.Changed:
			state, c = "CHANGED", cYellow
		}
		saved := res.VerboseAlways
		res.VerboseAlways = true // minimal always indents
		m.display(c, fmt.Sprintf("%s | %s => %s", host, state, m.dump(task, res)))
		res.VerboseAlways = saved
	}
}

func (m *Minimal) HostUnreachable(host string, task *playbook.Task, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.OneLine {
		m.display(cBrightRed, fmt.Sprintf("%s | UNREACHABLE!: %s", host, msg))
		return
	}
	res := map[string]any{"changed": false, "msg": msg, "unreachable": true}
	m.display(cBrightRed, fmt.Sprintf("%s | UNREACHABLE! => %s", host, template.PyJSON(res, 4, true, false)))
}

// adhocError prints the "[ERROR]: Task failed" block; ad-hoc tasks have no
// source file, so the origin is the synthesized task's repr.
func (m *Minimal) adhocError(task *playbook.Task, res *agentproto.Result) {
	kind := "Module failed"
	if res.Origin == "action" {
		kind = "Action failed"
	}
	args := yaml.NewOMap()
	if task.FreeForm != "" {
		args.Set("_raw_params", task.FreeForm)
	}
	for _, k := range m.ArgOrder {
		if v, ok := task.Args[k]; ok {
			args.Set(k, v)
		}
	}
	for k, v := range task.Args {
		if !args.Has(k) {
			args.Set(k, v)
		}
	}
	repr := yaml.NewOMap()
	repr.Set("action", task.Module)
	repr.Set("args", args)
	repr.Set("timeout", int64(m.TaskTimeout))
	if m.Async != 0 || m.Poll != 0 {
		repr.Set("async_val", int64(m.Async))
		repr.Set("poll", int64(m.Poll))
	}
	head := fmt.Sprintf("Task failed: %s: %s", kind, res.ErrorMessage())
	switch res.Origin {
	case "verbatim":
		head = res.Msg
	case "raised":
		head = "Task failed: " + res.ErrorMessage()
	}
	block := fmt.Sprintf("[ERROR]: %s\nOrigin: <adhoc '%s' task>\n\n%s\n",
		head, task.Module, template.PyStr(repr))
	if m.errors == nil {
		m.errors = map[string]bool{}
	}
	if m.errors[block] {
		return
	}
	m.errors[block] = true
	fmt.Fprint(m.Out, m.paint(cRed, strings.TrimRight(block, "\n")))
	fmt.Fprint(m.Out, "\n\n")
}

// The minimal callback does not report retries or async progress.
func (m *Minimal) Retrying(string, *playbook.Task, string, int, *agentproto.Result) {}
func (m *Minimal) AsyncPoll(string, string)                                         {}
func (m *Minimal) AsyncDone(string, string, bool)                                   {}

// oneLine is the oneline callback's result line: a command's output as
// "rc=N | (stdout) ..." (escaped newlines), else the result's JSON with
// its newlines removed; a failure first says it raised.
func (m *Minimal) oneLine(host string, task *playbook.Task, res *agentproto.Result) {
	_, async := res.Extra["ansible_job_id"]
	generic := func(caption string) string {
		esc := strings.NewReplacer("\n", "\\n", "\r", "\\r")
		rc := "-1"
		if res.RC != nil {
			rc = fmt.Sprint(*res.RC)
		}
		line := fmt.Sprintf("%s | %s | rc=%s | (stdout) %s", host, caption, rc, esc.Replace(res.Stdout))
		if res.Stderr != "" {
			line += " (stderr) " + esc.Replace(res.Stderr)
		}
		return line
	}
	dump := func() string {
		// _dump_results(result, indent=0), uncleaned: indent 0 unless the
		// result is verbose_always (or -vvv), then its newlines removed.
		saved := res.VerboseAlways
		defer func() { res.VerboseAlways = saved }()
		zero := m.indent(res.VerboseAlways) == 0
		res.VerboseAlways = true
		out := m.dumpRaw(task, res, false)
		if res.Censored {
			out = m.dump(task, res)
		}
		if zero {
			// json.dumps(indent=0): newlines without indentation.
			return regexp.MustCompile("\n *").ReplaceAllString(out, "")
		}
		return strings.ReplaceAll(out, "\n", "")
	}
	switch {
	case res.Skipped:
		m.display(cCyan, host+" | SKIPPED")
	case res.Failed:
		if _, raised := res.Extra["exception"]; raised {
			if noJSONModules[task.Module] {
				m.display(cRed, generic("FAILED"))
			} else {
				exc := strings.Split(strings.TrimSpace(fmt.Sprint(res.Extra["exception"])), "\n")
				m.display(cRed, "An exception occurred during task execution. To see the full traceback, use -vvv. The error was: "+exc[len(exc)-1])
			}
		}
		m.display(cRed, fmt.Sprintf("%s | FAILED! => %s", host, dump()))
	default:
		state, c := "SUCCESS", cGreen
		if res.Changed {
			state, c = "CHANGED", cYellow
		}
		if noJSONModules[task.Module] && !async {
			m.display(c, generic(state))
		} else {
			m.display(c, fmt.Sprintf("%s | %s => %s", host, state, dump()))
		}
	}
}
