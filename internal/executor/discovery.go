package executor

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// Python interpreter discovery, as ansible-core runs it before a Python
// module the first time a host needs one: with interpreter_python left at
// auto (the default), the first of INTERPRETER_PYTHON_FALLBACK that
// `command -v` finds on the target is the interpreter, reported back as
// the ansible_facts.discovered_interpreter_python fact (in the result of
// every module the action runs) so later tasks reuse it. understudy runs
// no Python, but discovers the same interpreter (modules whose output
// depends on Python read it) and reports the same fact and warnings.

// discoveredKey is the fact discovery sets.
const discoveredKey = "discovered_interpreter_python"

// interpreterFallback is INTERPRETER_PYTHON_FALLBACK's default.
var interpreterFallback = []string{"python3.14", "python3.13", "python3.12", "python3.11",
	"python3.10", "python3.9", "/usr/bin/python3", "python3"}

// noPythonModules are the modules ansible-core runs without a Python
// interpreter (action plugins over raw commands), and async_status
// polls, whose results never carry the discovery fact.
var noPythonModules = map[string]bool{"raw": true, "script": true, "async_status": true}

// discovery is one action's interpreter discovery state: an action
// discovers at most once, and every module it runs after that reports the
// fact (when the fact is the task host's to keep).
type discovery struct {
	done   bool
	path   string // the interpreter discovered ("" = configured or known)
	report bool   // the results carry the fact
	// warnings are discovery's, which _execute_module adds to the
	// result's (so a registered result holds them too).
	warnings []any
	help     map[string]string // the warnings' help text (see Result.WarningHelp)
}

// discoveryMode is the interpreter_python setting for a task: the
// ansible_python_interpreter variable, else ANSIBLE_PYTHON_INTERPRETER,
// else auto. ok is false for an explicit interpreter (no discovery).
func discoveryMode(vctx *vars.Context) (silent, ok bool) {
	var interp string
	if v, found := vctx.Get("ansible_python_interpreter"); found {
		if tv, err := vctx.TemplateValue(v); err == nil {
			v = tv
		}
		s, isStr := v.(string)
		if !isStr {
			return false, false
		}
		interp = s
	} else {
		interp = os.Getenv("ANSIBLE_PYTHON_INTERPRETER")
	}
	if interp == "" {
		interp = "auto"
	}
	if !strings.HasPrefix(interp, "auto") {
		return false, false
	}
	return strings.HasSuffix(interp, "_silent"), true
}

// knownInterpreter is the discovered_interpreter_python fact host already
// has, "" for none.
func (r *Runner) knownInterpreter(host string) string {
	vctx := r.Store.NewContext(host, template.Position{})
	facts, ok := vctx.Get("ansible_facts")
	if !ok {
		return ""
	}
	if m, ok := asStringMap(facts); ok {
		if s, ok := m[discoveredKey].(string); ok {
			return s
		}
	}
	return ""
}

// discoverInterpreter runs discovery for a module about to run on target
// (for host), once per action: the interpreter to use ("" to leave it to
// the configuration) and whether the action's results report the fact.
func (r *Runner) discoverInterpreter(ctx context.Context, st *discovery, host, target string, task *playbook.Task,
	vctx *vars.Context, conn connection.Connection) {
	if st.done {
		return
	}
	st.done = true
	silent, ok := discoveryMode(vctx)
	if !ok {
		return
	}
	held, _ := ctx.Value(discoveredCtxKey{}).(*string)
	if known := r.knownInterpreter(target); known != "" {
		st.path = known
		return
	}
	if held != nil && *held != "" {
		st.path = *held // an earlier loop item discovered it
		return
	}
	fallback := interpreterFallback
	if l := varList(vctx, "ansible_interpreter_python_fallback"); l != nil {
		fallback = l
	}
	r.displayVerbose(3, fmt.Sprintf("<%s> Attempting python interpreter discovery.", host))
	cmds := make([]string, len(fallback))
	for i, py := range fallback {
		cmds[i] = "command -v " + connection.ShellQuote(py)
	}
	found := "/usr/bin/python3"
	var interpreters []string
	if conn == nil {
		conn = connection.NewLocal()
	}
	res, err := conn.Exec(ctx, "echo FOUND; "+strings.Join(cmds, "; ")+"; echo ENDFOUND", connection.ExecOptions{})
	out := string(res.Stdout)
	if i, j := strings.Index(out, "FOUND"), strings.LastIndex(out, "ENDFOUND"); err == nil && i >= 0 && j > i {
		for _, l := range strings.Split(out[i+len("FOUND"):j], "\n") {
			if strings.HasPrefix(l, "/") {
				interpreters = append(interpreters, strings.TrimSpace(l))
			}
		}
	}
	switch {
	case len(interpreters) == 0:
		if !silent {
			tried := make([]any, len(fallback))
			for i, f := range fallback {
				tried[i] = f
			}
			st.warnings = append(st.warnings, fmt.Sprintf("No python interpreters found for host %s (tried %s).", template.PyRepr(host), template.PyRepr(tried)))
		}
	default:
		found = interpreters[0]
		if !silent {
			msg := fmt.Sprintf("Host %s is using the discovered Python interpreter at %s, but future installation of another "+
				"Python interpreter could cause a different interpreter to be discovered.", template.PyRepr(host), template.PyRepr(found))
			st.warnings = append(st.warnings, msg)
			if st.help == nil {
				st.help = map[string]string{}
			}
			st.help[msg] = "See https://docs.ansible.com/ansible-core/2.21/reference_appendices/interpreter_discovery.html for more information."
		}
	}
	st.path = found
	// A delegated task keeps the fact only with delegate_facts.
	st.report = task.Delegate == "" || task.DelegateFacts
	if held != nil && st.report {
		*held = found
	}
}

// discoveredCtxKey holds, for one task's run on a host, the interpreter
// an earlier loop item discovered.
type discoveredCtxKey struct{}

// asStringMap reads a fact dict, whichever map type holds it.
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case interface{ AsMap() map[string]any }:
		return m.AsMap(), true
	}
	return nil, false
}
