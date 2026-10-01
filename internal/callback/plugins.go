package callback

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// Settings are the ansible.cfg / ANSIBLE_* callback settings.
type Settings struct {
	StdoutCallback      string   // "" = default
	CallbacksEnabled    []string // extra (notification/aggregate) callbacks
	DisplayOkHosts      bool
	DisplaySkippedHosts bool
	Verbosity           int
	Adhoc               bool                // the ad-hoc command's default is minimal
	PluginDirs          []string            // where external (executable) callback plugins live
	Extra               []executor.Callback // additional callbacks (Go API OnEvent)
}

// Build assembles the output callback chain the way ansible-core loads
// plugins: one stdout callback plus the enabled aggregate callbacks.
// Callbacks that exist only as Python plugins cannot be loaded; like
// Ansible, an unloadable stdout callback is an error and an unloadable
// extra callback is a warning.
func Build(s Settings, warn func(string)) (executor.Callback, error) {
	var stdout executor.Callback
	var out io.Writer = os.Stdout
	name := strings.TrimPrefix(strings.TrimPrefix(s.StdoutCallback, "ansible.builtin."), "community.general.")
	switch {
	case name == "" && s.Adhoc, name == "minimal":
		m := NewMinimal(s.Verbosity)
		stdout, out = m, m.writer()
	case name == "", name == "default":
		d := New(s.Verbosity)
		stdout, out = d, d.writer()
	default:
		path, _ := findPlugin(s.StdoutCallback, s.PluginDirs)
		if path == "" {
			return nil, fmt.Errorf("Could not load '%s' callback plugin.", s.StdoutCallback)
		}
		p, err := startExecPlugin(s.StdoutCallback, path, true, warn)
		if err != nil {
			return nil, err
		}
		stdout = p
	}

	cb := stdout
	if !s.DisplayOkHosts || !s.DisplaySkippedHosts {
		cb = &filtered{Callback: cb, showOK: s.DisplayOkHosts, showSkipped: s.DisplaySkippedHosts}
	}
	var extras []executor.Callback
	for _, n := range s.CallbacksEnabled {
		switch strings.TrimPrefix(n, "ansible.posix.") {
		case "timer":
			extras = append(extras, &timerCallback{out: out, start: time.Now()})
		case "profile_tasks":
			extras = append(extras, newProfileTasks(out))
		default:
			path, _ := findPlugin(n, s.PluginDirs)
			if path == "" {
				warn(fmt.Sprintf("Skipping callback plugin '%s', unable to load", n))
				continue
			}
			p, err := startExecPlugin(n, path, false, warn)
			if err != nil {
				warn(fmt.Sprintf("Skipping callback plugin '%s', unable to load: %v", n, err))
				continue
			}
			extras = append(extras, p)
		}
	}
	extras = append(extras, s.Extra...)
	if len(extras) == 0 {
		return cb, nil
	}
	return &fanout{primary: cb, extras: extras}, nil
}

func (d *Default) writer() io.Writer { return d.Out }

// filtered implements display_ok_hosts / display_skipped_hosts: hidden
// results print nothing, and a task's banner waits for its first shown
// result (Ansible defers it the same way).
type filtered struct {
	executor.Callback
	showOK, showSkipped bool
	mu                  sync.Mutex
	pending             *pendingBanner
}

type pendingBanner struct {
	task    *playbook.Task
	name    string
	handler bool
}

func (f *filtered) TaskStart(task *playbook.Task, name string, handler bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = &pendingBanner{task, name, handler}
}

func (f *filtered) flush() {
	if f.pending != nil {
		f.Callback.TaskStart(f.pending.task, f.pending.name, f.pending.handler)
		f.pending = nil
	}
}

func (f *filtered) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if (res.Skipped && !f.showSkipped) || (!res.Failed && !res.Skipped && !res.Changed && !f.showOK) {
		return
	}
	f.flush()
	f.Callback.HostResult(host, task, res, ignored, item)
}

func (f *filtered) LoopResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if res.Skipped && !f.showSkipped {
		return
	}
	f.flush()
	f.Callback.LoopResult(host, task, res, ignored)
}

func (f *filtered) Included(task *playbook.Task, target string, hosts []string, item any, hasItem bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
	f.Callback.Included(task, target, hosts, item, hasItem)
}

