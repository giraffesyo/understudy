// Package callback renders execution events byte-for-byte like
// ansible-core's default stdout callback (reference: ansible-core 2.21):
// PLAY/TASK banners, per-host result lines with Python-json result dumps,
// "[ERROR]: Task failed" origin blocks, and the PLAY RECAP table.
package callback

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
)

type color string

// Ansible's default color names mapped to the escape codes its stringc uses.
const (
	cGreen      color = "0;32" // COLOR_OK
	cYellow     color = "0;33" // COLOR_CHANGED
	cRed        color = "0;31" // COLOR_ERROR
	cBrightRed  color = "1;31" // COLOR_UNREACHABLE
	cCyan       color = "0;36" // COLOR_SKIP
	cBrightPurp color = "1;35" // COLOR_WARN (recap "ignored")
)

// Keys debug keeps when it has a msg (Ansible's _DEBUG_ALLOWED_KEYS), and
// the keys it hides for var= output (_hide_in_debug).
var (
	debugAllowedKeys = map[string]bool{"msg": true, "exception": true, "warnings": true, "deprecations": true}
	debugHiddenKeys  = []string{"changed", "failed", "skipped", "invocation", "skip_reason", "ansible_loop_var", "ansible_index_var"}
)

// Default is the standard output callback.
type Default struct {
	Out       io.Writer
	Err       io.Writer // warnings; nil = discarded
	Verbosity int
	NoColor   bool
	Columns   int // banner width (Display.columns); 0 = 79
	mu        sync.Mutex
	errors    map[string]bool // Display de-duplicates repeated errors
	warns     map[string]bool // ... and repeated warnings
	play      *playbook.Play  // the current play (task paths of synthesized tasks)
}

// New builds the default callback, auto-detecting color and terminal width.
func New(verbosity int) *Default {
	fd := int(os.Stdout.Fd())
	// Display.columns = max(79, tty width - 1).
	cols := 79
	if term.IsTerminal(fd) {
		if w, _, err := term.GetSize(fd); err == nil && w-1 > cols {
			cols = w - 1
		}
	}
	return &Default{Out: os.Stdout, Err: os.Stderr, Verbosity: verbosity, NoColor: NoColor(), Columns: cols}
}

// NoColor reports whether stdout output goes uncolored: not a terminal, or
// NO_COLOR/ANSIBLE_NOCOLOR set, unless ANSIBLE_FORCE_COLOR is.
func NoColor() bool {
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("ANSIBLE_NOCOLOR") != "" || !term.IsTerminal(int(os.Stdout.Fd()))
	if v := os.Getenv("ANSIBLE_FORCE_COLOR"); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		noColor = false
	}
	return noColor
}

func (d *Default) paint(c color, s string) string {
	if d.NoColor {
		return s
	}
	return "\x1b[" + string(c) + "m" + s + "\x1b[0m"
}

func (d *Default) display(c color, s string) {
	if c != "" {
		s = d.paint(c, s)
	}
	fmt.Fprintln(d.Out, s)
}

// banner prints "\n<msg> ****" padded to the display width (min 3 stars).
func (d *Default) banner(msg string) {
	cols := d.Columns
	if cols == 0 {
		cols = 79
	}
	stars := cols - utf8.RuneCountInString(msg)
	if stars <= 3 {
		stars = 3
	}
	fmt.Fprintf(d.Out, "\n%s %s\n", msg, strings.Repeat("*", stars))
}

func (d *Default) PlayStart(play *playbook.Play) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.play = play
	name := strings.TrimSpace(play.Name)
	if name == "" {
		name = play.HostPattern
	}
	d.banner(fmt.Sprintf("PLAY [%s]", name))
}

func (d *Default) TaskStart(task *playbook.Task, displayName string, handler bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := displayName
	if name == "" {
		name = task.DisplayAction()
	}
	kind := "TASK"
	if handler {
		kind = "RUNNING HANDLER"
	}
	d.banner(fmt.Sprintf("%s [%s]", kind, strings.TrimSpace(name)))
	if d.Verbosity >= 2 {
		d.taskPath(task)
	}
}

