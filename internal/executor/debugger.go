package executor

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// This file is the task debugger: the debug strategy (linear plus the
// debugger on every failed or unreachable result), the debugger keyword
// and ANSIBLE_ENABLE_TASK_DEBUGGER, modeled on ansible-core's Debugger
// (a cmd.Cmd). Commands: p/pprint <expr>, c/continue, r/redo, q/quit (and
// end of input), u/update_task, h/help. Expressions are evaluated with the
// Jinja expression engine against task, task_vars, host, result and
// play_context; task.args[...] / task_vars[...] assignments and del are
// supported for fixing a task before redo (task_vars ones through
// update_task, as in ansible-core).

type debugAction int

const (
	debugContinue debugAction = iota
	debugRedo
	debugQuit
)

// needsDebugger is _needs_debugger for one result.
func (r *Runner) needsDebugger(play *playbook.Play, task *playbook.Task, res *agentproto.Result) bool {
	unreachable := res.Extra != nil && res.Extra["unreachable"] == true
	ignoreErrors := envBool("ANSIBLE_TASK_DEBUGGER_IGNORE_ERRORS", true) && task.IgnoreErrors
	global := (play != nil && play.Strategy == "debug") || envBool("ANSIBLE_ENABLE_TASK_DEBUGGER", false)
	ret := global && ((res.Failed && !ignoreErrors) || unreachable)
	mode := task.Debugger
	if mode == "" && play != nil {
		mode = play.Debugger
	}
	switch {
	case mode == "always":
		ret = true
	case mode == "never":
		ret = false
	case mode == "on_failed" && res.Failed && !ignoreErrors:
		ret = true
	case mode == "on_unreachable" && unreachable:
		ret = true
	case mode == "on_skipped" && res.Skipped:
		ret = true
	}
	return ret
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	}
	return false
}

// debugSession is one debugger prompt for a host's result.
type debugSession struct {
	r        *Runner
	task     *playbook.Task // a per-host copy; args edits apply to redo
	orig     *playbook.Task // the task as loaded (task._ds)
	host     string
	vctx     *vars.Context
	res      *agentproto.Result
	play     *playbook.Play
	override map[string]any // task_vars assignments
	// updated is task_vars as update_task templated the task with
	// (nil: not updated): a redo runs the task as loaded, with them.
	updated map[string]any
	lastcmd string
	out     io.Writer
}

func (r *Runner) debugIn() *bufio.Reader {
	if r.dbgReader == nil {
		in := r.DebugIn
		if in == nil {
			in = os.Stdin
		}
		r.dbgReader = bufio.NewReader(in)
	}
	return r.dbgReader
}

// runDebugger prompts until the user continues, redoes or quits.
func (r *Runner) runDebugger(s *debugSession) debugAction {
	r.dbgMu.Lock()
	defer r.dbgMu.Unlock()
	s.out = r.DebugOut
	if s.out == nil {
		s.out = os.Stdout
	}
	prompt := fmt.Sprintf("[%s] %s (debug)> ", s.host, taskRepr(s.task))
	in := r.debugIn()
	for {
		fmt.Fprint(s.out, prompt)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return s.quit() // input() at EOF: do_EOF
		}
		line = strings.TrimRight(line, "\r\n")
		if act, done := s.onecmd(line); done {
			return act
		}
	}
}

func (s *debugSession) quit() debugAction {
	fmt.Fprintln(s.out, "User interrupted execution")
	return debugQuit
}

var identChars = regexp.MustCompile(`^[A-Za-z0-9_]*`)

// onecmd is cmd.Cmd.onecmd: empty lines repeat the last command, "?" is
// help, a leading identifier selects the command, anything else is a
// statement.
func (s *debugSession) onecmd(line string) (debugAction, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		if s.lastcmd == "" {
			return 0, false
		}
		line = s.lastcmd
	}
	s.lastcmd = line
	if line == "EOF" {
		s.lastcmd = ""
	}
	if strings.HasPrefix(line, "?") {
		line = "help " + line[1:]
	}
	cmd := identChars.FindString(line)
	arg := strings.TrimSpace(line[len(cmd):])
	switch cmd {
	case "EOF", "q", "quit":
		return s.quit(), true
	case "c", "continue":
		return debugContinue, true
	case "r", "redo":
		return debugRedo, true
	case "u", "update_task":
		// The task is loaded again (task.args edits are lost) and
		// templated with task_vars, assignments included; a redo runs
		// it so. task_vars assignments take effect only through it.
		copied := *s.orig
		copied.Args = make(map[string]any, len(s.orig.Args))
		vctx := s.vctx.WithOverlay(maps.Clone(s.override))
		for k, v := range s.orig.Args {
			if tv, err := vctx.TemplateValue(v); err == nil {
				v = tv
			}
			copied.Args[k] = v
		}
		if copied.FreeForm != "" {
			if tv, err := vctx.TemplateString(copied.FreeForm); err == nil {
				copied.FreeForm = template.PyStr(tv)
			}
		}
		*s.task = copied
		s.updated = maps.Clone(s.override)
		return 0, false
	case "h", "help":
		s.help(arg)
		return 0, false
	case "p", "pprint":
		if v, ok := s.eval(arg); ok {
			fmt.Fprintln(s.out, pformat(v, 0, 0))
		}
		return 0, false
	}
	s.statement(line)
	return 0, false
}

