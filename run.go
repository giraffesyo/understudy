package understudy

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/cli"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// A program embedding understudy doubles as the agent for become on local
// connections: such a task's module runs in a child of the program,
// started through the become method with modules.LocalAgentArg.
func init() {
	modules.LocalAgent = true
	if len(os.Args) == 3 && os.Args[1] == modules.LocalAgentArg && os.Args[2] == "run" {
		os.Exit(modules.ServeFrame(os.Stdin, os.Stdout))
	}
}

// Options configure a programmatic run. The zero value targets the
// implicit localhost inventory with default settings. As with
// ansible-playbook, ansible.cfg and the ANSIBLE_* environment configure
// everything the options leave unset (gathering, fact caching, roles
// path, callbacks, timeouts, ...).
type Options struct {
	// Inventory sources: file/directory paths or literal host lists
	// ("web1,web2,"). Empty means implicit localhost (local connection),
	// whatever ansible.cfg names.
	Inventory []string
	Limit     string // host pattern intersected with each play's hosts

	// ExtraVarsFiles are YAML or JSON files of extra vars, loaded in order
	// as -e @file; ExtraVars apply over them.
	ExtraVarsFiles []string
	ExtraVars      map[string]any

	Forks     int
	CheckMode bool
	Diff      bool

	Become         bool
	BecomeUser     string
	BecomeMethod   string // sudo (default), su or doas
	BecomePassword string

	// Connection forces a transport ("local", "ssh"); empty uses each
	// host's ansible_connection with smart defaults.
	Connection      string
	RemoteUser      string
	PrivateKey      string
	HostKeyChecking *bool // nil = ansible.cfg / default (on)

	Tags     []string
	SkipTags []string

	// Settings configure the run as ANSIBLE_* environment variables would
	// ({"ANSIBLE_GATHERING": "smart"}), over the process environment and
	// ansible.cfg, without changing either.
	Settings map[string]string

	// BaseDir anchors relative template/copy/vars_files sources.
	// Defaults to the current working directory.
	BaseDir string

	// VaultPasswords decrypt !vault values and whole-file-encrypted
	// vars_files / group_vars. Any listed password may match.
	VaultPasswords []string

	// Output receives playbook progress in ansible-playbook's format.
	// Defaults to os.Stdout; use io.Discard to silence. Output that is
	// not a terminal is uncolored. Warnings go to os.Stderr.
	Output    io.Writer
	NoColor   bool
	Verbosity int

	// OnEvent, when set, receives every callback event (the same stream
	// external callback plugins get), serially and in order.
	OnEvent func(Event)
}

// Event is one callback event, named after ansible-core's CallbackBase
// hooks (v2_playbook_on_task_start, v2_runner_on_ok, v2_playbook_on_stats,
// ...). Result holds the task result in its registered-variable form.
type Event = callback.Event

// EventPlay and EventTask are the play and task an Event names.
type (
	EventPlay = callback.EventPlay
	EventTask = callback.EventTask
)

// HostResult is one host's recap counters.
type HostResult struct {
	OK, Changed, Unreachable, Failed, Skipped, Rescued, Ignored int
}

// Result summarizes a run.
type Result struct {
	Hosts    map[string]HostResult
	ExitCode int // 0 success, 2 when any host failed or was unreachable
}

// Failed reports whether any host failed or was unreachable.
func (r *Result) Failed() bool { return r.ExitCode != 0 }

// Run executes a playbook directly. The playbook structs flow through the
// same loader and executor as YAML files, so behavior is identical to
// rendering with YAML() and running the file.
func Run(ctx context.Context, pb Playbook, opts Options) (*Result, error) {
	data, err := pb.YAML()
	if err != nil {
		return nil, err
	}
	plays, err := playbook.Load(data, "<go-playbook>")
	if err != nil {
		return nil, fmt.Errorf("internal rendering error: %w", err)
	}
	return execute(ctx, cli.Request{Plays: plays}, opts)
}

// RunFiles runs playbook files as ansible-playbook does, one run with a
// single recap. Relative template, copy and vars_files sources resolve
// against each playbook's directory, and roles against the playbook's
// roles/ directory and the configured roles path; BaseDir is unused.
func RunFiles(ctx context.Context, playbooks []string, opts Options) (*Result, error) {
	return execute(ctx, cli.Request{Playbooks: playbooks}, opts)
}

// execute runs a request with the options, configured by ansible.cfg and
// the ANSIBLE_* environment as ansible-playbook would be.
func execute(ctx context.Context, req cli.Request, opts Options) (*Result, error) {
	req.BaseDir = opts.BaseDir
	req.Inventory = opts.Inventory
	req.Limit = opts.Limit
	req.ExtraVarsFiles = opts.ExtraVarsFiles
	req.ExtraVars = opts.ExtraVars
	req.Forks = opts.Forks
	req.CheckMode = opts.CheckMode
	req.Diff = opts.Diff
	req.Verbosity = opts.Verbosity
	req.Become = opts.Become
	req.BecomeUser = opts.BecomeUser
	req.BecomeMethod = opts.BecomeMethod
	req.BecomePassword = opts.BecomePassword
	req.Connection = opts.Connection
	req.RemoteUser = opts.RemoteUser
	req.PrivateKey = opts.PrivateKey
	req.HostKeyChecking = opts.HostKeyChecking
	req.Tags = opts.Tags
	req.SkipTags = opts.SkipTags
	req.VaultPasswords = opts.VaultPasswords
	req.Settings = opts.Settings
	req.Output = opts.Output
	req.NoColor = opts.NoColor
	if opts.OnEvent != nil {
		req.Callbacks = append(req.Callbacks, callback.NewEventCallback(opts.OnEvent))
	}
	defer func() { inventory.Decrypt = nil }()
	code, runner, err := cli.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	stats := runner.Stats()
	res := &Result{Hosts: make(map[string]HostResult, len(stats)), ExitCode: code}
	for host, st := range stats {
		res.Hosts[host] = HostResult{
			OK: st.OK, Changed: st.Changed, Unreachable: st.Unreachable,
			Failed: st.Failed, Skipped: st.Skipped, Rescued: st.Rescued,
			Ignored: st.Ignored,
		}
	}
	return res, nil
}