// taskPath is _print_task_path: "task path: <file>:<line>".
// A task built without a source of its own (a role's argument
// validation) shows its play's.
func (d *Default) taskPath(task *playbook.Task) {
	src := task.Src
	if task.Synthesized {
		if d.play == nil {
			return
		}
		src = d.play.Src
	}
	if src.File != "" && src.Line > 0 {
		d.display(cDebug, fmt.Sprintf("task path: %s:%d", src.File, src.Line))
	}
}

// PlaybookStart is v2_playbook_on_start: the -vv "PLAYBOOK: <file>" banner.
func (d *Default) PlaybookStart(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Verbosity > 1 {
		d.banner("PLAYBOOK: " + filepath.Base(path))
	}
}

// HandlerNotified is v2_playbook_on_notify.
func (d *Default) HandlerNotified(handler *playbook.Task, host string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Verbosity > 1 {
		d.display(cVerbose, fmt.Sprintf("NOTIFIED HANDLER %s for %s", handler.GetName(), host))
	}
}

func (d *Default) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if res.DelegatedTo != "" && res.DelegatedTo != host {
		host += " -> " + res.DelegatedTo
	}
	defer d.warnings(res)
	isItem := item != nil || (res.Extra != nil && res.Extra["ansible_loop_var"] != nil)
	itemLabel := ""
	if isItem {
		itemLabel = template.PyStr(item)
		if res.Censored {
			itemLabel = "(censored due to no_log)"
		}
	}

	switch {
	case res.Failed:
		d.taskError(task, res)
		if isItem {
			d.display(cRed, fmt.Sprintf("failed: [%s] (item=%s) => %s", host, itemLabel, d.dump(task, res)))
			return
		}
		d.display(cRed, fmt.Sprintf("fatal: [%s]: FAILED! => %s", host, d.dump(task, res)))
		if ignored {
			d.display(cCyan, "...ignoring")
		}
	case res.Skipped:
		line := fmt.Sprintf("skipping: [%s]", host)
		if isItem {
			line += fmt.Sprintf(" => (item=%s) ", itemLabel)
		}
		if d.runIsVerbose(task, res, 0) {
			line += " => " + d.dump(task, res)
		}
		d.display(cCyan, line)
	default:
		// v2_on_file_diff precedes v2_runner_on_ok.
		if res.ShowDiff && res.Changed && diffTruthy(res.Diff) {
			if s := d.getDiff(res.Diff); s != "" {
				// Display.display: CRLF -> LF, one trailing newline.
				s = strings.ReplaceAll(s, "\r\n", "\n")
				if !strings.HasSuffix(s, "\n") {
					s += "\n"
				}
				fmt.Fprint(d.Out, s)
			}
		}
		status, c := "ok", cGreen
		if res.Changed {
			status, c = "changed", cYellow
		}
		line := fmt.Sprintf("%s: [%s]", status, host)
		if isItem {
			line += fmt.Sprintf(" => (item=%s)", itemLabel)
		}
		if d.runIsVerbose(task, res, 0) {
			line += " => " + d.dump(task, res)
		}
		d.display(c, line)
	}
}

// LoopResult reports a loop's aggregate: ok/changed/failed aggregates print
// nothing beyond the per-item lines (and "...ignoring"); a fully skipped
// loop prints a final skipping line.
func (d *Default) LoopResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case res.Failed:
		if ignored {
			d.display(cCyan, "...ignoring")
		}
	case res.Skipped:
		line := fmt.Sprintf("skipping: [%s]", host)
		if d.runIsVerbose(task, res, 0) {
			line += " => " + d.dump(task, res)
		}
		d.display(cCyan, line)
	}
}

// Included is v2_playbook_on_include: "included: <file> for h1, h2".
func (d *Default) Included(task *playbook.Task, target string, hosts []string, item any, hasItem bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	line := fmt.Sprintf("included: %s for %s", target, strings.Join(hosts, ", "))
	if hasItem {
		line += fmt.Sprintf(" => (item=%s)", template.PyStr(item))
	}
	d.display(cCyan, line)
}