var debugDocs = map[string]string{
	"EOF": "Quit", "q": "Quit", "quit": "Quit",
	"c": "Continue to next result", "continue": "Continue to next result",
	"r": "Schedule task for re-execution. The re-execution may not be the next result", "redo": "Schedule task for re-execution. The re-execution may not be the next result",
	"u": "Recreate the task from ``task._ds``, and template with updated ``task_vars``", "update_task": "Recreate the task from ``task._ds``, and template with updated ``task_vars``",
	"p": "Pretty Print", "pprint": "Pretty Print",
	"h": `List available commands with "help" or detailed help with "help cmd".`, "help": `List available commands with "help" or detailed help with "help cmd".`,
}

// help is cmd.Cmd.do_help.
func (s *debugSession) help(arg string) {
	if arg != "" {
		if doc, ok := debugDocs[arg]; ok {
			fmt.Fprintf(s.out, "%s\n", doc)
		} else {
			fmt.Fprintf(s.out, "*** No help on %s\n", arg)
		}
		return
	}
	header := "Documented commands (type help <topic>):"
	fmt.Fprintf(s.out, "\n%s\n%s\n%s\n\n", header, strings.Repeat("=", len(header)),
		"EOF  c  continue  h  help  p  pprint  q  quit  r  redo  u  update_task")
}

var resultAttrRe = regexp.MustCompile(`^\s*result\.(\w+)`)

var (
	assignRe = regexp.MustCompile(`^(task\.args|task_vars)\s*\[(.+)\]\s*=\s*(.+)$`)
	delRe    = regexp.MustCompile(`^del\s+(task\.args|task_vars)\s*\[(.+)\]$`)
)

// statement is Debugger.default: assignments into task.args / task_vars,
// del of their keys, or an expression whose repr is echoed (Python's
// interactive 'single' mode).
func (s *debugSession) statement(line string) {
	if m := assignRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(m[3]), "=") {
		key, ok := s.evalKey(m[2])
		if !ok {
			return
		}
		v, ok := s.eval(m[3])
		if !ok {
			return
		}
		s.target(m[1])[key] = v
		return
	}
	if m := delRe.FindStringSubmatch(line); m != nil {
		key, ok := s.evalKey(m[2])
		if !ok {
			return
		}
		t := s.target(m[1])
		if _, has := t[key]; !has {
			fmt.Fprintf(s.out, "***KeyError:KeyError(%s)\n", pyRepr(key))
			return
		}
		delete(t, key)
		return
	}
	if v, ok := s.eval(line); ok && v != nil {
		fmt.Fprintln(s.out, pyRepr(v))
	}
}

func (s *debugSession) target(name string) map[string]any {
	if name == "task_vars" {
		return s.override
	}
	if s.task.Args == nil {
		s.task.Args = map[string]any{}
	}
	return s.task.Args
}

func (s *debugSession) evalKey(expr string) (string, bool) {
	v, ok := s.eval(expr)
	if !ok {
		return "", false
	}
	k, isStr := v.(string)
	if !isStr {
		k = template.PyStr(v)
	}
	return k, true
}

