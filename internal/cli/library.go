package cli

import (
	"context"
	"errors"
	"io"

	"github.com/giraffesyo/understudy/internal/config"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// Request is a playbook run started from Go (understudy.Run and
// understudy.RunFiles): ansible-playbook with the equivalent options,
// configuration and environment included.
type Request struct {
	Playbooks []string         // playbook files, in order
	Plays     []*playbook.Play // playbook structs, run after the files
	BaseDir   string           // where Plays' relative paths resolve

	// Inventory sources; none means just the implicit localhost.
	Inventory []string
	Limit     string

	ExtraVarsFiles []string       // each as -e @file, in order
	ExtraVars      map[string]any // over the files

	Forks     int
	CheckMode bool
	Diff      bool
	Verbosity int

	Become         bool
	BecomeUser     string
	BecomeMethod   string
	BecomePassword string

	Connection      string
	RemoteUser      string
	PrivateKey      string
	HostKeyChecking *bool

	Tags     []string
	SkipTags []string

	VaultPasswords []string

	// Settings are configuration as ANSIBLE_* environment variables, over
	// the process environment for this run.
	Settings map[string]string

	Output    io.Writer // nil = os.Stdout
	NoColor   bool
	Callbacks []executor.Callback
}

// Execute runs a Request: the exit status ansible-playbook would give, the
// runner (for its stats), and the error that stopped the run before it
// finished, if any.
func Execute(ctx context.Context, r Request) (int, *executor.Runner, error) {
	if len(r.Playbooks) == 0 && r.Plays == nil {
		return 1, nil, errors.New("no playbook to run")
	}
	defer config.SetOverrides(r.Settings)()
	cfg, err := config.Load()
	if err != nil {
		return exitCode(err, 5), nil, err
	}
	for _, w := range cfg.Warnings {
		warnOnce(w + "\n")
	}
	p := &parsedArgs{
		prog:              "ansible-playbook",
		positional:        r.Playbooks,
		plays:             r.Plays,
		playbookDir:       r.BaseDir,
		inventory:         r.Inventory,
		implicitInventory: len(r.Inventory) == 0,
		limit:             r.Limit,
		extraVars:         map[string]any{},
		extraVarsGo:       r.ExtraVars,
		forks:             r.Forks,
		check:             r.CheckMode,
		diff:              r.Diff,
		verbosity:         r.Verbosity,
		become:            r.Become,
		becomeUser:        r.BecomeUser,
		becomeMethod:      r.BecomeMethod,
		becomePass:        r.BecomePassword,
		connection:        r.Connection,
		remoteUser:        r.RemoteUser,
		privateKey:        r.PrivateKey,
		hostKeyChecking:   r.HostKeyChecking,
		vaultPasswords:    r.VaultPasswords,
		out:               r.Output,
		noColor:           r.NoColor,
		callbacks:         r.Callbacks,
	}
	if p.verbosity == 0 {
		p.verbosity = cfg.Verbosity
	}
	if p.playbookDir == "" {
		p.playbookDir = "."
	}
	for _, f := range r.ExtraVarsFiles {
		p.extraVarArgs = append(p.extraVarArgs, "@"+f)
	}
	for _, t := range r.Tags {
		p.tags = joinCSV(p.tags, t)
	}
	for _, t := range r.SkipTags {
		p.skipTags = joinCSV(p.skipTags, t)
	}
	l, err := loadPlaybooks(p)
	if err != nil {
		return exitCode(err, 1), nil, err
	}
	code, runner, err := l.run(ctx, p)
	if err != nil {
		return exitCode(err, 1), runner, err
	}
	return code, runner, nil
}