func (d *Default) NoHostsRemaining() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.banner("NO MORE HOSTS LEFT")
}

func (d *Default) HostUnreachable(host string, task *playbook.Task, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.HasPrefix(msg, "Task failed: ") {
		// A connection failure raised while running the task (become
		// timing out) carries its exception: the error block comes first.
		d.taskError(task, &agentproto.Result{Failed: true, Msg: msg, Origin: "verbatim"})
	}
	res := map[string]any{"changed": false, "msg": msg, "unreachable": true}
	d.display(cBrightRed, fmt.Sprintf("fatal: [%s]: UNREACHABLE! => %s", host, template.PyJSON(res, d.indent(false), true, false)))
}

func (d *Default) Recap(stats map[string]*executor.HostStats, order []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.banner("PLAY RECAP")
	hosts := append([]string(nil), order...)
	sort.Strings(hosts)
	for _, host := range hosts {
		st := stats[host]
		if !st.Processed() {
			continue
		}
		fmt.Fprintf(d.Out, "%s : %s %s %s %s %s %s %s\n",
			d.hostColor(host, st),
			d.colorize("ok", st.OK, cGreen),
			d.colorize("changed", st.Changed, cYellow),
			d.colorize("unreachable", st.Unreachable, cBrightRed),
			d.colorize("failed", st.Failed, cRed),
			d.colorize("skipped", st.Skipped, cCyan),
			d.colorize("rescued", st.Rescued, cGreen),
			d.colorize("ignored", st.Ignored, cBrightPurp),
		)
	}
	fmt.Fprintln(d.Out)
}

// hostColor is Ansible's hostcolor(): "%-26s" plain, "%-37s" colored (the
// escape codes count toward the width, exactly as in Python).
func (d *Default) hostColor(host string, st *executor.HostStats) string {
	if d.NoColor {
		return fmt.Sprintf("%-26s", host)
	}
	c := cGreen
	switch {
	case st.Failed > 0 || st.Unreachable > 0:
		c = cRed
	case st.Changed > 0:
		c = cYellow
	}
	return fmt.Sprintf("%-37s", d.paint(c, host))
}

// colorize is Ansible's colorize(): "lead=%-4s", colored only when nonzero.
func (d *Default) colorize(lead string, n int, c color) string {
	s := fmt.Sprintf("%s=%-4s", lead, strconv.Itoa(n))
	if n != 0 {
		return d.paint(c, s)
	}
	return s
}

// runIsVerbose is CallbackBase._run_is_verbose: past the given verbosity
// (or with verbose_always) a result is dumped, except a setup/gather_facts
// result, whose action sets _ansible_verbose_override.
func (d *Default) runIsVerbose(task *playbook.Task, res *agentproto.Result, verbosity int) bool {
	return (d.Verbosity > verbosity || res.VerboseAlways) && !verboseOverride(task.Module)
}

func verboseOverride(module string) bool {
	switch module {
	case "setup", "ansible.builtin.setup", "ansible.legacy.setup",
		"gather_facts", "ansible.builtin.gather_facts", "ansible.legacy.gather_facts":
		return true
	}
	return false
}

func (d *Default) indent(verboseAlways bool) int {
	if verboseAlways || d.Verbosity > 2 {
		return 4
	}
	return 0
}

// dump is CallbackBase._dump_results after _clean_results: internal keys,
// invocation/diff (below -vvv) and exception are dropped, debug output is
// trimmed to its message, and keys are sorted in Python-json form.
func (d *Default) dump(task *playbook.Task, res *agentproto.Result) string {
	if res.Censored {
		// TaskResult.clean_copy under no_log keeps only these keys.
		m := map[string]any{"censored": censoredMsg}
		if !isDebug(task.Module) { // debug results carry no changed key
			m["changed"] = res.Changed
		}
		for _, k := range []string{"attempts", "retries"} {
			if v, ok := res.Extra[k]; ok {
				m[k] = v
			}
		}
		return template.PyJSON(m, d.indent(false), true, false)
	}
	return d.dumpRaw(task, res, true)
}