// eval evaluates an expression in the debugger scope, reporting errors
// the way Debugger.evaluate does ("***Type:repr").
func (s *debugSession) eval(expr string) (v any, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(s.out, "***Exception:Exception(%s)\n", pyRepr(fmt.Sprint(p)))
			v, ok = nil, false
		}
	}()
	scope := &debugScope{s: s}
	out, err := s.r.Engine.EvalExpression(expr, scope, template.Position{})
	if err != nil {
		var ue *template.UndefinedError
		if m := resultAttrRe.FindStringSubmatch(expr); m != nil && m[1] != "host" && m[1] != "task" && m[1] != "utr" {
			// ansible-core 2.19+ passes a HostTaskResult (host, task, utr).
			fmt.Fprintf(s.out, "***AttributeError:AttributeError(\"'HostTaskResult' object has no attribute '%s'\")\n", m[1])
		} else if asUndefined(err, &ue) && !scope.known(ue.Name) && isIdent(ue.Name) {
			fmt.Fprintf(s.out, "***NameError:NameError(\"name '%s' is not defined\")\n", ue.Name)
		} else {
			fmt.Fprintf(s.out, "***Exception:Exception(%s)\n", pyRepr(err.Error()))
		}
		return nil, false
	}
	return out, true
}

func asUndefined(err error, target **template.UndefinedError) bool {
	ue, ok := err.(*template.UndefinedError)
	if ok {
		*target = ue
	}
	return ok
}

func isIdent(s string) bool {
	return s != "" && identChars.FindString(s) == s && (s[0] < '0' || s[0] > '9')
}

// debugScope is the debugger's namespace.
type debugScope struct{ s *debugSession }

func (d *debugScope) known(name string) bool {
	_, ok := d.Get(name)
	return ok
}

func (d *debugScope) Get(name string) (any, bool) {
	s := d.s
	switch name {
	case "task":
		return &debugTask{t: s.task}, true
	case "task_vars":
		return &overlayMapping{base: s.vctx.AsMapping(), over: s.override}, true
	case "host":
		return debugHost(s.host), true
	case "result":
		return &debugResult{host: s.host, task: s.task, res: s.res}, true
	case "play_context":
		return &debugPlayContext{r: s.r, play: s.play, task: s.task}, true
	case "True":
		return true, true
	case "False":
		return false, true
	case "None":
		return nil, true
	}
	return nil, false
}

// taskRepr is Task.__repr__.
func taskRepr(t *playbook.Task) string {
	if t.Module == "meta" {
		if m, ok := t.Args["_raw_params"].(string); ok {
			return "TASK: meta (" + m + ")"
		}
		if t.FreeForm != "" {
			return "TASK: meta (" + t.FreeForm + ")"
		}
	}
	name := t.Name
	if name == "" {
		name = t.Module
	}
	if t.RoleName != "" {
		name = t.RoleName + " : " + name
	}
	return "TASK: " + name
}

type debugHost string

// debugTask exposes a task's attributes (task.args, task.name, ...).
type debugTask struct{ t *playbook.Task }

func (d *debugTask) fields() map[string]any {
	t := d.t
	args := map[string]any{}
	maps.Copy(args, t.Args)
	if t.FreeForm != "" {
		args["_raw_params"] = t.FreeForm
	}
	whens := make([]any, len(t.When))
	for i, w := range t.When {
		whens[i] = w
	}
	tags := make([]any, len(t.Tags))
	for i, tg := range t.Tags {
		tags[i] = tg
	}
	return map[string]any{
		"args": &liveArgs{d: d, m: args}, "name": t.Name, "action": t.Module, "when": whens,
		"register": t.Register, "ignore_errors": t.IgnoreErrors, "loop": t.Loop,
		"vars": t.Vars, "tags": tags, "delegate_to": t.Delegate, "until": t.Until,
		"retries": int64(t.Retries), "delay": int64(t.Delay), "notify": t.Notify,
		"no_log": t.NoLog, "run_once": t.RunOnce, "debugger": t.Debugger,
	}
}

func (d *debugTask) GetItem(k string) (any, bool) { v, ok := d.fields()[k]; return v, ok }
func (d *debugTask) Keys() []string               { return sortedKeys(d.fields()) }
func (d *debugTask) Len() int                     { return len(d.fields()) }

// liveArgs is task.args: reads see the current args.
type liveArgs struct {
	d *debugTask
	m map[string]any
}

func (a *liveArgs) GetItem(k string) (any, bool) { v, ok := a.m[k]; return v, ok }
func (a *liveArgs) Keys() []string               { return sortedKeys(a.m) }
func (a *liveArgs) Len() int                     { return len(a.m) }

// debugResult is result: result._result is the result dict.
type debugResult struct {
	host string
	task *playbook.Task
	res  *agentproto.Result
}

func (d *debugResult) fields() map[string]any {
	return map[string]any{"host": debugHost(d.host), "task": &debugTask{t: d.task}, "utr": d.res.ToVars()}
}
func (d *debugResult) GetItem(k string) (any, bool) { v, ok := d.fields()[k]; return v, ok }
func (d *debugResult) Keys() []string               { return sortedKeys(d.fields()) }
func (d *debugResult) Len() int                     { return 4 }

