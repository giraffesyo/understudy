// Package callback renders execution events byte-for-byte like
// ansible-core's default stdout callback (reference: ansible-core 2.21):
// PLAY/TASK banners, per-host result lines with Python-json result dumps,
// "[ERROR]: Task failed" origin blocks, and the PLAY RECAP table.
package callback

import (
	"fmt"
	"io"
	"os"
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
	debugHiddenKeys  = []string{"changed", "failed", "skipped", "invocation", "skip_reason"}
)

// Default is the standard output callback.
type Default struct {
	Out       io.Writer
	Verbosity int
	NoColor   bool
	Columns   int // banner width (Display.columns); 0 = 79
	mu        sync.Mutex
	errors    map[string]bool // Display de-duplicates repeated errors
	srcCache  map[string][]string
}

// New builds the default callback, auto-detecting color and terminal width.
func New(verbosity int) *Default {
	fd := int(os.Stdout.Fd())
	isTTY := term.IsTerminal(fd)
	noColor := os.Getenv("NO_COLOR") != "" || os.Getenv("ANSIBLE_NOCOLOR") != "" || !isTTY
	if v := os.Getenv("ANSIBLE_FORCE_COLOR"); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		noColor = false
	}
	// Display.columns = max(79, tty width - 1).
	cols := 79
	if isTTY {
		if w, _, err := term.GetSize(fd); err == nil && w-1 > cols {
			cols = w - 1
		}
	}
	return &Default{Out: os.Stdout, Verbosity: verbosity, NoColor: noColor, Columns: cols}
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
		name = task.Module
	}
	kind := "TASK"
	if handler {
		kind = "RUNNING HANDLER"
	}
	d.banner(fmt.Sprintf("%s [%s]", kind, strings.TrimSpace(name)))
}

func (d *Default) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	d.mu.Lock()
	defer d.mu.Unlock()

	isItem := item != nil || (res.Extra != nil && res.Extra["ansible_loop_var"] != nil)
	itemLabel := ""
	if isItem {
		itemLabel = template.PyStr(item)
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
		if d.Verbosity > 0 {
			line += " => " + d.dump(task, res)
		}
		d.display(cCyan, line)
	default:
		status, c := "ok", cGreen
		if res.Changed {
			status, c = "changed", cYellow
		}
		line := fmt.Sprintf("%s: [%s]", status, host)
		if isItem {
			line += fmt.Sprintf(" => (item=%s)", itemLabel)
		}
		if d.Verbosity > 0 || res.VerboseAlways {
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
		if d.Verbosity > 0 {
			line += " => " + d.dump(task, res)
		}
		d.display(cCyan, line)
	}
}

func (d *Default) HostUnreachable(host string, task *playbook.Task, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
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
		if st == nil {
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
	m := res.ToVars()
	delete(m, "failed")
	delete(m, "skipped")
	if res.RC == nil {
		for _, k := range []string{"stdout_lines", "stderr_lines"} {
			delete(m, k)
		}
	}
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
	if isDebug(task.Module) {
		if _, hasMsg := m["msg"]; hasMsg {
			for k := range m {
				if !debugAllowedKeys[k] {
					delete(m, k)
				}
			}
		} else if name, ok := task.Args["var"].(string); ok && m[name] != nil {
			// var= output is just the variable (loop keys included).
			m = map[string]any{name: m[name]}
		} else {
			for _, k := range debugHiddenKeys {
				delete(m, k)
			}
		}
	}
	return template.PyJSON(m, d.indent(res.VerboseAlways), true, false)
}

func isDebug(module string) bool {
	return module == "debug" || module == "ansible.builtin.debug" || module == "ansible.legacy.debug"
}

// taskError prints ansible-core's "[ERROR]: Task failed" block with the
// task's source excerpt. Identical blocks print once, as Display dedupes.
func (d *Default) taskError(task *playbook.Task, res *agentproto.Result) {
	var b strings.Builder
	switch res.Origin {
	case "verbatim":
		fmt.Fprintf(&b, "[ERROR]: %s\n", res.Msg)
	case "action":
		fmt.Fprintf(&b, "[ERROR]: Task failed: Action failed: %s\n", res.Msg)
	default:
		fmt.Fprintf(&b, "[ERROR]: Task failed: Module failed: %s\n", res.Msg)
	}
	if task.Src.File != "" && task.Src.Line > 0 {
		fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n", task.Src.File, task.Src.Line, task.Src.Col)
		b.WriteString(d.excerpt(task.Src.File, task.Src.Line, task.Src.Col))
	}
	block := b.String()
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

// excerpt renders up to two lines of context plus the target line, with
// right-aligned line numbers and a caret under the column.
func (d *Default) excerpt(file string, line, col int) string {
	if d.srcCache == nil {
		d.srcCache = map[string][]string{}
	}
	lines, ok := d.srcCache[file]
	if !ok {
		data, err := os.ReadFile(file)
		if err == nil {
			lines = strings.Split(string(data), "\n")
		}
		d.srcCache[file] = lines
	}
	if line > len(lines) {
		return ""
	}
	width := len(strconv.Itoa(line))
	var b strings.Builder
	for n := max(1, line-2); n <= line; n++ {
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("%*d %s", width, n, lines[n-1]), " \t\r"))
	}
	fmt.Fprintf(&b, "%s^ column %d\n", strings.Repeat(" ", width+1+max(col-1, 0)), col)
	return b.String()
}