func (d *Default) dumpRaw(task *playbook.Task, res *agentproto.Result, clean bool) string {
	m := res.ToVars()
	delete(m, "failed")
	delete(m, "skipped")
	for k := range m {
		if strings.HasPrefix(k, "_ansible_") {
			delete(m, k)
		}
	}
	if d.Verbosity < 3 {
		delete(m, "invocation")
		delete(m, "diff")
	}
	delete(m, "exception")
	delete(m, "warnings")
	delete(m, "deprecations")
	if _, loop := m["results"]; loop && task.Loop != nil {
		delete(m, "results")
	}
	if clean && isDebug(task.Module) {
		if msg, hasMsg := m["msg"]; hasMsg {
			for k := range m {
				if !debugAllowedKeys[k] {
					delete(m, k)
				}
			}
			if msg == nil {
				delete(m, "msg") // msg: null renders as {}
			}
		} else {
			for _, k := range debugHiddenKeys {
				delete(m, k)
			}
		}
	}
	return template.PyJSON(m, d.indent(res.VerboseAlways), true, false)
}

const censoredMsg = "the output has been hidden due to the fact that 'no_log: true' was specified for this result"

func isDebug(module string) bool {
	return module == "debug" || module == "ansible.builtin.debug" || module == "ansible.legacy.debug"
}

// warnings prints a result's module warnings (to stderr, as Display.warning
// does).
func (d *Default) warnings(res *agentproto.Result) {
	if d.Err == nil || res.Extra == nil {
		return
	}
	list, _ := res.Extra["warnings"].([]any)
	for _, w := range list {
		msg := template.PyStr(w)
		seen := d.warns[msg]
		if d.warns == nil {
			d.warns = map[string]bool{}
		}
		d.warns[msg] = true
		if seen {
			continue
		}
		// Display.display: a message ending in a newline gets no second one.
		fmt.Fprintf(d.Err, "%s\n", d.paint(cBrightPurp, "[WARNING]: "+strings.TrimSuffix(msg, "\n")))
	}
}

// taskError prints ansible-core's "[ERROR]: Task failed" block with the
// task's source excerpt. Identical blocks print once, as Display dedupes.
func (d *Default) taskError(task *playbook.Task, res *agentproto.Result) {
	if res.Origin == "plain" {
		return // a plain failure (not an exception) has no error block
	}
	var b strings.Builder
	if ec := res.ErrorChain; ec != nil {
		d.taskErrorChain(task, ec)
		return
	}
	switch res.Origin {
	case "verbatim":
		fmt.Fprintf(&b, "[ERROR]: %s\n", res.Msg)
	case "action":
		fmt.Fprintf(&b, "[ERROR]: Task failed: Action failed: %s\n", res.ErrorMessage())
	case "raised":
		fmt.Fprintf(&b, "[ERROR]: Task failed: %s\n", res.ErrorMessage())
	default:
		fmt.Fprintf(&b, "[ERROR]: Task failed: Module failed: %s\n", res.ErrorMessage())
	}
	if task.Src.File != "" && task.Src.Line > 0 {
		fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n", task.Src.File, task.Src.Line, task.Src.Col)
		b.WriteString(d.excerpt(task.Src.File, task.Src.Line, task.Src.Col))
	}
	// Display.display turns Windows newlines into Unix ones.
	block := strings.ReplaceAll(b.String(), "\r\n", "\n")
	if d.errors == nil {
		d.errors = map[string]bool{}
	}
	if d.errors[block] {
		return
	}
	d.errors[block] = true
	fmt.Fprint(d.Out, d.paint(cRed, strings.TrimRight(block, "\n")))
	fmt.Fprint(d.Out, "\n\n")
}

