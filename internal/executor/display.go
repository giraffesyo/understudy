package executor

import (
	"context"
	"fmt"
	"os"
	"os/user"

	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// PlaybookCallback is implemented by callbacks that also take the
// playbook-level hooks ansible-core's default callback prints at -vv.
type PlaybookCallback interface {
	// PlaybookStart is v2_playbook_on_start (the "PLAYBOOK: <file>" banner).
	PlaybookStart(path string)
	// HandlerNotified is v2_playbook_on_notify: a handler queued to run
	// on a host ("NOTIFIED HANDLER <name> for <host>").
	HandlerNotified(handler *playbook.Task, host string)
}

// NoHostsMatchedCallback is implemented by callbacks that report a play
// whose pattern matched no hosts (v2_playbook_on_no_hosts_matched).
type NoHostsMatchedCallback interface {
	NoHostsMatched()
}

// ForwardNoHostsMatched reports a play that matched no hosts to a
// callback that shows it.
func ForwardNoHostsMatched(cb Callback) {
	if c, ok := cb.(NoHostsMatchedCallback); ok {
		c.NoHostsMatched()
	}
}

func (f *freeCallback) NoHostsMatched() { ForwardNoHostsMatched(f.Callback) }

// COLOR_VERBOSE, the color Display.verbose (display.v, display.vv, ...)
// prints in.
const colorVerbose = "0;34"

// displayVerbose is Display.verbose: msg on stdout when the run is at
// least that verbose.
func (r *Runner) displayVerbose(level int, msg string) {
	if r.Opts.Verbosity < level {
		return
	}
	if !r.Opts.NoColor {
		msg = "\x1b[" + colorVerbose + "m" + msg + "\x1b[0m"
	}
	fmt.Fprintln(os.Stdout, msg)
}

// displayRedirects is the plugin loader's -vv "redirecting" lines as a
// task executes on a host: TaskExecutor resolves a redirected action
// three times, a redirected module twice before the action runs (and
// once more as the module is built, see displayModuleRedirect).
func (r *Runner) displayRedirects(task *playbook.Task) {
	if r.Opts.Verbosity < 2 {
		return
	}
	notes, times := playbook.ModuleRedirects(task.Action), 2
	if a := playbook.ActionRedirect(task.Action); a != "" {
		notes, times = []string{a}, 3
	}
	for range times {
		for _, n := range notes {
			r.displayVerbose(2, n)
		}
	}
}

// displayModuleRedirect is the third resolution of a task's redirected
// module, as the action builds it (after the connection is up).
func (r *Runner) displayModuleRedirect(task *playbook.Task, module string) {
	if r.Opts.Verbosity < 2 || module != task.Module || playbook.ActionRedirect(task.Action) != "" {
		return
	}
	for _, n := range playbook.ModuleRedirects(task.Action) {
		r.displayVerbose(2, n)
	}
}

// connectedKey marks a task execution's context with the address its
// connection last announced itself for.
type connectedKey struct{}

// connectingNote returns the hook a task execution calls as it first uses
// the connection: the local connection plugin's -vvv "ESTABLISH LOCAL
// CONNECTION" line, once per task and host. (Over SSH, ansible-core's
// lines trace each ssh command it runs, which understudy's agent protocol
// does not share.)
func (r *Runner) connectingNote(ctx context.Context, local bool, vctx *vars.Context, host, target string) func() {
	last, _ := ctx.Value(connectedKey{}).(*string)
	return func() {
		if r.Opts.Verbosity < 3 || last == nil {
			return
		}
		// remote_addr: the target's ansible_host, else its name (the
		// implicit localhost's address, 127.0.0.1, unless delegated to).
		addr := target
		if v, ok := r.Store.RawHostVar(target, "ansible_host"); ok {
			if tv, err := vctx.TemplateValue(v); err == nil {
				addr = fmt.Sprint(tv)
			}
		} else if target == host && (target == "localhost" || target == "127.0.0.1") && r.Inv.Hosts[target] == nil {
			addr = "127.0.0.1"
		}
		// A loop keeps its connection while the address stays the same.
		if *last == addr {
			return
		}
		*last = addr
		if local {
			r.displayVerbose(3, fmt.Sprintf("<%s> ESTABLISH LOCAL CONNECTION FOR USER: %s", addr, localUser()))
		}
	}
}

// transfersFiles are the actions that set up a remote temp dir (and so
// connect) as soon as they run, before any check can fail them.
var transfersFiles = map[string]bool{
	"copy": true, "template": true, "unarchive": true, "script": true, "uri": true,
}

// localUser is getpass.getuser(): LOGNAME, USER, LNAME or USERNAME, else
// the password database's name for the uid.
func localUser() string {
	for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// displayLoadNotes prints the -vv lines loading these plays printed.
func (r *Runner) displayLoadNotes(plays []*playbook.Play) {
	for _, p := range plays {
		for _, n := range p.LoadNotes {
			r.displayVerbose(2, n)
		}
	}
}

// ForwardPlaybookStart and ForwardHandlerNotified pass the optional
// PlaybookCallback hooks to a wrapped callback that takes them.
func ForwardPlaybookStart(cb Callback, path string) {
	if pc, ok := cb.(PlaybookCallback); ok {
		pc.PlaybookStart(path)
	}
}

// ForwardCustomStats hands the run's custom stats to a callback that
// shows them.
func ForwardCustomStats(cb Callback, custom map[string]*yaml.OMap) {
	if cs, ok := cb.(CustomStatsCallback); ok {
		cs.CustomStats(custom)
	}
}

func ForwardHandlerNotified(cb Callback, handler *playbook.Task, host string) {
	if pc, ok := cb.(PlaybookCallback); ok {
		pc.HandlerNotified(handler, host)
	}
}

func (f *freeCallback) PlaybookStart(path string) { ForwardPlaybookStart(f.Callback, path) }

func (f *freeCallback) HandlerNotified(handler *playbook.Task, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ForwardHandlerNotified(f.Callback, handler, host)
}