func (f *filtered) HostUnreachable(host string, task *playbook.Task, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
	f.Callback.HostUnreachable(host, task, msg)
}

func (f *filtered) NoHostsRemaining() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flush()
	f.Callback.NoHostsRemaining()
}

func (f *filtered) PlayStart(play *playbook.Play) {
	f.mu.Lock()
	f.pending = nil
	f.mu.Unlock()
	f.Callback.PlayStart(play)
}

func (f *filtered) PlaybookStart(path string) { executor.ForwardPlaybookStart(f.Callback, path) }

func (f *filtered) HandlerNotified(handler *playbook.Task, host string) {
	executor.ForwardHandlerNotified(f.Callback, handler, host)
}

// fanout sends every event to the stdout callback, then to each enabled
// aggregate callback in order.
type fanout struct {
	primary executor.Callback
	extras  []executor.Callback
}

func (m *fanout) PlayStart(p *playbook.Play) {
	m.primary.PlayStart(p)
	for _, c := range m.extras {
		c.PlayStart(p)
	}
}
func (m *fanout) NoHostsRemaining() {
	m.primary.NoHostsRemaining()
	for _, c := range m.extras {
		c.NoHostsRemaining()
	}
}
func (m *fanout) TaskStart(t *playbook.Task, name string, handler bool) {
	m.primary.TaskStart(t, name, handler)
	for _, c := range m.extras {
		c.TaskStart(t, name, handler)
	}
}
func (m *fanout) HostResult(h string, t *playbook.Task, r *agentproto.Result, ignored bool, item any) {
	m.primary.HostResult(h, t, r, ignored, item)
	for _, c := range m.extras {
		c.HostResult(h, t, r, ignored, item)
	}
}
func (m *fanout) LoopResult(h string, t *playbook.Task, r *agentproto.Result, ignored bool) {
	m.primary.LoopResult(h, t, r, ignored)
	for _, c := range m.extras {
		c.LoopResult(h, t, r, ignored)
	}
}
func (m *fanout) HostUnreachable(h string, t *playbook.Task, msg string) {
	m.primary.HostUnreachable(h, t, msg)
	for _, c := range m.extras {
		c.HostUnreachable(h, t, msg)
	}
}
func (m *fanout) Included(t *playbook.Task, target string, hosts []string, item any, hasItem bool) {
	m.primary.Included(t, target, hosts, item, hasItem)
	for _, c := range m.extras {
		c.Included(t, target, hosts, item, hasItem)
	}
}
func (m *fanout) PlaybookStart(path string) {
	executor.ForwardPlaybookStart(m.primary, path)
	for _, c := range m.extras {
		executor.ForwardPlaybookStart(c, path)
	}
}
func (m *fanout) HandlerNotified(handler *playbook.Task, host string) {
	executor.ForwardHandlerNotified(m.primary, handler, host)
	for _, c := range m.extras {
		executor.ForwardHandlerNotified(c, handler, host)
	}
}
func (m *fanout) Recap(stats map[string]*executor.HostStats, order []string) {
	m.primary.Recap(stats, order)
	for _, c := range m.extras {
		c.Recap(stats, order)
	}
}

// quiet is the no-op base for aggregate callbacks.
type quiet struct{}

func (quiet) PlayStart(*playbook.Play)                                         {}
func (quiet) NoHostsRemaining()                                                {}
func (quiet) TaskStart(*playbook.Task, string, bool)                           {}
func (quiet) HostResult(string, *playbook.Task, *agentproto.Result, bool, any) {}
func (quiet) LoopResult(string, *playbook.Task, *agentproto.Result, bool)      {}
func (quiet) HostUnreachable(string, *playbook.Task, string)                   {}
func (quiet) Included(*playbook.Task, string, []string, any, bool)             {}
func (quiet) Recap(map[string]*executor.HostStats, []string)                   {}

// banner is Display.banner at the default 79-column width.
func banner(w io.Writer, msg string) {
	stars := 79 - len([]rune(msg))
	if stars <= 3 {
		stars = 3
	}
	fmt.Fprintf(w, "\n%s %s\n", msg, strings.Repeat("*", stars))
}

// timerCallback is ansible.posix.timer: total run time after the recap.
type timerCallback struct {
	quiet
	out   io.Writer
	start time.Time
}