// debugPlayContext is play_context (the attributes worth inspecting).
type debugPlayContext struct {
	r    *Runner
	play *playbook.Play
	task *playbook.Task
}

func (d *debugPlayContext) fields() map[string]any {
	return map[string]any{
		"check_mode": d.r.effectiveCheckMode(d.play, d.task), "diff": d.r.effectiveDiff(d.play, d.task),
		"remote_user": firstNonEmpty(d.task.RemoteUser, d.play.RemoteUser),
		"connection":  firstNonEmpty(d.task.Connection, d.play.Connection),
		"verbosity":   int64(d.r.Opts.Verbosity),
	}
}
func (d *debugPlayContext) GetItem(k string) (any, bool) { v, ok := d.fields()[k]; return v, ok }
func (d *debugPlayContext) Keys() []string               { return sortedKeys(d.fields()) }
func (d *debugPlayContext) Len() int                     { return len(d.fields()) }

// overlayMapping is task_vars with the debugger's assignments on top.
type overlayMapping struct {
	base template.Mapping
	over map[string]any
}

func (o *overlayMapping) GetItem(k string) (any, bool) {
	if v, ok := o.over[k]; ok {
		return v, true
	}
	return o.base.GetItem(k)
}
func (o *overlayMapping) Keys() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range o.base.Keys() {
		seen[k] = true
		out = append(out, k)
	}
	for _, k := range sortedKeys(o.over) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}
func (o *overlayMapping) Len() int { return len(o.Keys()) }

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pyRepr is Python repr() for plain values (dicts in key order).
func pyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(t)
	case debugHost:
		return string(t)
	case *debugTask:
		return taskRepr(t.t)
	case *debugResult:
		return "HostTaskResult(host=" + t.host + ", task=" + taskRepr(t.task) + ")"
	case *debugPlayContext:
		return "<PlayContext>"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eIN") {
			s += ".0"
		}
		return s
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyStrRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return dictRepr(sortedKeys(t), func(k string) any { return t[k] })
	case template.Mapping:
		return dictRepr(t.Keys(), func(k string) any { v, _ := t.GetItem(k); return v })
	}
	p := template.Plain(v)
	switch p.(type) {
	case map[string]any, []any, string, bool, int64, float64, nil:
		return pyRepr(p)
	}
	return template.PyStr(v)
}

func dictRepr(keys []string, get func(string) any) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = pyStrRepr(k) + ": " + pyRepr(get(k))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// pyStrRepr is repr() of a str.
func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pformat is pprint.pformat (width 80, sorted dicts, one item per line
// once a container does not fit).
func pformat(v any, indent, allowance int) string {
	const width = 80
	v = normalizeForPrint(v)
	rep := pyRepr(v)
	if len([]rune(rep)) <= width-indent-allowance {
		return rep
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return rep
		}
		keys := sortedKeys(t)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			kr := pyStrRepr(k)
			b.WriteString(kr + ": ")
			last := i == len(keys)-1
			al := 1
			if last {
				al = allowance + 1
			}
			b.WriteString(pformat(t[k], indent+1+len([]rune(kr))+2, al))
			if !last {
				b.WriteString(",\n" + strings.Repeat(" ", indent+1))
			}
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		if len(t) == 0 {
			return rep
		}
		var b strings.Builder
		b.WriteByte('[')
		for i, e := range t {
			last := i == len(t)-1
			al := 1
			if last {
				al = allowance + 1
			}
			b.WriteString(pformat(e, indent+1, al))
			if !last {
				b.WriteString(",\n" + strings.Repeat(" ", indent+1))
			}
		}
		b.WriteByte(']')
		return b.String()
	}
	return rep
}

// normalizeForPrint turns mappings (except the debugger's own objects)
// into plain dicts.
func normalizeForPrint(v any) any {
	switch t := v.(type) {
	case *debugTask, *debugResult, *debugPlayContext, debugHost:
		return v
	case *liveArgs:
		return t.m
	case template.Mapping:
		m := map[string]any{}
		for _, k := range t.Keys() {
			x, _ := t.GetItem(k)
			m[k] = normalizeForPrint(x)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = normalizeForPrint(x)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeForPrint(x)
		}
		return out
	case string, bool, int64, int, float64, nil:
		return v
	}
	return template.Plain(v)
}