// taskErrorChain is format_event_verbose_message for an error caused by
// another: the brief chained message, the outer error with the task's
// source context, then "<<< caused by >>>" and the cause with its help.
func (d *Default) taskErrorChain(task *playbook.Task, ec *agentproto.ErrorChain) {
	var b strings.Builder
	brief := ec.Outer
	for _, cause := range []string{ec.Mid, ec.Inner} {
		if cause != "" && !strings.HasSuffix(brief, cause) {
			brief = strings.TrimRight(brief, ". ") + ": " + cause
		}
	}
	b.WriteString("[ERROR]: " + brief + "\n\n" + ec.Outer + "\n")
	if task.Src.File != "" && task.Src.Line > 0 {
		fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n", task.Src.File, task.Src.Line, task.Src.Col)
		b.WriteString(strings.TrimRight(d.excerpt(task.Src.File, task.Src.Line, task.Src.Col), "\n") + "\n")
	}
	if ec.Mid != "" {
		b.WriteString("\n<<< caused by >>>\n\n" + ec.Mid + "\n")
		if ec.MidFile != "" && ec.MidLine > 0 {
			fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n", ec.MidFile, ec.MidLine, ec.MidCol)
			b.WriteString(strings.TrimRight(d.excerpt(ec.MidFile, ec.MidLine, ec.MidCol), "\n") + "\n")
		}
	}
	b.WriteString("\n<<< caused by >>>\n\n")
	switch {
	case ec.InnerFile != "" && ec.InnerLine > 0:
		fmt.Fprintf(&b, "%s\nOrigin: %s:%d:%d\n\n", ec.Inner, ec.InnerFile, ec.InnerLine, ec.InnerCol)
		b.WriteString(d.excerpt(ec.InnerFile, ec.InnerLine, ec.InnerCol))
		if ec.Help != "" {
			b.WriteString("\n" + ec.Help)
		}
	case ec.Help != "" && !strings.Contains(ec.Inner, "\n") && !strings.Contains(ec.Help, "\n"):
		b.WriteString(ec.Inner + " " + ec.Help)
	case ec.Help != "":
		b.WriteString(ec.Inner + "\n\n" + ec.Help)
	default:
		b.WriteString(ec.Inner)
	}
	// Display.display turns Windows newlines into Unix ones.
	block := strings.ReplaceAll(b.String(), "\r\n", "\n")
	if d.errors == nil {
		d.errors = map[string]bool{}
	}
	if d.errors[block] {
		return
	}
	d.errors[block] = true
	fmt.Fprint(d.Out, d.paint(cRed, strings.TrimSpace(block)))
	fmt.Fprint(d.Out, "\n\n")
}

// excerpt renders the annotated source context of an origin.
func (d *Default) excerpt(file string, line, col int) string {
	return playbook.SourceContext(file, line, col)
}

const (
	cDebug   color = "0;90" // COLOR_DEBUG (dark gray)
	cVerbose color = "0;34" // COLOR_VERBOSE (blue)
)

// Retrying is v2_runner_retry: "FAILED - RETRYING: [host]: task (N retries left)."
func (d *Default) Retrying(host string, task *playbook.Task, name string, left int, res *agentproto.Result) {
	d.mu.Lock()
	defer d.mu.Unlock()
	line := fmt.Sprintf("FAILED - RETRYING: [%s]: %s (%d retries left).", host, name, left)
	if d.runIsVerbose(task, res, 2) {
		// v2_runner_retry dumps without _clean_results (no debug trim).
		line += "Result was: " + d.dumpRaw(task, res, false)
	}
	d.display(cDebug, line)
}

// AsyncPoll is v2_runner_on_async_poll.
func (d *Default) AsyncPoll(host, jid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.display(cDebug, fmt.Sprintf("ASYNC POLL on %s: jid=%s started=True finished=False", host, jid))
}

// AsyncDone is v2_runner_on_async_ok / _failed.
func (d *Default) AsyncDone(host, jid string, failed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kind := "OK"
	if failed {
		kind = "FAILED"
	}
	d.display(cDebug, fmt.Sprintf("ASYNC %s on %s: jid=%s", kind, host, jid))
}