func (t *timerCallback) Recap(map[string]*executor.HostStats, []string) {
	d := int(time.Since(t.start).Seconds())
	banner(t.out, "PLAYBOOK RECAP")
	fmt.Fprintf(t.out, "Playbook run took %d days, %d hours, %d minutes, %d seconds\n\n",
		d/86400, d%86400/3600, d%3600/60, d%60)
}

// profileTasks is ansible.posix.profile_tasks: a timestamp line under each
// task banner and the slowest tasks at the end.
type profileTasks struct {
	quiet
	out     io.Writer
	t0, tn  time.Time
	mu      sync.Mutex
	current *profileEntry
	entries []*profileEntry
}

type profileEntry struct {
	name    string
	started time.Time
	elapsed time.Duration
}

func newProfileTasks(out io.Writer) *profileTasks {
	now := time.Now()
	return &profileTasks{out: out, t0: now, tn: now}
}

// taskTime renders profile_tasks' "Weekday DD Month YYYY  HH:MM:SS -ZZZZ
// (since last)       since start *****" line.
func (p *profileTasks) taskTime() string {
	now := time.Now()
	msg := fmt.Sprintf("%s (%s)%s%s", now.Format("Monday 02 January 2006  15:04:05 -0700"),
		profileDuration(now.Sub(p.tn)), strings.Repeat(" ", 7), profileDuration(now.Sub(p.t0)))
	p.tn = now
	return filled(msg, "*")
}

// filled is profile_tasks' filled(): pad with fchar to 79 columns plus a
// trailing space.
func filled(msg, fchar string) string {
	width := 79
	if msg != "" {
		msg += " "
		width = 79 - len([]rune(msg))
	}
	if width < 3 {
		width = 3
	}
	return msg + strings.Repeat(fchar, width) + " "
}

func profileDuration(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func (p *profileTasks) TaskStart(t *playbook.Task, name string, _ bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeCurrent()
	fmt.Fprintln(p.out, p.taskTime())
	p.current = &profileEntry{name: name, started: time.Now()}
	p.entries = append(p.entries, p.current)
}

func (p *profileTasks) closeCurrent() {
	if p.current != nil {
		p.current.elapsed = time.Since(p.current.started)
		p.current = nil
	}
}

func (p *profileTasks) Recap(map[string]*executor.HostStats, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeCurrent()
	banner(p.out, "TASKS RECAP")
	fmt.Fprintln(p.out, p.taskTime())
	fmt.Fprintln(p.out, filled("", "="))
	sorted := append([]*profileEntry(nil), p.entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].elapsed > sorted[j].elapsed })
	if len(sorted) > 20 {
		sorted = sorted[:20]
	}
	for _, e := range sorted {
		name := e.name + " "
		if pad := 70 - len([]rune(name)); pad > 0 {
			name += strings.Repeat("-", pad)
		}
		val := fmt.Sprintf(" %.02fs", e.elapsed.Seconds())
		if pad := 9 - len(val); pad > 0 {
			val = strings.Repeat("-", pad) + val
		}
		fmt.Fprintln(p.out, name+val)
	}
}

func (quiet) Retrying(string, *playbook.Task, string, int, *agentproto.Result) {}
func (quiet) AsyncPoll(string, string)                                         {}
func (quiet) AsyncDone(string, string, bool)                                   {}

func (m *fanout) Retrying(h string, t *playbook.Task, name string, left int, r *agentproto.Result) {
	m.primary.Retrying(h, t, name, left, r)
	for _, c := range m.extras {
		c.Retrying(h, t, name, left, r)
	}
}
func (m *fanout) AsyncPoll(h, jid string) {
	m.primary.AsyncPoll(h, jid)
	for _, c := range m.extras {
		c.AsyncPoll(h, jid)
	}
}
func (m *fanout) AsyncDone(h, jid string, failed bool) {
	m.primary.AsyncDone(h, jid, failed)
	for _, c := range m.extras {
		c.AsyncDone(h, jid, failed)
	}
}

// Fanout delivers every event to primary, then to each extra callback.
func Fanout(primary executor.Callback, extras ...executor.Callback) executor.Callback {
	return &fanout{primary: primary, extras: extras}
}
