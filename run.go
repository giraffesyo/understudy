package understudy

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/vault"
)

// Options configure a programmatic run. The zero value targets the
// implicit localhost inventory with default settings.
type Options struct {
	// Inventory sources: file/directory paths or literal host lists
	// ("web1,web2,"). Empty means implicit localhost (local connection).
	Inventory []string
	Limit     string // host pattern intersected with each play's hosts

	ExtraVars map[string]any

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

	// BaseDir anchors relative template/copy/vars_files sources.
	// Defaults to the current working directory.
	BaseDir string

	// VaultPasswords decrypt !vault values and whole-file-encrypted
	// vars_files / group_vars. Any listed password may match.
	VaultPasswords []string

	// Output receives playbook progress in ansible-playbook's format.
	// Defaults to os.Stdout; use io.Discard to silence.
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
	roleBase := opts.BaseDir
	if roleBase == "" {
		roleBase = "."
	}
	var secrets *vault.Secrets
	if len(opts.VaultPasswords) > 0 {
		secrets = vault.NewSecrets(opts.VaultPasswords...)
		inventory.Decrypt = secrets.MaybeDecryptFile
		defer func() { inventory.Decrypt = nil }()
	}
	if err := playbook.ResolveRoles(plays, roleBase, nil); err != nil {
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	hostKeyChecking := cfg.HostKeyChecking
	if opts.HostKeyChecking != nil {
		hostKeyChecking = *opts.HostKeyChecking
	}
	forks := opts.Forks
	if forks <= 0 {
		forks = cfg.Forks
	}
	remoteUser := opts.RemoteUser
	if remoteUser == "" {
		remoteUser = cfg.RemoteUser
	}
	privateKey := opts.PrivateKey
	if privateKey == "" {
		privateKey = cfg.PrivateKeyFile
	}
	baseDir := opts.BaseDir
	if baseDir == "" {
		baseDir = "."
	}

	inv, err := inventory.Load(opts.Inventory, []string{baseDir})
	if err != nil {
		return nil, err
	}

	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	var cb executor.Callback = &callback.Default{Out: out, Verbosity: opts.Verbosity, NoColor: opts.NoColor}
	if opts.OnEvent != nil {
		cb = callback.Fanout(cb, callback.NewEventCallback(opts.OnEvent))
	}

	runner := executor.NewRunner(inv, cb, executor.Options{
		Forks:        forks,
		CheckMode:    opts.CheckMode,
		Diff:         opts.Diff,
		Verbosity:    opts.Verbosity,
		ExtraVars:    opts.ExtraVars,
		Become:       opts.Become,
		BecomeUser:   opts.BecomeUser,
		BecomeMethod: opts.BecomeMethod,
		BecomePass:   opts.BecomePassword,
		Connection:   opts.Connection,
		BaseDir:      baseDir,
		Tags:         opts.Tags,
		SkipTags:     opts.SkipTags,
		Vault:        secrets,
		ConnOpts: connection.ManagerOptions{
			RemoteUser:      remoteUser,
			PrivateKey:      privateKey,
			HostKeyChecking: hostKeyChecking,
			Timeout:         cfg.Timeout,
			RemoteTmp:       cfg.RemoteTmp,
		},
	})
	runner.Limit = opts.Limit

	code, err := runner.Run(ctx, plays)
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
