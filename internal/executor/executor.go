// Package executor runs plays with the linear strategy: every host finishes
// task N before task N+1 starts, with per-task parallelism bounded by forks.
package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/vault"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// Callback receives execution events for display.
type Callback interface {
	PlayStart(play *playbook.Play)
	TaskStart(task *playbook.Task, displayName string, handler bool)
	// HostResult reports one host's result; for loops it is called per item
	// with the item's display label, then LoopResult reports the aggregate.
	HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any)
	LoopResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool)
	HostUnreachable(host string, task *playbook.Task, msg string)
	// NoHostsRemaining reports that no hosts are left to run the play on
	// (v2_playbook_on_no_hosts_remaining).
	NoHostsRemaining()
	// Retrying reports a failed until: attempt with retries left.
	Retrying(host string, task *playbook.Task, name string, left int, res *agentproto.Result)
	// AsyncPoll / AsyncDone report a poll > 0 async job's progress.
	AsyncPoll(host, jid string)
	AsyncDone(host, jid string, failed bool)
	// Included announces a dynamic include's target for a group of hosts
	// (with the loop item's label when hasItem).
	Included(task *playbook.Task, target string, hosts []string, item any, hasItem bool)
	Recap(stats map[string]*HostStats, order []string)
}

// HostStats is one host's play-recap line.
type HostStats struct {
	OK, Changed, Unreachable, Failed, Skipped, Rescued, Ignored int
}

// Processed reports whether the host has any result counted
// (AggregateStats.processed): only those hosts appear in the recap, so a
// host whose tasks were all skipped by --step, or that only ran meta
// tasks, is left out.
func (st *HostStats) Processed() bool {
	return st != nil && *st != HostStats{}
}

// Options configure a run.
type Options struct {
	Forks     int
	CheckMode bool
	Diff      bool
	Verbosity int
	ExtraVars map[string]any
	// ExtraVarOrigins are where the extra vars named reserved variables.
	ExtraVarOrigins []template.KeyOrigin
	// ExtraVarValues are where each extra var's value came from.
	ExtraVarValues map[string]template.Position
	Become         bool
	BecomeUser     string
	BecomeMethod   string // --become-method ("" = sudo)
	BecomePass     string
	Connection     string // "" = per-host behavioral vars; "local" forces local
	BaseDir        string // playbook directory
	Tags           []string
	SkipTags       []string
	RolesPath      []string                  // roles_path search directories (after <playbook>/roles)
	ConfigFile     string                    // ansible.cfg in effect ("" = none): ansible_config_file
	Inventory      []string                  // inventory sources: ansible_inventory_sources
	ConnOpts       connection.ManagerOptions // ssh-level settings (user, keys, host key checking)

	ForceHandlers bool           // --force-handlers: notified handlers run on failed hosts too
	StartAtTask   string         // --start-at-task: skip tasks until one matches
	Step          bool           // --step: confirm each task interactively
	Vault         *vault.Secrets // vault passwords for !vault values and encrypted files

	// NoDeprecationWarnings is deprecation_warnings=False.
	NoDeprecationWarnings bool
	// AllowBrokenConditionals is ALLOW_BROKEN_CONDITIONALS.
	AllowBrokenConditionals bool
	// InjectFactsSet is INJECT_FACTS_AS_VARS set explicitly (not left at
	// its deprecated default): top-level facts then do not warn.
	InjectFactsSet bool
	// TaskTimeout is TASK_TIMEOUT: the timeout keyword's default, in
	// seconds (0 = none).
	TaskTimeout int
	// NoColor leaves the runner's own verbose lines (Display.vv) uncolored.
	NoColor bool
	// RefreshInventory re-parses the inventory sources for meta:
	// refresh_inventory, as ansible-core's InventoryManager does (its
	// plugins' output and warnings included). nil: nothing to re-read.
	RefreshInventory func()
}

// Runner executes playbooks.
type Runner struct {
	Inv      *inventory.Inventory
	Engine   *template.Engine
	Store    *vars.Store
	Conns    *connection.Manager
	Callback Callback
	Opts     Options
	Limit    string // --limit pattern, intersected with each play's hosts
	// BookPaths name each playbook RunPlaybooks runs, as given on the
	// command line ("N plays in <path>" at -vv); nil = the plays' files.
	BookPaths []string
	// CallbackNotes are the -vv lines loading the callbacks printed
	// (skipped stdout callbacks), shown once the first playbook loads.
	CallbackNotes []string

	// DebugIn/DebugOut are the task debugger's terminal (default stdin
	// and stdout).
	DebugIn  io.Reader
	DebugOut io.Writer

	stats          map[string]*HostStats
	order          []string
	failed         map[string]bool
	notified       map[*playbook.Task]map[string]bool // handler -> hosts to run on
	roleRan        map[string]map[string]bool         // role load key -> hosts a task of it ran on
	roleDone       map[string]map[string]bool         // role load key -> hosts it completed on
	notifyOrder    map[string][]string                // host -> notifications saved, in order
	handlerNames   map[*playbook.Task]*string         // templated handler names (nil: unusable)
	fatalErr       error                              // an error raised processing results: ends the run
	inHandlers     bool                               // a handler flush is running
	blockFailed    map[string]map[int]bool            // host -> block ID -> failure caught by rescue
	nextBlockID    int                                // fresh IDs for blocks of included files
	failedIn       map[string]map[int]bool            // host -> blocks it was inside when it failed hard
	ended          map[string]bool                    // meta: end_host (per play)
	runOnceHosts   []string                           // hosts a running run_once task fans out to
	curPlay        *playbook.Play
	warned         map[string]bool
	freeSem        *turnstile   // free strategy: forks shared fairly across hosts
	parallelActive map[int]bool // parallel blocks currently running
	startedAt      bool
	stepContinue   bool
	aborted        bool            // any_errors_fatal, a failed batch: stop the playbook
	maxFailBroke   bool            // max_fail_percentage ended the playbook
	unreachable    map[string]bool // hosts unreachable so far (until clear_host_errors)
	batchFailed    map[string]bool // hosts already failed when the batch started
	userQuit       bool            // task debugger quit, --step at EOF: exit quitCode, no recap
	quitCode       int
	dbgReader      *bufio.Reader
	dbgMu          sync.Mutex
	playEnded      bool // meta: end_play
	batchEnded     bool // meta: end_batch
	mu             sync.Mutex
	implicitMu     sync.Mutex
	implicitSet    bool // the implicit localhost's inventory vars are set
}

// NewRunner builds a runner over a loaded inventory.
func NewRunner(inv *inventory.Inventory, cb Callback, opts Options) *Runner {
	if opts.Forks <= 0 {
		opts.Forks = 5
	}
	engine := template.New()
	engine.AllowBrokenConditionals = opts.AllowBrokenConditionals
	r := &Runner{
		Inv:      inv,
		Engine:   engine,
		Store:    vars.NewStore(engine),
		Callback: cb,
		Opts:     opts,
		stats:    map[string]*HostStats{},
		failed:   map[string]bool{},
	}
	for _, name := range inv.SortedHostNames() {
		r.Store.SetInventoryVars(name, inv.EffectiveVars(inv.Hosts[name]))
	}
	if opts.ExtraVars != nil {
		r.Store.SetExtraVars(opts.ExtraVars)
		r.Store.SetValueOrigins(vars.LExtraVars, "", opts.ExtraVarValues)
	}
	connOpts := opts.ConnOpts
	connOpts.Connection = opts.Connection
	r.Conns = connection.NewManager(r.Store, connOpts)
	if opts.Vault != nil && !opts.Vault.Empty() {
		r.Store.VaultDecrypt = func(v yaml.VaultedString) (string, error) {
			out, err := opts.Vault.Decrypt(v.Ciphertext)
			return string(out), err
		}
	}
	r.installLookups()
	r.Engine.Deprecation = r.deprecation
	r.Engine.Verbose = r.displayVerbose
	return r
}

// registerImplicit gives the implicit localhost, once a pattern has
// created it, its inventory variables.
func (r *Runner) registerImplicit(h *inventory.Host) {
	if !h.Implicit() {
		return
	}
	r.implicitMu.Lock()
	defer r.implicitMu.Unlock()
	if !r.implicitSet {
		r.implicitSet = true
		r.Store.SetInventoryVars(h.Name, r.Inv.EffectiveVars(h))
	}
}

// Playbooks load before a Runner exists: an action nothing implements is
// a load error, as in ansible-core.
func init() { playbook.ModuleKnown = actions.Known }

// resolvePlayHosts matches a play's pattern (∩ --limit) against inventory,
// initializing stats rows for newly seen hosts.
func (r *Runner) resolvePlayHosts(play *playbook.Play) ([]string, error) {
	hosts, err := r.Inv.Match(play.HostPattern)
	if err != nil {
		return nil, err
	}
	if r.Limit != "" {
		limited, err := r.Inv.Match(r.Limit)
		if err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, h := range limited {
			keep[h.Name] = true
		}
		var filtered []*inventory.Host
		for _, h := range hosts {
			if keep[h.Name] {
				filtered = append(filtered, h)
			}
		}
		hosts = filtered
	}
	var names []string
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range hosts {
		names = append(names, h.Name)
		r.registerImplicit(h)
		if _, ok := r.stats[h.Name]; !ok {
			r.stats[h.Name] = &HostStats{}
			r.order = append(r.order, h.Name)
		}
	}
	return names, nil
}

// Run executes plays as one playbook and returns the exit code (0 ok, 2
// failed hosts, 4 unreachable hosts).
func (r *Runner) Run(ctx context.Context, plays []*playbook.Play) (int, error) {
	return r.RunPlaybooks(ctx, [][]*playbook.Play{plays})
}

// RunPlaybooks runs each playbook's plays in turn, as PlaybookExecutor.run
// does: every playbook ends with its own recap (the stats accumulate
// across playbooks), and a playbook whose result is not 0 is the last one
// run. The exit code is that result.
func (r *Runner) RunPlaybooks(ctx context.Context, books [][]*playbook.Play) (int, error) {
	code := 0
	for i, plays := range books {
		r.displayLoadNotes(plays)
		if i == 0 {
			for _, n := range r.CallbackNotes {
				r.displayVerbose(2, n)
			}
		}
		if i < len(r.BookPaths) {
			// A playbook file (not ad-hoc or Go API plays) announces itself.
			ForwardPlaybookStart(r.Callback, r.BookPaths[i])
			r.displayVerbose(2, fmt.Sprintf("%d plays in %s", len(plays), r.BookPaths[i]))
		}
		for _, play := range plays {
			if err := r.runPlay(ctx, play); err != nil {
				return 1, err
			}
			if r.fatalErr != nil {
				return 1, r.fatalErr
			}
			if r.quitRequested() {
				return r.quitCode, nil
			}
			if r.aborted {
				break
			}
		}
		r.Callback.Recap(r.stats, r.order)
		if code = r.result(); code != 0 {
			break
		}
	}
	return code, nil
}

// result is the TaskQueueManager's result for the last play run: a play
// ended by max_fail_percentage gives RUN_FAILED_HOSTS (2); otherwise
// RUN_UNREACHABLE_HOSTS (4) while any host is unreachable, then
// RUN_FAILED_HOSTS while any host is failed. Both carry over from play to
// play (a host that failed stays failed in later plays) until meta:
// clear_host_errors clears them, so the last play's result covers the
// earlier plays' failures too, but not the recap's cleared ones.
func (r *Runner) result() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.maxFailBroke:
		return 2
	case len(r.unreachable) > 0:
		return 4
	}
	for _, failed := range r.failed {
		if failed {
			return 2
		}
	}
	return 0
}

func (r *Runner) runPlay(ctx context.Context, play *playbook.Play) error {
	if len(play.Roles) > 0 {
		return fmt.Errorf("internal error: play %q has unresolved roles (playbook.ResolveRoles was not called)", play.Name)
	}
	if err := r.promptVars(play); err != nil {
		return err
	}
	if play.Dir != "" {
		// Several playbooks in one run: paths resolve from each play's
		// own playbook directory.
		r.Opts.BaseDir = play.Dir
	}
	r.mu.Lock()
	r.curPlay = play
	r.ended = map[string]bool{}
	r.playEnded = false
	r.mu.Unlock()
	r.Store.SetPlayVars(play.Vars)
	reserved := append([]template.KeyOrigin{}, play.VarOrigins...)
	for _, defaults := range play.RoleDefaults {
		r.Store.AddRoleDefaults(defaults)
	}
	for _, roleVars := range play.RoleVars {
		r.Store.AddRoleVars(roleVars)
	}

	// vars_files load relative to the playbook dir, templated with play vars.
	for _, vf := range play.VarsFiles {
		probe := r.Store.NewContext("", playPos(play))
		rendered, err := probe.TemplateString(vf)
		if err != nil {
			return fmt.Errorf("%s: templating vars_files entry %q: %w", play.Src.File, vf, err)
		}
		path, _ := rendered.(string)
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.Opts.BaseDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("could not load vars_files entry %q: %w", vf, err)
		}
		if r.Opts.Vault != nil {
			if data, err = r.Opts.Vault.MaybeDecryptFile(data); err != nil {
				return fmt.Errorf("vars_files %q: %w", vf, err)
			}
		}
		v, err := yaml.Unmarshal(data, path)
		if err != nil {
			return err
		}
		m, ok := yaml.PlainMap(v)
		if !ok && v != nil {
			return fmt.Errorf("%s: vars file must contain a mapping", path)
		}
		r.Store.AddVarsFile(m)
		if node, err := yaml.ParseSingle(data, path); err == nil {
			reserved = append(reserved, playbook.ReservedKeyOrigins(node, path)...)
		}
	}
	// The play's variables, before any host's, are checked for reserved
	// names (VariableManager.get_vars warns as it merges them).
	r.warnReserved(append(reserved, r.Opts.ExtraVarOrigins...))
	// ansible-core reads vars_files and resolves the play's hosts before
	// the banner: a file that fails to parse, or a pattern that is an
	// error, ends the run without one.
	allHosts, err := r.resolvePlayHosts(play)
	if err != nil {
		return err
	}
	r.Callback.PlayStart(play)
	if len(allHosts) == 0 {
		fmt.Println("skipping: no hosts matched")
		return nil
	}

	// serial: batches roll through the play; the whole play runs for each
	// batch before the next starts. A batch that breaches
	// max_fail_percentage aborts the play.
	batches := batchHosts(allHosts, play.Serial)
	for bi, batch := range batches {
		if len(batches) > 1 && bi > 0 {
			r.Callback.PlayStart(play) // Ansible reprints the banner per batch
		}
		r.batchEnded = false
		failedBefore := len(r.failedSet(batch))
		r.mu.Lock()
		r.batchFailed = map[string]bool{}
		for _, h := range batch {
			r.batchFailed[h] = r.failed[h]
		}
		r.mu.Unlock()
		err := r.runPlayBatch(ctx, play, batch)
		// A batch that breached max_fail_percentage ends the play with
		// every host failed: the linear strategy reports that no hosts
		// remain (once for the breach, once for the failed hosts), and
		// the play's RUN_FAILED_BREAK_PLAY stops the whole run.
		if err == errBatchAborted || (err == nil && r.batchBreached(batch, play.MaxFailPercentage)) {
			r.Callback.NoHostsRemaining()
			r.Callback.NoHostsRemaining()
			r.mu.Lock()
			r.aborted, r.maxFailBroke = true, true
			r.mu.Unlock()
			return nil
		}
		// When every host of a batch fails, ansible-playbook stops the
		// whole run: no further batches, plays or playbooks.
		if err == nil && len(batch) > 0 && len(r.failedSet(batch))-failedBefore == len(batch) {
			r.mu.Lock()
			r.aborted = true
			r.mu.Unlock()
			return nil
		}
		if err != nil {
			return err
		}
		if r.playEnded {
			break
		}
	}
	return nil
}

// runPlayBatch runs the full play (facts + sections + handler flushes) for
// one batch of hosts.
func (r *Runner) runPlayBatch(ctx context.Context, play *playbook.Play, playHosts []string) error {
	if freeStrategy(play) {
		inner := r.Callback
		r.Callback = &freeCallback{Callback: inner, names: map[*playbook.Task]freeBanner{}}
		r.freeSem = newTurnstile(r.Opts.Forks, r.activeOf(playHosts))
		defer func() { r.Callback, r.freeSem = inner, nil }()
	}
	r.resetNotified()
	r.stepContinue = false // --step's (c)ontinue lasts for one strategy run
	r.mu.Lock()
	r.blockFailed = map[string]map[int]bool{}
	if r.nextBlockID < 1<<20 {
		r.nextBlockID = 1 << 20 // above any parse-time block ID
	}
	r.failedIn = map[string]map[int]bool{}
	r.mu.Unlock()
	if play.GatherFacts == nil || *play.GatherFacts {
		gather := &playbook.Task{
			Name:    "Gathering Facts",
			Module:  "setup",
			Args:    play.GatherArgs,
			LoopVar: "item",
			Src:     play.Src,
		}
		r.runTaskAcrossHosts(ctx, play, gather, r.activeOf(playHosts), playHosts, false)
	}
	for _, section := range [][]*playbook.Task{play.PreTasks, play.Tasks, play.PostTasks} {
		if err := r.runSection(ctx, play, section, playHosts); err != nil {
			return err
		}
		if r.playEnded || r.batchEnded {
			return nil // ended plays/batches do not run pending handlers
		}
		if err := r.flushHandlers(ctx, play, playHosts); err != nil {
			return err
		}
	}
	return nil
}

// batchHosts splits hosts into serial batches. An empty spec yields one
// batch of all hosts. Numeric entries are host counts; "N%" entries are
// percentages (rounded down, minimum 1). The last size repeats until all
// hosts are consumed.
func batchHosts(hosts []string, serial []any) [][]string {
	if len(serial) == 0 {
		return [][]string{hosts}
	}
	total := len(hosts)
	sizeAt := func(i int) int {
		spec := serial[i]
		var n int
		switch t := spec.(type) {
		case string:
			if pct, ok := parsePercent(t, total); ok {
				n = pct
			}
		case int64:
			n = int(t)
		case int:
			n = t
		case float64:
			n = int(t)
		}
		if n < 1 {
			n = 1
		}
		return n
	}

	var batches [][]string
	pos := 0
	idx := 0
	for pos < total {
		size := sizeAt(min(idx, len(serial)-1))
		end := min(pos+size, total)
		batches = append(batches, hosts[pos:end])
		pos = end
		idx++
	}
	return batches
}

func parsePercent(s string, total int) (int, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "%") {
		return 0, false
	}
	var pct float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(s, "%"), "%f", &pct); err != nil {
		return 0, false
	}
	n := int(float64(total) * pct / 100.0)
	if n < 1 {
		n = 1
	}
	return n, true
}

// batchBreached reports whether the hosts that failed during this batch
// exceed max_fail_percentage (>=0) of it, as the linear strategy checks
// after each task: strictly greater, computed as ansible-core does, and
// counting failed hosts only (not unreachable ones, nor hosts that had
// already failed in an earlier play).
func (r *Runner) batchBreached(batch []string, maxFailPct float64) bool {
	if maxFailPct < 0 || len(batch) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := 0
	for _, h := range batch {
		if r.failed[h] && !r.unreachable[h] && !r.batchFailed[h] {
			failed++
		}
	}
	return float64(failed)/float64(len(batch)) > maxFailPct/100.0
}

const maxIncludeDepth = 100

// runTaskList executes a task sequence with the linear strategy. restrict
// (non-nil) limits execution to a host subset — used by include_tasks,
// whose file may differ per host.
func (r *Runner) runTaskList(ctx context.Context, play *playbook.Play, tasks []*playbook.Task, playHosts, restrict []string, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("include_tasks nesting exceeds %d levels (include loop?)", maxIncludeDepth)
	}
	listRestrict := restrict
	for i := 0; i < len(tasks); i++ {
		task := tasks[i]
		restrict := listRestrict
		if r.playEnded || r.batchEnded {
			return nil
		}
		if ref, level, ok := r.parallelBlock(task); ok {
			end := i
			for end < len(tasks) && hasBlockRef(tasks[end], ref.ID, level) {
				end++
			}
			if err := r.runParallel(ctx, play, tasks[i:end], level, ref.ID, playHosts, listRestrict, depth); err != nil {
				return err
			}
			i = end - 1
			continue
		}
		if !r.tagsMatch(task, play) {
			continue
		}
		if task.Role != nil && !task.Role.AllowDuplicates {
			// A role that already completed on a host does not run
			// there again (get_next_task_for_host skips its tasks).
			restrict = r.roleNotDone(task, playHosts, restrict)
			if len(restrict) == 0 {
				continue
			}
		}
		if task.Implicit && task.Module == "meta" && task.FreeForm == playbook.RoleCompleteAction {
			r.roleComplete(task, playHosts, restrict)
			continue
		}
		if r.Opts.StartAtTask != "" && !r.startedAt {
			if task.Module != "include_tasks" && task.Module != "include_role" && task.Module != "import_role" &&
				!r.startAt(r.taskDisplayName(task, playHosts)) {
				continue
			}
		}
		if task.Module == "meta" {
			if err := r.runMeta(ctx, play, task, playHosts, restrict); err != nil {
				return err
			}
			continue
		}
		// Block routing: skip hosts whose enclosing block state says no
		// (failed block sections skip; rescue runs only after failure).
		// Hard-failed hosts are re-admitted for the always sections of
		// blocks they failed inside — so no early break on empty active.
		active := r.activeOf(playHosts)
		active = append(active, r.alwaysEligible(playHosts, task)...)
		active = r.blockEligible(active, task)
		if restrict != nil {
			active = intersect(active, restrict)
		}
		if len(active) == 0 {
			continue
		}
		// --step asks about each task some host is about to run (meta
		// tasks are not asked about; an import_role is not a task).
		if r.Opts.Step && task.Module != "import_role" && !r.stepTask(task, false) {
			continue
		}
		if task.Module == "include_tasks" || task.Module == "include_role" {
			if err := r.runDynamicInclude(ctx, play, task, active, playHosts, depth); err != nil {
				return err
			}
			continue
		}
		if task.Module == "import_role" {
			if err := r.runImportRole(ctx, play, task, active, playHosts, depth); err != nil {
				return err
			}
			continue
		}
		before := r.failedSet(active)
		r.runTaskAcrossHosts(ctx, play, task, active, playHosts, false)

		// any_errors_fatal: a new hard failure ends the whole playbook once
		// this task has finished on every host.
		fatal := play.AnyErrorsFatal
		if task.AnyErrorsFatal != nil {
			fatal = *task.AnyErrorsFatal
		}
		if fatal && len(r.failedSet(active)) > len(before) {
			r.mu.Lock()
			r.aborted, r.playEnded = true, true
			r.mu.Unlock()
			return nil
		}

		// max_fail_percentage is evaluated after each task: too many failed
		// hosts in the batch aborts the play immediately.
		if r.batchBreached(playHosts, play.MaxFailPercentage) {
			return errBatchAborted
		}
	}
	return nil
}

// errBatchAborted signals that max_fail_percentage was breached; it is not
// a real error, just a stop signal handled in runPlay.
var errBatchAborted = fmt.Errorf("batch aborted (max_fail_percentage exceeded)")

func intersect(a, b []string) []string {
	keep := make(map[string]bool, len(b))
	for _, h := range b {
		keep[h] = true
	}
	var out []string
	for _, h := range a {
		if keep[h] {
			out = append(out, h)
		}
	}
	return out
}

// runImportRole implements import_role: the role's tasks splice in directly
// (no banner or recap entry for the import itself), and the import's
// when/tags/vars inherit onto each role task.
func (r *Runner) runImportRole(ctx context.Context, play *playbook.Play, task *playbook.Task, active, playHosts []string, depth int) error {
	if task.Loop != nil {
		return fmt.Errorf("%s:%d: import_role cannot be used with a loop (use include_role)", task.Src.File, task.Src.Line)
	}
	name, _ := task.Args["name"].(string)
	if name == "" {
		return fmt.Errorf("%s:%d: import_role requires a name", task.Src.File, task.Src.Line)
	}
	tasks, _, err := r.loadIncludedRole(play, task, name)
	if err != nil {
		for _, host := range active {
			r.record(host, task, agentproto.Fail("%s: %v", task.Module, err), nil)
		}
		return nil
	}
	r.adoptBlocks(task, tasks)
	for _, t := range tasks {
		// A static import is its tasks' parent: every inheritable
		// keyword on it (and on its enclosing blocks) applies.
		playbook.Inherit(t, task)
	}
	return r.runTaskList(ctx, play, tasks, playHosts, active, depth+1)
}

// resolveIncludePath locates an included task file: absolute, relative to
// the including file, the role's tasks dir, or the playbook dir.
func (r *Runner) resolveIncludePath(path string, task *playbook.Task) (string, error) {
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("include_tasks: %s not found", path)
	}
	var candidates []string
	if task.Src.File != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(task.Src.File), path))
	}
	if task.SrcDir != "" {
		candidates = append(candidates,
			filepath.Join(task.SrcDir, "tasks", path),
			filepath.Join(task.SrcDir, path))
	}
	candidates = append(candidates, filepath.Join(r.Opts.BaseDir, path))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("include_tasks: could not find %q (searched near %s)", path, task.Src.File)
}

// taskDisplayName renders the task name for the banner. Ansible templates
// task names once at task-start; it uses the first host's vars (loop item is
// not bound yet). Non-templated names take the cheap path; a templating error
// (e.g. an undefined var) falls back to the raw name rather than aborting.
func (r *Runner) taskDisplayName(task *playbook.Task, active []string) string {
	name := task.Name
	if name != "" && len(active) > 0 && (strings.Contains(name, "{{") || strings.Contains(name, "{%")) {
		ctx := r.Store.NewContext(active[0], template.Position{File: task.Src.File, Line: task.Src.Line})
		if rendered, err := ctx.TemplateString(name); err == nil {
			name = fmt.Sprintf("%v", rendered)
		}
	}
	// Role tasks show as "role : task" (Ansible banner form), falling back
	// to the action for unnamed tasks.
	if task.RoleName != "" {
		if name == "" {
			name = task.DisplayAction()
		}
		return task.RoleName + " : " + name
	}
	return name
}

// runTaskAcrossHosts executes one task on all active hosts with forks
// parallelism (one errgroup per task = the linear-strategy barrier).
func (r *Runner) runTaskAcrossHosts(ctx context.Context, play *playbook.Play, task *playbook.Task, active, playHosts []string, handler bool) {
	if note := playbook.ActionRedirect(task.Action); note != "" && len(active) > 0 {
		// The strategy resolves the action (for bypass_host_loop) before
		// the banner.
		r.displayVerbose(2, note)
	}
	r.taskStart(task, r.taskDisplayName(task, active), handler)
	if task.RunOnce && len(active) > 0 {
		// run_once: the first host runs; register/facts/notify fan out to
		// every host of the batch.
		r.mu.Lock()
		r.runOnceHosts = append([]string(nil), active...)
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			r.runOnceHosts = nil
			r.mu.Unlock()
		}()
		active = active[:1]
	}
	g, gctx := errgroup.WithContext(ctx)
	if r.freeSem == nil {
		g.SetLimit(r.Opts.Forks)
	}
	for _, host := range active {
		g.Go(func() error {
			if r.freeSem != nil {
				r.freeSem.acquire(host)
				defer r.freeSem.release()
			}
			r.mu.Lock()
			stopped := r.fatalErr != nil
			r.mu.Unlock()
			if stopped {
				return nil // a result raised: no host starts after it
			}
			r.runTaskOnHost(gctx, play, task, host, playHosts)
			return nil
		})
	}
	g.Wait()
}

// tagsMatch applies --tags/--skip-tags with the special always/never tags.
func (r *Runner) tagsMatch(task *playbook.Task, play *playbook.Play) bool {
	tags := append(append([]string{}, play.Tags...), task.Tags...)
	has := func(t string) bool {
		for _, x := range tags {
			if x == t {
				return true
			}
		}
		return false
	}
	for _, skip := range r.Opts.SkipTags {
		if has(skip) {
			return false
		}
	}
	if has("never") && !r.wantsTag(tags) {
		return false
	}
	if len(r.Opts.Tags) == 0 {
		return true
	}
	if has("always") {
		return true
	}
	return r.wantsTag(tags)
}

func (r *Runner) wantsTag(tags []string) bool {
	for _, want := range r.Opts.Tags {
		for _, t := range tags {
			if t == want {
				return true
			}
		}
	}
	return false
}

// blockEligible filters hosts by their block state for this task.
func (r *Runner) blockEligible(hosts []string, task *playbook.Task) []string {
	if len(task.Blocks) == 0 {
		return hosts
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, host := range hosts {
		hb := r.blockFailed[host]
		ok := true
		for _, ref := range task.Blocks {
			switch ref.Section {
			case playbook.SectionBlock:
				if hb[ref.ID] {
					ok = false
				}
			case playbook.SectionRescue:
				if !hb[ref.ID] {
					ok = false
				}
			}
			// SectionAlways runs regardless of the block's failure state.
		}
		if ok {
			out = append(out, host)
		}
	}
	return out
}

// alwaysEligible returns failed hosts that must still run this task because
// it is in the always section of a block they failed inside.
func (r *Runner) alwaysEligible(playHosts []string, task *playbook.Task) []string {
	inAlways := false
	for _, ref := range task.Blocks {
		if ref.Section == playbook.SectionAlways {
			inAlways = true
			break
		}
	}
	if !inAlways {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, host := range playHosts {
		if !r.failed[host] {
			continue
		}
		for _, ref := range task.Blocks {
			if ref.Section == playbook.SectionAlways && r.failedIn[host][ref.ID] {
				out = append(out, host)
				break
			}
		}
	}
	return out
}

// catchInRescue routes a failure to the nearest enclosing block that has a
// rescue section and hasn't already failed. Returns true when caught.
func (r *Runner) catchInRescue(host string, task *playbook.Task) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(task.Blocks) - 1; i >= 0; i-- {
		ref := task.Blocks[i]
		// Only a failure in a block's main section is catchable by that
		// block; rescue/always failures propagate outward.
		if ref.Section != playbook.SectionBlock || !ref.HasRescue {
			continue
		}
		if r.blockFailed[host] == nil {
			r.blockFailed[host] = map[int]bool{}
		}
		if r.blockFailed[host][ref.ID] {
			continue // this block already failed once; propagate outward
		}
		r.blockFailed[host][ref.ID] = true
		return true
	}
	return false
}

// resetNotified clears handler notification state (per play).
func (r *Runner) resetNotified() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notified = map[*playbook.Task]map[string]bool{}
	r.notifyOrder = map[string][]string{}
	r.handlerNames = nil
	// The play's role cache: what ran and completed where.
	r.roleRan = map[string]map[string]bool{}
	r.roleDone = map[string]map[string]bool{}
}

// notifyHandlers saves one host's notifications (called on change): each
// of lists is one result's notify list (a loop's items that ran each
// carry theirs), announced ("Notification for handler ... has been
// saved." at -vv) when announce is set (not for a run_once fan-out).
// During a flush, a handler that notifies others queues them at once
// (v2_playbook_on_notify). A notification no handler answers, by name or
// listen topic, ends the run with ansible-core's error; it reports false.
func (r *Runner) notifyHandlers(host string, lists [][]string, announce bool) bool {
	r.mu.Lock()
	inHandlers := r.inHandlers
	r.mu.Unlock()
	for _, names := range lists {
		for _, name := range names {
			matches := r.searchHandlers(name)
			if len(matches) == 0 {
				r.fatal(fmt.Errorf("The requested handler '%s' was not found in either the main handlers list nor in the listening handlers list", name))
				return false
			}
			if inHandlers {
				for _, h := range matches {
					if r.notifyHost(h, host) {
						r.forwardNotified(h, host)
					}
				}
				continue
			}
			r.mu.Lock()
			if !slices.Contains(r.notifyOrder[host], name) {
				r.notifyOrder[host] = append(r.notifyOrder[host], name)
			}
			r.mu.Unlock()
			if announce {
				r.displayVerbose(2, fmt.Sprintf("Notification for handler %s has been saved.", name))
			}
		}
	}
	return true
}

// forwardNotified is v2_playbook_on_notify, naming the handler as its
// name was templated.
func (r *Runner) forwardNotified(h *playbook.Task, host string) {
	if name, ok := r.handlerName(h); ok && name != h.Name {
		c := *h
		c.Name = name
		h = &c
	}
	ForwardHandlerNotified(r.Callback, h, host)
}

// notifyHost marks handler h to run on host, reporting whether that is
// new (Handler.notify_host).
func (r *Runner) notifyHost(h *playbook.Task, host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notified[h] == nil {
		r.notified[h] = map[string]bool{}
	}
	if r.notified[h][host] {
		return false
	}
	r.notified[h][host] = true
	return true
}

// fatal ends the run at once with err (an AnsibleError raised while
// processing results): no recap, exit 1.
func (r *Runner) fatal(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fatalErr == nil {
		r.fatalErr = err
	}
	r.aborted, r.playEnded = true, true
}

// searchHandlers is search_handlers_by_notification: the play's handlers
// are searched block by block from the last loaded one; a handler whose
// (templated) name, or role-qualified name, is the notification answers
// alone, else every handler listening to it does (one per name).
func (r *Runner) searchHandlers(notification string) []*playbook.Task {
	r.mu.Lock()
	play := r.curPlay
	var handlers []*playbook.Task
	if play != nil {
		handlers = handlerSearchOrder(play.Handlers)
	}
	r.mu.Unlock()
	var listening []*playbook.Task
	seen := map[string]bool{}
	for _, h := range handlers {
		name, ok := r.handlerName(h)
		if !ok {
			continue
		}
		if name != "" && (notification == name || (h.RoleName != "" && notification == h.RoleName+" : "+name)) {
			return []*playbook.Task{h}
		}
	}
	for _, h := range handlers {
		if !slices.Contains(h.Listen, notification) {
			continue
		}
		name, _ := r.handlerName(h)
		if name != "" && seen[name] {
			continue
		}
		seen[name] = true
		listening = append(listening, h)
	}
	return listening
}

// handlerSearchOrder lists handlers block by block, the last block first
// (the last handler loaded with a name wins): consecutive handlers from
// one file and top-level block form a block.
func handlerSearchOrder(handlers []*playbook.Task) []*playbook.Task {
	type group struct{ list []*playbook.Task }
	var groups []*group
	key := func(h *playbook.Task) string {
		k := h.Src.File
		if len(h.Blocks) > 0 {
			k += fmt.Sprintf("#%d", h.Blocks[0].ID)
		}
		return k
	}
	last := ""
	for _, h := range handlers {
		if k := key(h); len(groups) == 0 || k != last {
			groups = append(groups, &group{})
			last = k
		}
		g := groups[len(groups)-1]
		g.list = append(g.list, h)
	}
	var out []*playbook.Task
	for i := len(groups) - 1; i >= 0; i-- {
		out = append(out, groups[i].list...)
	}
	return out
}

// handlerName is a handler's name, templated once with the play's
// variables; false when it cannot be (the handler is then unusable by
// name, as ansible-core warns when it has no listen topics either).
func (r *Runner) handlerName(h *playbook.Task) (string, bool) {
	if !strings.Contains(h.Name, "{{") && !strings.Contains(h.Name, "{%") {
		return h.Name, true
	}
	r.mu.Lock()
	if r.handlerNames == nil {
		r.handlerNames = map[*playbook.Task]*string{}
	}
	cached, ok := r.handlerNames[h]
	r.mu.Unlock()
	if ok {
		if cached == nil {
			return "", false
		}
		return *cached, true
	}
	ctx := r.Store.NewContext("", template.Position{File: h.Src.File, Line: h.Src.Line, Col: h.Src.Col})
	v, err := ctx.TemplateString(h.Name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.handlerNames[h] = nil
		if len(h.Listen) == 0 {
			msg := err.Error()
			if cause, ok := template.Cause(err); ok {
				msg = cause
			}
			r.mu.Unlock()
			r.warn(fmt.Sprintf("Handler '%s' is unusable because it has no listen topics and the name could not be templated "+
				"(host-specific variables are not supported in handler names). The error: %s", h.Name, msg))
			r.mu.Lock()
		}
		return "", false
	}
	name := fmt.Sprint(v)
	r.handlerNames[h] = &name
	return name, true
}

// announceNotified is flush_handlers expanding a host's saved
// notifications into the handlers they name: a NOTIFIED HANDLER line for
// each newly notified.
func (r *Runner) announceNotified(host string) {
	r.mu.Lock()
	names := r.notifyOrder[host]
	delete(r.notifyOrder, host)
	r.mu.Unlock()
	for _, name := range names {
		for _, h := range r.searchHandlers(name) {
			if r.notifyHost(h, host) {
				r.forwardNotified(h, host)
			}
		}
	}
}

// flushHandlers runs notified handlers in definition order across the hosts
// that notified them, clearing the notification set.
func (r *Runner) flushHandlers(ctx context.Context, play *playbook.Play, playHosts []string) error {
	r.mu.Lock()
	handlers := append([]*playbook.Task(nil), play.Handlers...)
	r.mu.Unlock()
	announce := r.activeOf(playHosts)
	if play.ForceHandlers || r.Opts.ForceHandlers {
		announce = r.notEnded(playHosts, nil)
	}
	for _, h := range announce {
		r.announceNotified(h)
	}
	r.mu.Lock()
	r.inHandlers = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inHandlers = false
		r.mu.Unlock()
	}()
	for _, handler := range handlers {
		if r.playEnded {
			return nil
		}
		r.mu.Lock()
		hosts := r.notified[handler]
		delete(r.notified, handler)
		r.mu.Unlock()
		if len(hosts) == 0 {
			continue
		}
		candidates := r.activeOf(playHosts)
		if play.ForceHandlers || r.Opts.ForceHandlers {
			// force_handlers: hosts that failed after being notified still
			// run their handlers (but not ended/unreachable ones).
			candidates = r.notEnded(playHosts, nil)
		}
		var active []string
		for _, h := range candidates {
			if hosts[h] && !r.isUnreachable(h) {
				active = append(active, h)
			}
		}
		if len(active) == 0 {
			continue
		}
		if r.Opts.Step && !r.stepTask(handler, true) {
			// A handler skipped at the step prompt did not run: its hosts
			// stay notified for the next flush.
			r.mu.Lock()
			r.notified[handler] = hosts
			r.mu.Unlock()
			continue
		}
		r.runTaskAcrossHosts(ctx, play, handler, active, playHosts, true)
	}
	return nil
}

// failedSet lists which of hosts have failed hard so far.
func (r *Runner) failedSet(hosts []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range hosts {
		if r.failed[h] {
			out = append(out, h)
		}
	}
	return out
}

// activeOf filters a play's host list down to hosts that have not failed.
func (r *Runner) activeOf(playHosts []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range playHosts {
		if !r.failed[h] && !r.ended[h] {
			out = append(out, h)
		}
	}
	return out
}

// newHostContext builds a vars context with the play-level magic variables.
func (r *Runner) newHostContext(host string, pos template.Position, playHosts []string) *vars.Context {
	c := r.Store.NewContext(host, pos)
	if r.Inv != nil {
		c.SetMagic("groups", r.Inv.GroupsMap())
		if h := r.Inv.GetHost(host); h != nil {
			r.registerImplicit(h)
			names := r.Inv.GroupNames(h)
			list := make([]any, len(names))
			for i, n := range names {
				list[i] = n
			}
			c.SetMagic("group_names", list)
		}
	}
	if playHosts != nil {
		list := make([]any, len(playHosts))
		for i, h := range playHosts {
			list[i] = h
		}
		c.SetMagic("ansible_play_hosts", list)
		c.SetMagic("play_hosts", deprecate(deprecatedPlayHosts, list))
		c.SetMagic("ansible_play_hosts_all", list)
	}
	c.SetMagic("playbook_dir", r.Opts.BaseDir)
	c.SetMagic("ansible_check_mode", r.Opts.CheckMode)
	r.setRunMagic(c, host, playHosts)
	// ansible_connection reflects the connection in effect when the host
	// does not set it: play keyword, then -c, then the default.
	if _, ok := r.Store.RawHostVar(host, "ansible_connection"); !ok {
		conn := ""
		if r.curPlay != nil {
			conn = r.curPlay.Connection
		}
		if conn == "" && r.Conns != nil {
			conn = r.Conns.Opts.Connection
		}
		if conn == "" || conn == "smart" {
			conn = "ssh"
			if (host == "localhost" || host == "127.0.0.1") && r.Inv != nil && r.Inv.Hosts[host] == nil {
				conn = "local"
			}
		}
		c.SetMagic("ansible_connection", conn)
	}
	// hostvars: a lazy mapping from any inventory host to that host's
	// resolved variable view. Building another host's context is deferred
	// until hostvars['other'] is actually accessed.
	if r.Inv != nil {
		c.SetMagic("hostvars", newHostVars(r, pos, playHosts))
	}
	return c
}

// hostVars is the lazy `hostvars` magic variable.
type hostVars struct {
	r         *Runner
	pos       template.Position
	playHosts []string
	names     []string
}

func newHostVars(r *Runner, pos template.Position, playHosts []string) *hostVars {
	return &hostVars{r: r, pos: pos, playHosts: playHosts, names: r.Inv.HostNames()}
}

func (h *hostVars) GetItem(host string) (any, bool) {
	// A localhost name resolves to the implicit localhost, as
	// InventoryData.get_host does.
	if h.r.Inv.GetHost(host) == nil {
		return nil, false
	}
	// Build the target host's context on demand and expose it as a mapping.
	return hostVarsVars{h.r.newHostContext(host, h.pos, h.playHosts).AsMapping()}, true
}

// hostVarsVars is one host's variables through hostvars: without
// hostvars itself (get_vars(include_hostvars=False)), so walking every
// key terminates.
type hostVarsVars struct{ template.Mapping }

func (v hostVarsVars) Keys() []string {
	keys := v.Mapping.Keys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "hostvars" {
			out = append(out, k)
		}
	}
	return out
}

func (v hostVarsVars) Len() int { return len(v.Keys()) }

// PyTypeName is the class messages name it by.
func (hostVarsVars) PyTypeName() string { return "HostVarsVars" }

// PyTypeName is the class messages name it by.
func (*hostVars) PyTypeName() string { return "HostVars" }

// MissingItem is the message of a host hostvars does not have: its
// expression.
func (*hostVars) MissingItem(host string) string {
	return "hostvars[" + template.PyRepr(host) + "]"
}

func (h *hostVars) Keys() []string { return h.names }
func (h *hostVars) Len() int       { return len(h.names) }

func playPos(play *playbook.Play) template.Position {
	return template.Position{File: play.Src.File, Line: play.Src.Line, Col: play.Src.Col}
}

// runTaskOnHost is the per-host task pipeline: when -> loop -> template args
// -> retries -> changed_when/failed_when -> register -> stats.
func (r *Runner) runTaskOnHost(ctx context.Context, play *playbook.Play, task *playbook.Task, host string, playHosts []string) {
	var override map[string]any
	for {
		if r.quitRequested() {
			return
		}
		res, items, resolved := r.execTaskOnHost(ctx, play, task, host, playHosts, override)
		if !r.needsDebugger(play, resolved, res) {
			r.record(host, resolved, res, items)
			return
		}
		// The result is displayed and counted first, then the debugger
		// runs; redo rolls the host's state back.
		snap := r.snapshotHost(host)
		r.record(host, task, res, items)
		copied := *task
		copied.Args = maps.Clone(task.Args)
		if override == nil {
			override = map[string]any{}
		}
		pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
		vctx := r.newHostContext(host, pos, playHosts).WithRoleScope(task.ScopeDefaults, task.ScopeVars)
		if len(task.Vars) > 0 {
			vctx = vctx.WithOverlay(task.Vars)
		}
		s := &debugSession{r: r, task: &copied, host: host, vctx: vctx.WithOverlay(override), res: res, play: play, override: override}
		switch r.runDebugger(s) {
		case debugContinue:
			return
		case debugQuit:
			r.requestQuit()
			return
		case debugRedo:
			r.restoreHost(host, snap, res)
			task = s.task
			if raw, ok := task.Args["_raw_params"]; ok {
				// task.args['_raw_params'] is the free-form command.
				task.FreeForm = template.PyStr(raw)
				delete(task.Args, "_raw_params")
			}
		}
	}
}

// execTaskOnHost runs one task (all loop items) on a host and returns the
// result to record, with the per-item results for loops, and the task with
// its templated keywords resolved for this host.
func (r *Runner) execTaskOnHost(ctx context.Context, play *playbook.Play, task *playbook.Task, host string, playHosts []string, override map[string]any) (*agentproto.Result, []any, *playbook.Task) {
	// One TaskExecutor (and connection) per task and host, loop items
	// included.
	ctx = context.WithValue(ctx, connectedKey{}, new(string))
	// Discovery updates the task's variables for its later loop items.
	ctx = context.WithValue(ctx, discoveredCtxKey{}, new(string))
	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	r.warnReservedFor(host, task)
	base := r.newHostContext(host, pos, playHosts).WithRoleScope(task.ScopeDefaults, task.ScopeVars)
	if len(task.Vars) > 0 {
		base = base.WithOverlay(task.Vars)
	}
	if len(override) > 0 {
		base = base.WithOverlay(override)
	}
	// ansible_search_path: the role (if any) then the task's directory;
	// file lookups search it (DataLoader.path_dwim_relative_stack).
	var search []any
	if task.SrcDir != "" {
		search = append(search, task.SrcDir)
	}
	if d := taskDir(task); d != "" && d != task.SrcDir {
		search = append(search, d)
	}
	base.SetMagic("ansible_search_path", search)
	base.SetMagic(taskActionVar, task.DisplayAction())
	resolved, err := resolveKeywords(task, base)
	if err != nil {
		return agentproto.Fail("%v", err), nil, task
	}
	task = resolved

	// Resolve the loop (nil = run once with no loop var).
	items, isLoop, err := r.resolveLoop(task, base)
	if err != nil {
		return loopFailure(err), nil, task
	}

	if !isLoop {
		return r.runOnce(ctx, play, task, host, base, nil), nil, task
	}

	lc, err := newLoopControl(task, base, items)
	if err != nil {
		return loopControlFailure(err), nil, task
	}
	r.checkLoopControl(task, base)

	// Loop: aggregate per-item results Ansible-style.
	var itemResults []any
	var notify [][]string
	anyChanged, anyFailed, allSkipped := false, false, true
	for i, item := range items {
		if i > 0 && lc.pause > 0 {
			time.Sleep(lc.pause)
		}
		// Rebuild the per-item context from the store each iteration so a
		// set_fact from an earlier item is visible to later ones (Ansible's
		// accumulate-in-a-loop pattern).
		itemCtx := r.newHostContext(host, pos, playHosts).WithRoleScope(task.ScopeDefaults, task.ScopeVars)
		if len(task.Vars) > 0 {
			itemCtx = itemCtx.WithOverlay(task.Vars)
		}
		if len(override) > 0 {
			itemCtx = itemCtx.WithOverlay(override)
		}
		itemCtx = itemCtx.WithOverlay(lc.vars(i))
		res := r.runOnce(ctx, play, task, host, itemCtx, item)
		stop := r.breakWhen(task, itemCtx, res)
		if !res.Failed {
			notify = append(notify, res.Notify...)
		}
		// Per-item results carry the loop variable(s), as Ansible's do.
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		lc.annotate(res.Extra, i)
		r.Callback.HostResult(host, task, shown(task, res), false, lc.label(itemCtx, i))
		m := orderedResult(task, task.Module, res.ToVars())
		itemResults = append(itemResults, m)
		anyChanged = anyChanged || res.Changed
		anyFailed = anyFailed || res.Failed
		allSkipped = allSkipped && res.Skipped
		if stop {
			break
		}
	}
	if itemResults == nil {
		itemResults = []any{}
	}
	agg := loopResult(itemResults, anyChanged, anyFailed, allSkipped)
	agg.Notify = notify
	return agg, itemResults, task
}

// loopResult is a loop's aggregate result (build_loop_result): the
// per-item results under "results".
func loopResult(itemResults []any, changed, failed, allSkipped bool) *agentproto.Result {
	agg := &agentproto.Result{Changed: changed, Failed: failed, Skipped: allSkipped,
		Extra: map[string]any{"results": itemResults}}
	switch {
	case len(itemResults) == 0:
		agg.Extra["skip_reason"] = "No items in the list"
		agg.Extra["skipped_reason"] = deprecate(deprecatedSkippedReason, "No items in the list")
	case failed:
		agg.Msg = "One or more items failed"
	case allSkipped:
		agg.Msg = "All items skipped"
	default:
		agg.Msg = "All items completed"
	}
	return agg
}

// resolveLoop templates the loop value. Returns isLoop=false when absent.
// with_<lookup> loops feed the templated terms through the lookup plugin.
func (r *Runner) resolveLoop(task *playbook.Task, vctx *vars.Context) ([]any, bool, error) {
	if task.Loop == nil {
		return nil, false, nil
	}
	// Items keep deprecated values deprecated: reading them warns again.
	pos, ok := task.KeywordPos["loop"]
	if !ok {
		if pos, ok = task.KeywordPos["with_"+task.LoopWith]; !ok {
			pos = task.KeywordPos["with_list"]
		}
	}
	v, err := vctx.At(pos).KeepingDeprecated().TemplateValue(task.Loop)
	if err != nil {
		return nil, false, &loopTemplateError{pos: pos, err: err}
	}
	v = template.Undeprecate(v) // the list itself (play_hosts)
	if task.LoopWith != "" {
		if r.Engine.Lookup == nil {
			return nil, false, fmt.Errorf("with_%s: lookups are not available", task.LoopWith)
		}
		terms, ok := v.([]any)
		if !ok {
			terms = []any{v}
		}
		out, err := r.Engine.Lookup(r.Engine.NewEvalCtx(vctx, template.Position{File: task.Src.File, Line: task.Src.Line}), task.LoopWith, terms, nil)
		if err != nil {
			return nil, false, err
		}
		if list, ok := out.([]any); ok {
			return list, true, nil
		}
		return []any{out}, true, nil
	}
	switch t := v.(type) {
	case []any:
		return t, true, nil
	case string:
		return nil, false, fmt.Errorf("loop value must be a list, got a string (%q)", t)
	default:
		return nil, false, fmt.Errorf("loop value must be a list, got %T", v)
	}
}

// runOnce executes one occurrence (one loop item or the whole task).
func (r *Runner) runOnce(ctx context.Context, play *playbook.Play, task *playbook.Task, host string, vctx *vars.Context, item any) *agentproto.Result {
	// when: gate.
	if skip, err := whenSkip(vctx, task.When, task.WhenPos); err != nil {
		return err.result()
	} else if skip != nil {
		return skip
	}

	// Template module args, dropping omitted ones. Each argument's
	// templates report its own origin; set_fact's copies stay deprecated
	// where their source was.
	argsCtx := vctx
	if task.Module == "set_fact" {
		argsCtx = vctx.KeepingDeprecated()
	}
	args, failed := r.templatedArgs(task, vctx)
	if failed != nil {
		return failed
	}
	if args == nil {
		args = make(map[string]any, len(task.Args))
	}
	for _, k := range slices.Sorted(maps.Keys(task.Args)) { // ansible-core templates them in key order
		raw := task.Args[k]
		if k == "that" && isAssertModule(task.Module) {
			// assert's finalize_task_arg: 'that' stays raw (each entry is
			// a conditional), except that a string that is entirely a
			// template may resolve to a list of conditionals.
			args[k] = assertThat(vctx.At(argPos(task, k)), raw)
			continue
		}
		v, err := argsCtx.At(argPos(task, k)).Sourced().TemplateValue(raw)
		if err != nil {
			return argTemplateError(task, k, argPos(task, k), err)
		}
		if _, isOmit := v.(template.Omit); isOmit {
			delete(args, k)
			continue
		}
		args[k] = v
	}
	if task.Module == "set_fact" {
		if bad := invalidSetFactName(task, args); bad != nil {
			return bad
		}
	}
	freeForm := task.FreeForm
	if freeForm != "" {
		v, err := vctx.At(task.ArgsPos).TemplateString(freeForm)
		if err != nil {
			return argTemplateError(task, "_raw_params", task.ArgsPos, err)
		}
		freeForm = ""
		if v != nil { // a template with no output is None
			freeForm = fmt.Sprintf("%v", v)
		}
	}

	actx, target, err := r.actionContext(ctx, host, task, play, vctx)
	delegated := ""
	if target != host {
		delegated = target
	}
	if err != nil {
		if ue, ok := err.(*unreachableError); ok {
			return &agentproto.Result{Failed: true, Msg: ue.Error(), DelegatedTo: delegated,
				Extra: map[string]any{"unreachable": true}}
		}
		res := agentproto.Fail("%v", err)
		res.DelegatedTo = delegated
		return res
	}

	// until/retries loop, as TaskExecutor._execute: 1 + retries attempts
	// (retries defaults to 3 when only until is set); a task with retries
	// but no until retries until it stops failing. changed_when and
	// failed_when apply to every attempt, before until is evaluated.
	total := 1
	if task.RetriesSet {
		total += max(0, task.Retries)
	} else if task.Until != "" {
		total += 3
	}
	timeout, bad := r.taskTimeout(play, task, vctx)
	if bad != nil {
		bad.DelegatedTo = delegated
		return bad
	}
	var res *agentproto.Result
	retriesExhausted := false
	for attempt := 1; attempt <= total; attempt++ {
		var timedOut bool
		if res, timedOut = r.dispatchTimed(ctx, task, actx, args, freeForm, timeout); timedOut {
			res.DelegatedTo = delegated
			return res
		}
		if task.Async > 0 && task.Poll != 0 && !res.Failed && res.Extra["ansible_job_id"] != nil {
			res = r.pollAsync(ctx, host, task, actx, res)
		}
		if total > 1 {
			setExtra(res, "attempts", attempt)
		}
		if !res.Skipped {
			if fail := applyChangedFailedWhen(task, vctx, res); fail != nil {
				return fail
			}
		}
		if total == 1 {
			break
		}
		done := !res.Failed
		if task.Until != "" {
			pos := conditionalPos(task, "until", task.Until)
			ok, err := vctx.WithOverlay(registerOverlay(task, res)).At(pos).EvalWhen([]string{task.Until})
			if err != nil {
				return (&conditionalError{keyword: "until", pos: pos, err: err}).result()
			}
			done = ok
		}
		if done {
			break
		}
		if attempt < total {
			setExtra(res, "retries", total)
			setExtra(res, "attempts", attempt+1)
			name := r.taskDisplayName(task, []string{host})
			if name == "" {
				name = task.DisplayAction()
			}
			retried := shown(task, res)
			if task.Loop != nil || task.LoopWith != "" {
				// A loop item's attempt carries its loop variables.
				copied := *retried
				copied.Extra = maps.Clone(retried.Extra)
				if copied.Extra == nil {
					copied.Extra = map[string]any{}
				}
				copied.Extra["ansible_loop_var"] = task.LoopVar
				copied.Extra[task.LoopVar] = item
				if task.IndexVar != "" {
					copied.Extra["ansible_index_var"] = task.IndexVar
					if v, ok := vctx.Get(task.IndexVar); ok {
						copied.Extra[task.IndexVar] = v
					}
				}
				retried = &copied
			}
			r.Callback.Retrying(host, task, name, total-(attempt+1), retried)
			time.Sleep(time.Duration(task.Delay) * time.Second)
		} else {
			// Out of attempts: ansible-core records retries-1 attempts and
			// marks the result failed, even one the module reported ok.
			retriesExhausted = !res.Failed
			res.Failed = true
			setExtra(res, "attempts", total-1)
			if retriesExhausted {
				res.Origin = "plain" // no exception, so no error block
			}
		}
	}
	if res.Failed && !retriesExhausted && res.Origin != "plain" {
		// ansible-core attaches an ErrorSummary to every failed task
		// result; templated (register, ansible_failed_result) it renders as
		// this placeholder unless tracebacks are enabled. The callback
		// strips it from the fatal line.
		if _, has := res.Extra["exception"]; !has {
			setExtra(res, "exception", "(traceback unavailable)")
		}
	}
	res.DelegatedTo = delegated
	res.ShowDiff = r.effectiveDiff(play, task)
	if len(task.Notify) > 0 && !res.Failed && !res.Skipped {
		res.Notify = [][]string{templateNotify(task, vctx)}
	}
	return res
}

// templateNotify is the task's notify list templated for one run (a loop
// item's notify may name its item).
func templateNotify(task *playbook.Task, vctx *vars.Context) []string {
	out := make([]string, 0, len(task.Notify))
	for _, n := range task.Notify {
		if strings.Contains(n, "{{") || strings.Contains(n, "{%") {
			if v, err := vctx.TemplateString(n); err == nil {
				n = fmt.Sprint(v)
			}
		}
		out = append(out, n)
	}
	return out
}

// applyChangedFailedWhen lets changed_when / failed_when override the
// module's own verdict; a non-nil return is an evaluation error result.
func applyChangedFailedWhen(task *playbook.Task, vctx *vars.Context, res *agentproto.Result) *agentproto.Result {
	if len(task.ChangedWhen) > 0 || len(task.FailedWhen) > 0 {
		resCtx := vctx.WithOverlay(registerOverlay(task, res))
		if len(task.ChangedWhen) > 0 {
			ok, err := evalConditionals(task, resCtx, "changed_when", task.ChangedWhen)
			if err != nil {
				return err.actionResult(res)
			}
			res.Changed = ok
			setExtra(res, "changed_when_result", ok)
		}
		if len(task.FailedWhen) > 0 {
			ok, err := evalConditionals(task, resCtx, "failed_when", task.FailedWhen)
			if err != nil {
				return err.actionResult(res)
			}
			if res.Failed && !ok {
				// ansible-core 2.19+ records that a failure was overridden.
				setExtra(res, "failed_when_suppressed_exception", "(traceback unavailable)")
			}
			res.Failed = ok
			setExtra(res, "failed_when_result", ok)
			if ok {
				// ansible-core's wording; the callback shows it verbatim.
				res.Msg = "Task failed: Action failed: A 'failed_when' expression evaluated to 'True'."
				res.Origin = "verbatim"
			}
		}
	}
	return nil
}

// registerOverlay exposes the in-flight result under the register name (and
// 'result' when unregistered) for until/failed_when/changed_when.
func registerOverlay(task *playbook.Task, res *agentproto.Result) map[string]any {
	m := orderedResult(task, task.Module, res.ToVars())
	name := task.Register
	if name == "" {
		name = "result"
	}
	return map[string]any{name: m}
}

// remoteTmp is the shell plugin's remote_tmp option: the ansible_remote_tmp
// variable, else ansible.cfg/ANSIBLE_REMOTE_TMP, else Ansible's default.
func (r *Runner) remoteTmp(vctx *vars.Context) string {
	if v, ok := vctx.Get("ansible_remote_tmp"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if r.Conns != nil && r.Conns.Opts.RemoteTmp != "" {
		return r.Conns.Opts.RemoteTmp
	}
	return "~/.ansible/tmp"
}

// actionContext builds the execution context for one occurrence of a task
// on host. With delegate_to, the task keeps host's variables but runs over
// the delegate's connection (its own connection vars); the returned target
// names where it ran.
func (r *Runner) actionContext(ctx context.Context, host string, task *playbook.Task, play *playbook.Play, vctx *vars.Context) (*actions.Context, string, error) {
	become, err := r.effectiveBecome(play, task, vctx)
	if err != nil {
		return nil, host, err
	}
	kw := connection.Keywords{
		Connection: firstNonEmpty(task.Connection, play.Connection),
		RemoteUser: firstNonEmpty(task.RemoteUser, play.RemoteUser),
	}
	target := host
	if task.Delegate != "" {
		v, err := vctx.TemplateString(task.Delegate)
		if err != nil {
			return nil, host, fmt.Errorf("error templating delegate_to: %v", err)
		}
		target = fmt.Sprintf("%v", v)
	}
	var conn connection.Connection
	var inProcess bool
	if target != host && (target == "localhost" || target == "127.0.0.1") && r.Inv.Hosts[target] == nil {
		// Implicit localhost: the control node, over the local connection.
		conn, inProcess = connection.NewLocal(), true
	} else {
		conn, inProcess, err = r.Conns.GetWith(ctx, target, kw)
	}
	if err != nil {
		return nil, target, &unreachableError{host: host, err: err}
	}
	envKeys, env, err := r.taskEnvironment(play, task, vctx)
	if err != nil {
		return nil, target, err
	}
	connecting := r.connectingNote(ctx, inProcess, vctx, host, target)
	disc := &discovery{}
	return &actions.Context{
		Host:          host,
		Vars:          vctx,
		Conn:          conn,
		Become:        become,
		CheckMode:     r.effectiveCheckMode(play, task),
		Diff:          r.effectiveDiff(play, task),
		Background:    task.Async > 0,
		AsyncTimeout:  task.Async,
		BaseDir:       r.Opts.BaseDir,
		SrcDir:        task.SrcDir,
		TaskDir:       taskDir(task),
		Verbosity:     r.Opts.Verbosity,
		RemoteTmp:     r.remoteTmp(vctx),
		Delegated:     target != host,
		DelegateFacts: task.DelegateFacts,
		ArgPos:        argPositions(task),
		RunModule: func(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
			if req.PythonInterpreter == "" {
				req.PythonInterpreter = pythonInterpreter(vctx)
			}
			if req.PythonFallback == nil {
				req.PythonFallback = varList(vctx, "ansible_interpreter_python_fallback")
			}
			b := become
			if b != nil {
				bs := *b
				bs.Shell = r.shellOptions(target, kw, vctx)
				if inProcess {
					// The local connection has no remote_user: the
					// action's is the user running the playbook.
					bs.Shell.RemoteUser = localUser()
				}
				b = &bs
			}
			connecting()
			r.displayModuleRedirect(task, req.Module)
			python := !noPythonModules[req.Module]
			if python {
				r.discoverInterpreter(ctx, disc, host, target, task, vctx, conn)
				if req.PythonInterpreter == "" {
					req.PythonInterpreter = disc.path
				}
			}
			res, err := r.runModule(ctx, host, target, kw, inProcess, b, task, envKeys, env, req, payload)
			if len(disc.warnings) > 0 {
				if err == nil && res != nil {
					if res.Extra == nil {
						res.Extra = map[string]any{}
					}
					prior, _ := res.Extra["warnings"].([]any)
					res.Extra["warnings"] = append(append([]any{}, disc.warnings...), prior...)
					for k, v := range disc.help {
						if res.WarningHelp == nil {
							res.WarningHelp = map[string]string{}
						}
						res.WarningHelp[k] = v
					}
				} else {
					for _, w := range disc.warnings {
						if help := disc.help[w.(string)]; help != "" {
							r.warn(w.(string) + " " + help)
						} else {
							r.warn(w.(string))
						}
					}
				}
				disc.warnings = nil
			}
			if err == nil && res != nil && python && disc.report && res.Origin != "action" {
				// _execute_module propagates the discovery to the
				// controller as a fact in its result (a result the
				// action plugin made itself has none).
				if res.AnsibleFacts == nil {
					res.AnsibleFacts = map[string]any{}
				}
				res.AnsibleFacts[discoveredKey] = disc.path
			}
			return res, err
		},
		Connecting: connecting,
		SetFact: func(name string, value any) {
			// The fact carries where its value came from: the argument
			// (or the value a template in it passed along).
			raw, written := task.Args[name]
			var origin template.OriginRef
			if written {
				origin = vctx.ValueOrigin(raw, argPos(task, name))
			}
			for _, h := range r.factHosts(host, target, task) {
				if written {
					r.Store.SetHostFactOrigin(h, name, value, origin)
				} else {
					r.Store.SetHostFact(h, name, value)
				}
			}
		},
		SetIncludeVars: func(vars map[string]any) {
			for _, h := range r.factHosts(host, target, task) {
				r.Store.SetIncludeVars(h, vars)
			}
		},
		Warn: r.warnBlock,
	}, target, nil
}

// shellOptions are the shell plugin options for a task's temporary files
// on target: the ansible_admin_users, ansible_system_tmpdirs,
// ansible_common_remote_group and ansible_shell_allow_world_readable_temp
// variables over the configuration.
func (r *Runner) shellOptions(target string, kw connection.Keywords, vctx *vars.Context) *connection.ShellOptions {
	sh := r.Conns.Opts.Shell
	sh.RemoteUser = r.Conns.RemoteUser(target, kw)
	sh.RemoteTmp = r.remoteTmp(vctx)
	sh.Warn = r.Conns.Opts.Warn
	if l := varList(vctx, "ansible_admin_users"); l != nil {
		sh.AdminUsers = l
	}
	if l := varList(vctx, "ansible_system_tmpdirs"); l != nil {
		sh.SystemTmpdirs = l
	}
	if v, ok := vctx.Get("ansible_common_remote_group"); ok && v != nil {
		if tv, err := vctx.TemplateValue(v); err == nil {
			v = tv
		}
		sh.CommonRemoteGroup = fmt.Sprint(v)
	}
	if v, ok := vctx.Get("ansible_shell_allow_world_readable_temp"); ok && v != nil {
		if tv, err := vctx.TemplateValue(v); err == nil {
			v = tv
		}
		sh.WorldReadableTemp = template.Truthy(v) && !strings.EqualFold(fmt.Sprint(v), "false") && !strings.EqualFold(fmt.Sprint(v), "no")
	}
	return &sh
}

// varList is a list-typed variable (a list, or a comma-separated
// string), nil when unset.
func varList(vctx *vars.Context, name string) []string {
	v, ok := vctx.Get(name)
	if !ok || v == nil {
		return nil
	}
	if tv, err := vctx.TemplateValue(v); err == nil {
		v = tv
	}
	var out []string
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
	case string:
		for _, e := range strings.Split(t, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	default:
		return nil
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// pythonInterpreter is the interpreter ansible would run the task's module
// with when one is configured (ansible_python_interpreter, else
// ANSIBLE_PYTHON_INTERPRETER), "" for discovery. understudy runs no
// Python; modules whose output depends on it read this.
func pythonInterpreter(vctx *vars.Context) string {
	var interp string
	if v, ok := vctx.Get("ansible_python_interpreter"); ok {
		if tv, err := vctx.TemplateValue(v); err == nil {
			v = tv
		}
		if s, ok := v.(string); ok {
			interp = s
		}
	} else {
		interp = os.Getenv("ANSIBLE_PYTHON_INTERPRETER")
	}
	if strings.HasPrefix(interp, "auto") {
		return ""
	}
	return interp
}

// factHosts is where a task's facts land: the delegate with
// delegate_facts, every host of the batch for run_once, else the host.
func (r *Runner) factHosts(host, target string, task *playbook.Task) []string {
	if task.DelegateFacts && target != host {
		return []string{target}
	}
	return r.fanOut(host, task)
}

// fanOut is the set of hosts a task's register/notify apply to: all hosts
// the task was run_once for, else just the host.
func (r *Runner) fanOut(host string, task *playbook.Task) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if task.RunOnce && len(r.runOnceHosts) > 0 {
		return append([]string(nil), r.runOnceHosts...)
	}
	return []string{host}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// unreachableError marks transport failures (distinct from task failures).
type unreachableError struct {
	host string
	err  error
}

func (e *unreachableError) Error() string { return e.err.Error() }

// effectiveCheckMode resolves the run-level flag against a task's
// check_mode: override (check_mode: false forces execution during a
// --check run; true forces a dry run of that task).
func (r *Runner) effectiveCheckMode(play *playbook.Play, task *playbook.Task) bool {
	if task.CheckMode != nil {
		return *task.CheckMode
	}
	if play != nil && play.CheckMode != nil {
		return *play.CheckMode
	}
	return r.Opts.CheckMode
}

// taskDir is the directory of the file that defined the task.
func taskDir(task *playbook.Task) string {
	if task.Src.File == "" {
		return ""
	}
	return filepath.Dir(task.Src.File)
}

// effectiveDiff resolves --diff against a task's diff: keyword.
func (r *Runner) effectiveDiff(play *playbook.Play, task *playbook.Task) bool {
	if task.Diff != nil {
		return *task.Diff
	}
	if play != nil && play.Diff != nil {
		return *play.Diff
	}
	return r.Opts.Diff
}

// effectiveBecome resolves privilege escalation for a task on a host the
// way PlayContext does: command-line options, then play, block and task
// keywords, then the host's connection variables (ansible_become,
// ansible_become_method/_user/_password/_exe/_flags and the per-method
// ansible_<method>_* forms), which outrank keywords.
func (r *Runner) effectiveBecome(play *playbook.Play, task *playbook.Task, vctx *vars.Context) (_ *connection.BecomeSpec, err error) {
	defer func() {
		// Resolving a host variable panics on a template error.
		if p := recover(); p != nil {
			e, ok := p.(error)
			if !ok {
				panic(p)
			}
			err = e
		}
	}()
	on := r.Opts.Become
	spec := &connection.BecomeSpec{User: r.Opts.BecomeUser, Method: r.Opts.BecomeMethod, Password: r.Opts.BecomePass}
	for _, bf := range []playbook.BecomeFields{play.Become, task.Become} {
		if bf.Become != nil {
			on = *bf.Become
		}
		if bf.BecomeUser != "" {
			spec.User = bf.BecomeUser
		}
		if bf.Method != "" {
			spec.Method = bf.Method
		}
		if bf.Exe != "" {
			spec.Exe = bf.Exe
		}
		if bf.Flags != nil {
			spec.Flags = bf.Flags
		}
	}
	str := func(v any) (string, error) {
		if s, ok := v.(string); ok {
			out, err := vctx.TemplateString(s)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%v", out), nil
		}
		return fmt.Sprintf("%v", v), nil
	}
	hostVar := func(names ...string) (string, bool, error) {
		for _, n := range names {
			if v, ok := vctx.Get(n); ok && v != nil {
				s, err := str(v)
				return s, true, err
			}
		}
		return "", false, nil
	}
	if v, ok := vctx.Get("ansible_become"); ok {
		switch t := v.(type) {
		case bool:
			on = t
		case string:
			s, err := str(t)
			if err != nil {
				return nil, err
			}
			on = template.Truthy(s) && !strings.EqualFold(s, "false") && !strings.EqualFold(s, "no") && s != "0"
		}
	}
	if !on {
		return nil, nil
	}
	m, _, err := hostVar("ansible_become_method")
	if err != nil {
		return nil, err
	}
	if m != "" {
		spec.Method = m
	}
	if strings.Contains(spec.Method, "{{") {
		if spec.Method, err = str(spec.Method); err != nil {
			return nil, err
		}
	}
	if spec.Method != "" {
		norm := playbook.NormalizeBecomeMethod(spec.Method)
		if norm == "" {
			return nil, fmt.Errorf("become_method %q is not supported (supported: sudo, su, doas)", spec.Method)
		}
		spec.Method = norm
	} else {
		spec.Method = "sudo"
	}
	meth := spec.Method
	if s, ok, err := hostVar("ansible_become_user", "ansible_"+meth+"_user"); err != nil {
		return nil, err
	} else if ok {
		spec.User = s
	}
	if s, ok, err := hostVar("ansible_become_password", "ansible_become_pass", "ansible_"+meth+"_password", "ansible_"+meth+"_pass"); err != nil {
		return nil, err
	} else if ok {
		spec.Password = s
	}
	if s, ok, err := hostVar("ansible_become_exe", "ansible_"+meth+"_exe"); err != nil {
		return nil, err
	} else if ok {
		spec.Exe = s
	}
	if s, ok, err := hostVar("ansible_become_flags", "ansible_"+meth+"_flags"); err != nil {
		return nil, err
	} else if ok {
		spec.Flags = &s
	}
	for _, p := range []*string{&spec.User, &spec.Exe} {
		if strings.Contains(*p, "{{") {
			if *p, err = str(*p); err != nil {
				return nil, err
			}
		}
	}
	if spec.Flags != nil && strings.Contains(*spec.Flags, "{{") {
		f, err := str(*spec.Flags)
		if err != nil {
			return nil, err
		}
		spec.Flags = &f
	}
	if spec.User == "" {
		spec.User = "root"
	}
	// The local connection's become_success_timeout (values below 1 use
	// the default).
	if s, ok, err := hostVar("ansible_local_become_success_timeout"); err != nil {
		return nil, err
	} else if ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 1 {
			spec.SuccessTimeout = time.Duration(n) * time.Second
		}
	}
	return spec, nil
}

// taskEnvironment is the task's environment for a host, as
// Task._post_validate_environment builds it: the play's entries, then the
// role's, the enclosing blocks' and the task's own (merged at parse
// time), in order. A mapping entry's values are templated one by one (an
// omitted value is left out); any other entry is a template that must
// yield a mapping. keys lists the variables in the order ansible-core
// writes them, where a variable keeps the place it was first set at.
func (r *Runner) taskEnvironment(play *playbook.Play, task *playbook.Task, vctx *vars.Context) (keys []string, env map[string]string, err error) {
	var entries []any
	if play != nil {
		entries = append(entries, play.Environment...)
	}
	entries = append(entries, task.Environment...)
	if len(entries) == 0 {
		return nil, nil, nil
	}
	if pos, ok := task.KeywordPos["environment"]; ok {
		vctx = vctx.At(pos)
	}
	env = map[string]string{}
	set := func(k string, v any) {
		if _, seen := env[k]; !seen {
			keys = append(keys, k)
		}
		env[k] = template.PyStr(v)
	}
	for _, entry := range entries {
		if m, ok := envMapping(entry); ok {
			for _, k := range m.keys {
				v, err := vctx.TemplateValue(m.get(k))
				if err != nil {
					// Fact gathering tolerates an environment built from
					// the facts it is about to gather.
					if task.Module == "setup" && strings.Contains(err.Error(), "ansible_env") {
						continue
					}
					return nil, nil, err
				}
				if _, omitted := v.(template.Omit); omitted {
					continue
				}
				set(k, v)
			}
			continue
		}
		v, err := vctx.TemplateValue(entry)
		if err != nil {
			return nil, nil, err
		}
		if _, omitted := v.(template.Omit); omitted {
			continue
		}
		m, ok := envMapping(v)
		if !ok {
			r.warn("could not parse environment value, skipping: " + template.PyRepr(entries))
			continue
		}
		for _, k := range m.keys {
			set(k, m.get(k))
		}
	}
	return keys, env, nil
}

// orderedMapping is a mapping's keys in order, with a getter.
type orderedMapping struct {
	keys []string
	get  func(string) any
}

// envMapping views v as a mapping in its key order (a plain map's keys
// sorted).
func envMapping(v any) (orderedMapping, bool) {
	switch t := template.Undeprecate(v).(type) {
	case *yaml.OMap:
		return orderedMapping{keys: t.Keys(), get: t.Get}, true
	case map[string]any:
		return orderedMapping{keys: slices.Sorted(maps.Keys(t)), get: func(k string) any { return t[k] }}, true
	}
	return orderedMapping{}, false
}

// runModule executes a module request: in-process for local connections,
// via the remote agent otherwise (bootstrapped lazily on first use).
func (r *Runner) runModule(ctx context.Context, host, target string, kw connection.Keywords, inProcess bool, become *connection.BecomeSpec, task *playbook.Task, envKeys []string, env map[string]string, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
	if len(env) > 0 {
		req.Env, req.EnvOrder = env, envKeys
	}
	if become != nil && become.User != "" {
		req.BecomeUser = become.User
	}
	if inProcess {
		// Mirror the remote agent's JSON round-trip: module code
		// (internal/modules, which must not import yaml) only ever sees plain
		// JSON-shaped values, never *yaml.OMap. Flatten ordered maps in the args
		// so the in-process path matches what the wire path delivers.
		if m, ok := yaml.AsMap(req.Args).(map[string]any); ok {
			req.Args = m
		}
		if become == nil {
			res := modules.RunContext(ctx, req, payload)
			res.Origin = moduleOrigin(res)
			return res, nil
		}
		// Under become the module runs in a child of this binary, started
		// through the become method like the agent on a remote host.
		if !modules.LocalAgent {
			return nil, fmt.Errorf("become on a local connection needs a binary that serves the understudy agent")
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		login := &connection.LoginInfo{Path: os.Getenv("PATH"), UID: os.Getuid(), GID: os.Getgid()}
		login.Home, _ = os.UserHomeDir()
		client := &connection.AgentClient{Conn: connection.NewLocal(), AgentPath: exe, AgentArg: modules.LocalAgentArg, Login: login}
		res, err := client.Run(ctx, req, payload, become)
		if bf := actions.BecomeFailure(err); bf != nil {
			return bf, nil
		}
		if res != nil {
			res.Origin = moduleOrigin(res)
		}
		return res, err
	}
	agentClient, err := r.Conns.AgentWith(ctx, target, kw)
	if err != nil {
		return nil, err
	}
	res, err := agentClient.Run(ctx, req, payload, become)
	if bf := actions.BecomeFailure(err); bf != nil {
		return bf, nil
	}
	if res != nil {
		res.Origin = moduleOrigin(res)
	}
	return res, err
}

// moduleOrigin classifies a module result for the callback: a module that
// crashed (an unhandled exception in Ansible) carries ansible-core's full
// "Task failed: Module failed: ..." message, shown verbatim.
func moduleOrigin(res *agentproto.Result) string {
	if res.Origin != "" {
		return res.Origin
	}
	if res.Failed && res.Msg == "" && res.Cause == "" && res.ErrorChain == nil {
		// A failure without a msg: ansible-core's error is "Unknown error."
		// and it becomes the result's msg too.
		if m, _ := res.Extra["msg"].(string); m == "" {
			delete(res.Extra, "msg")
			res.Msg = "Task failed: Module failed: Unknown error."
		}
	}
	// An exception with an empty message (a bare NotImplementedError)
	// leaves just "Task failed: Module failed.".
	if res.Failed && (strings.HasPrefix(res.Msg, "Task failed: Module failed: ") || res.Msg == "Task failed: Module failed.") {
		return "verbatim"
	}
	return "module"
}

// dispatch routes to a control-side action or the module runtime.
func (r *Runner) dispatch(ctx context.Context, task *playbook.Task, actx *actions.Context, args map[string]any, freeForm string) *agentproto.Result {
	r.displayRedirects(task)
	// (uri refuses check mode before it gets that far.)
	if transfersFiles[task.Module] && actx.Connecting != nil && !(task.Module == "uri" && actx.CheckMode) {
		actx.Connecting()
	}
	if task.Module == "include_vars" {
		return r.runIncludeVars(task, actx, args)
	}
	if a := actions.Lookup(task.Module); a != nil {
		res := a.Run(ctx, actx, args, freeForm)
		if res != nil && res.Origin == "" {
			res.Origin = "action"
		}
		return res
	}
	if !modules.Exists(task.Module) {
		return agentproto.Fail("unknown module %q", task.Module)
	}
	n := &actions.Normal{Module: task.Module}
	res := n.Run(ctx, actx, args, freeForm)
	addRoutingDeprecation(task, res)
	nameCheckModeSkip(task, res)
	nameUnsupportedParams(task, res)
	return res
}

// record finalizes a task result for one host (non-loop path emits the
// callback here; loops emitted per item already).
func (r *Runner) record(host string, task *playbook.Task, res *agentproto.Result, loopItems []any) {
	if !res.Skipped && (res.Extra == nil || res.Extra["unreachable"] != true) {
		r.markRoleRan(task, host)
	}
	if res.Extra != nil && res.Extra["unreachable"] == true {
		r.Callback.HostUnreachable(host, task, res.Msg)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stats[host].Unreachable++
		r.failed[host] = true
		if r.unreachable == nil {
			r.unreachable = map[string]bool{}
		}
		r.unreachable[host] = true
		return
	}
	ignored := task.IgnoreErrors && res.Failed
	if task.Module == "set_fact" && !res.Failed && !res.Skipped {
		r.warnReserved(setFactOrigins(task))
	}
	if task.Register != "" && template.IsReservedName(task.Register) {
		p := task.KeywordPos["register"]
		r.warnReserved([]template.KeyOrigin{{Name: task.Register, File: p.File, Line: p.Line, Col: p.Col}})
	}
	if res.Changed && !res.Failed && !res.Skipped && len(res.Notify) > 0 {
		// Notifications are saved before the result prints, once per
		// result that carries them (its notify templated for it): a
		// loop's item results that ran.
		for _, h := range r.fanOut(host, task) {
			if !r.notifyHandlers(h, res.Notify, h == host) {
				return // the run ends here, before the result prints
			}
		}
	}
	if loopItems == nil {
		r.Callback.HostResult(host, task, shown(task, res), ignored, nil)
	} else {
		r.Callback.LoopResult(host, task, shown(task, res), ignored)
	}
	if task.Register != "" {
		for _, h := range r.fanOut(host, task) {
			if task.Module == "include_vars" {
				// include_vars data stays trusted (templated on use).
				r.Store.SetHostVarRaw(h, task.Register, orderedResult(task, task.Module, res.ToVars()))
				continue
			}
			r.Store.SetHostFact(h, task.Register, orderedResult(task, task.Module, res.ToVars()))
		}
	}
	// Gathered facts land in the facts layer, both prefixed at top level
	// (inject_facts_as_vars) and under the ansible_facts dict. set_fact
	// writes its own layer via the SetFact hook.
	// Only a successful result's facts are kept (a failed task's are
	// reported but not applied).
	if len(res.AnsibleFacts) > 0 && task.Module != "set_fact" && !res.Failed {
		r.applyFacts(host, res.DelegatedTo, task, res.AnsibleFacts)
	}
	if loopItems != nil && task.Module != "set_fact" {
		// A loop's facts are its items' (each item that did not fail).
		for _, it := range loopItems {
			m, ok := asStringMap(it)
			if !ok || m["failed"] == true {
				continue
			}
			if facts, ok := asStringMap(m["ansible_facts"]); ok && len(facts) > 0 {
				r.applyFacts(host, res.DelegatedTo, task, facts)
			}
		}
	}
	if res.Failed && !ignored && r.catchInRescue(host, task) {
		// Rescued: the fatal line printed, but the host stays in the play
		// and the failure details flow into the rescue section's vars.
		r.Store.SetHostFact(host, "ansible_failed_result", orderedResult(task, task.Module, res.ToVars()))
		r.Store.SetHostFact(host, "ansible_failed_task", map[string]any{
			"name": task.Name, "action": task.Module,
		})
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stats[host].Rescued++
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[host]
	switch {
	case res.Skipped:
		st.Skipped++
	case res.Failed && !ignored:
		st.Failed++
		r.failed[host] = true
		// Remember the blocks this host was inside so their always
		// sections still run for it.
		for _, ref := range task.Blocks {
			if r.failedIn[host] == nil {
				r.failedIn[host] = map[int]bool{}
			}
			r.failedIn[host][ref.ID] = true
		}
	default:
		// ok, changed, and failed-but-ignored all count toward ok, matching
		// Ansible's recap: a changed task is also ok, and an ignored task is
		// ok+ignored (and changed too, if it changed).
		st.OK++
		if res.Changed {
			st.Changed++
		}
		if res.Failed && ignored {
			st.Ignored++
		}
	}
}

func (r *Runner) recordFailure(host string, task *playbook.Task, res *agentproto.Result) {
	r.record(host, task, res, nil)
}

// Stats exposes per-host counters (for tests and API callers).
func (r *Runner) Stats() map[string]*HostStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*HostStats, len(r.stats))
	for h, st := range r.stats {
		copied := *st
		out[h] = &copied
	}
	return out
}

// SortedHosts returns recap ordering.
func SortedHosts(stats map[string]*HostStats) []string {
	out := make([]string, 0, len(stats))
	for h := range stats {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func setExtra(res *agentproto.Result, key string, v any) {
	if res.Extra == nil {
		res.Extra = map[string]any{}
	}
	res.Extra[key] = v
}

// runSection runs one task section. The linear strategy moves all hosts
// through each task together; free and host_pinned let every host walk the
// section on its own, so a slow host never holds the others back. forks
// bounds concurrent task executions through a FIFO semaphore, so hosts
// take turns task by task as in Ansible's free strategy.
func (r *Runner) runSection(ctx context.Context, play *playbook.Play, tasks []*playbook.Task, playHosts []string) error {
	if !freeStrategy(play) {
		return r.runTaskList(ctx, play, tasks, playHosts, nil, 0)
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, host := range r.activeOf(playHosts) {
		g.Go(func() error {
			return r.runTaskList(gctx, play, tasks, playHosts, []string{host}, 0)
		})
	}
	return g.Wait()
}

func freeStrategy(play *playbook.Play) bool {
	return play != nil && (play.Strategy == "free" || play.Strategy == "host_pinned")
}

// taskStart announces a task. Under the free strategies results from
// different tasks interleave, so the banner is deferred to the first
// result of that task (freeCallback), as Ansible prints it.
func (r *Runner) taskStart(task *playbook.Task, name string, handler bool) {
	if fc, ok := r.Callback.(*freeCallback); ok {
		fc.announce(task, name, handler)
		return
	}
	r.Callback.TaskStart(task, name, handler)
}

// freeCallback wraps the output callback during free-strategy plays:
// every result is preceded by its task's banner whenever the previous
// output belonged to a different task. Banner and result print atomically.
type freeCallback struct {
	Callback
	mu    sync.Mutex
	names map[*playbook.Task]freeBanner
	last  *playbook.Task
}

type freeBanner struct {
	name    string
	handler bool
}

func (f *freeCallback) announce(task *playbook.Task, name string, handler bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[task.Identity()] = freeBanner{name, handler}
}

func (f *freeCallback) banner(task *playbook.Task) {
	task = task.Identity()
	if f.last == task {
		return
	}
	f.last = task
	if b, ok := f.names[task]; ok {
		f.Callback.TaskStart(task, b.name, b.handler)
	}
}

func (f *freeCallback) HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.banner(task)
	f.Callback.HostResult(host, task, res, ignored, item)
}

func (f *freeCallback) LoopResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.banner(task)
	f.Callback.LoopResult(host, task, res, ignored)
}

func (f *freeCallback) HostUnreachable(host string, task *playbook.Task, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.banner(task)
	f.Callback.HostUnreachable(host, task, msg)
}

func (f *freeCallback) Included(task *playbook.Task, target string, hosts []string, item any, hasItem bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.banner(task)
	f.Callback.Included(task, target, hosts, item, hasItem)
}

// turnstile hands out forks slots to hosts in queue order. Hosts start
// queued in inventory order and rejoin at the tail each time they want
// their next task, so with forks=1 hosts take turns task by task exactly
// like Ansible's free strategy.
type turnstile struct {
	mu     sync.Mutex
	cond   *sync.Cond
	free   int
	queue  []string
	queued map[string]bool
}

func newTurnstile(slots int, hosts []string) *turnstile {
	t := &turnstile{free: slots, queued: map[string]bool{}}
	t.cond = sync.NewCond(&t.mu)
	for _, h := range hosts {
		t.queue = append(t.queue, h)
		t.queued[h] = true
	}
	return t
}

func (t *turnstile) acquire(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.queued[host] {
		t.queue = append(t.queue, host)
		t.queued[host] = true
	}
	for t.free == 0 || t.queue[0] != host {
		t.cond.Wait()
	}
	t.queue = t.queue[1:]
	t.queued[host] = false
	t.free--
	t.cond.Broadcast()
}

func (t *turnstile) release() {
	t.mu.Lock()
	t.free++
	t.mu.Unlock()
	t.cond.Broadcast()
}

// argPos is where a module argument was written: its own value in the
// mapping form, else the module's k=v string.
func argPos(task *playbook.Task, key string) template.Position {
	if p, ok := task.ArgPos[key]; ok {
		return p
	}
	return task.ArgsPos
}

// argPositions is argPos for every argument of the task.
func argPositions(task *playbook.Task) map[string]template.Position {
	if len(task.Args) == 0 {
		return task.ArgPos
	}
	out := make(map[string]template.Position, len(task.Args))
	for k := range task.Args {
		if p := argPos(task, k); p.File != "" {
			out[k] = p
		}
	}
	return out
}

// loopTemplateError is a loop value that did not template.
type loopTemplateError struct {
	pos template.Position
	err error
}

func (e *loopTemplateError) Error() string { return e.err.Error() }
func (e *loopTemplateError) Unwrap() error { return e.err }

// loopFailure is the failed result of a task whose loop could not be
// resolved: a template error is raised as is, at the loop's origin.
func loopFailure(err error) *agentproto.Result {
	var le *loopTemplateError
	if errors.As(err, &le) {
		if cause, ok := template.Cause(le.err); ok {
			res := agentproto.Fail("%s", cause)
			res.Origin = "verbatim"
			res.ErrorChain = &agentproto.ErrorChain{Inner: cause,
				InnerFile: le.pos.File, InnerLine: le.pos.Line, InnerCol: le.pos.Col}
			return res
		}
	}
	return agentproto.Fail("error templating loop: %v", err)
}

// conditionalError is a conditional keyword (when, until, changed_when,
// failed_when) that did not evaluate, at the conditional's origin.
type conditionalError struct {
	keyword string
	pos     template.Position
	err     error
}

func (e *conditionalError) Error() string { return e.message() }

// message is ansible-core's "A 'when' expression failed: <cause>".
func (e *conditionalError) message() string {
	article := "A"
	if e.keyword == "until" {
		article = "An"
	}
	return fmt.Sprintf("%s '%s' expression failed: %s", article, e.keyword, template.ConditionalCause(e.err))
}

// result is the task's failed result: "Task failed" caused by the
// conditional's error.
func (e *conditionalError) result() *agentproto.Result {
	inner := e.message()
	res := agentproto.Fail("Task failed: %s", inner)
	res.Origin = "verbatim"
	res.ErrorChain = e.chain("Task failed.")
	return res
}

// chain is the error's display below outer: the conditional's failure
// at its origin (a plugin error raised while handling another split off).
func (e *conditionalError) chain(outer string) *agentproto.ErrorChain {
	ec := &agentproto.ErrorChain{Outer: outer, Inner: e.message(),
		InnerFile: e.pos.File, InnerLine: e.pos.Line, InnerCol: e.pos.Col}
	if be, ok := template.IsBrokenConditional(e.err); ok {
		// The broken conditional is its own event, at the conditional,
		// with how to allow it.
		article := "A"
		if e.keyword == "until" {
			article = "An"
		}
		ec.Inner = fmt.Sprintf("%s '%s' expression failed.", article, e.keyword)
		ec.Root = brokenConditionalChain(be)
	} else if ie, ok := e.err.(*template.IndirectConditionalError); ok {
		// The expression a template made fails where its text came from.
		article := "A"
		if e.keyword == "until" {
			article = "An"
		}
		cause, _ := template.Cause(ie.Err)
		ec.Inner = fmt.Sprintf("%s '%s' expression failed: Error while evaluating conditional.", article, e.keyword)
		ec.Root = &agentproto.ErrorChain{Inner: cause, InnerFile: ie.Pos.File, InnerLine: ie.Pos.Line, InnerCol: ie.Pos.Col}
	} else if head, detail, ok := template.SplitCause(e.err); ok {
		article := "A"
		if e.keyword == "until" {
			article = "An"
		}
		ec.Inner = fmt.Sprintf("%s '%s' expression failed: %s", article, e.keyword, head)
		ec.Root = &agentproto.ErrorChain{Inner: detail}
	}
	return ec
}

// actionResult is the module's result failed by a changed_when or
// failed_when that did not evaluate (raised in the action: "Task failed:
// Action failed").
func (e *conditionalError) actionResult(res *agentproto.Result) *agentproto.Result {
	out := *res
	out.Failed = true
	out.ErrorChain = e.chain("Task failed: Action failed.")
	return &out
}

// brokenConditionalChain is a broken conditional's event: its message at
// the conditional, with its help.
func brokenConditionalChain(be *template.BrokenConditionalError) *agentproto.ErrorChain {
	return &agentproto.ErrorChain{Inner: be.Msg, Help: be.Help,
		InnerFile: be.Pos.File, InnerLine: be.Pos.Line, InnerCol: be.Pos.Col}
}

// evalConditionals evaluates a conditional keyword's list: all must
// hold, each evaluated at its own origin.
func evalConditionals(task *playbook.Task, vctx *vars.Context, keyword string, conds []string) (bool, *conditionalError) {
	for _, cond := range conds {
		pos := conditionalPos(task, keyword, cond)
		ok, err := vctx.At(pos).EvalWhen([]string{cond})
		if err != nil {
			return false, &conditionalError{keyword: keyword, pos: pos, err: err}
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// conditionalPos is where a conditional keyword's expression came from:
// the list entry's own origin, else the keyword's value.
func conditionalPos(task *playbook.Task, keyword, cond string) template.Position {
	if file, line, col, ok := yaml.Origin(cond); ok {
		return template.Position{File: file, Line: line, Col: col}
	}
	return task.KeywordPos[keyword]
}

// whenSkip evaluates when: conditions in order. Like Ansible, a skip
// reports the first condition that was false (a literal false as the
// boolean itself). nil means the task runs.
func whenSkip(vctx *vars.Context, when []string, pos map[string]template.Position) (*agentproto.Result, *conditionalError) {
	for _, cond := range when {
		ok, err := vctx.At(pos[cond]).EvalWhen([]string{cond})
		if err != nil {
			return nil, &conditionalError{keyword: "when", pos: pos[cond], err: err}
		}
		if !ok {
			var failed any = cond
			if strings.EqualFold(cond, "false") {
				failed = false
			}
			return &agentproto.Result{Skipped: true, Extra: map[string]any{
				"false_condition": failed,
				"skip_reason":     "Conditional result was False",
			}}, nil
		}
	}
	return nil, nil
}

// invalidSetFactName is set_fact's failure for a fact name that is not a
// valid variable name (the first, in the task's order), nil when all are.
func invalidSetFactName(task *playbook.Task, args map[string]any) *agentproto.Result {
	keys := slices.Collect(maps.Keys(args))
	slices.SortFunc(keys, func(a, b string) int {
		pa, pb := task.ArgKeyPos[a], task.ArgKeyPos[b]
		if pa.Line != pb.Line {
			return pa.Line - pb.Line
		}
		if pa.Col != pb.Col {
			return pa.Col - pb.Col
		}
		return strings.Compare(a, b)
	})
	for _, k := range keys {
		if k == "cacheable" || playbook.ValidVariableName(k) {
			continue
		}
		msg, help := playbook.InvalidVariableName(k)
		res := agentproto.Fail("Task failed: %s", msg)
		res.Origin = "verbatim"
		p, ok := task.ArgKeyPos[k]
		if !ok {
			p = argPos(task, k)
		}
		res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: msg, Help: help,
			InnerFile: p.File, InnerLine: p.Line, InnerCol: p.Col}
		return res
	}
	return nil
}

// setFactOrigins are the reserved names set_fact sets, where each was
// written, in source order.
func setFactOrigins(task *playbook.Task) []template.KeyOrigin {
	var out []template.KeyOrigin
	for k := range task.Args {
		if k == "cacheable" || !template.IsReservedName(k) {
			continue
		}
		p, ok := task.ArgKeyPos[k]
		if !ok {
			p = argPos(task, k)
		}
		out = append(out, template.KeyOrigin{Name: k, File: p.File, Line: p.Line, Col: p.Col})
	}
	slices.SortFunc(out, func(a, b template.KeyOrigin) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
	return out
}

// applyFacts records a result's facts for the hosts they belong to, both
// prefixed at top level (inject_facts_as_vars) and under ansible_facts,
// updating the facts already cached (host_cache |= facts).
func (r *Runner) applyFacts(host, delegatedTo string, task *playbook.Task, facts map[string]any) {
	target := host
	if delegatedTo != "" {
		target = delegatedTo
	}
	stripped := make(map[string]any, len(facts))
	for k, v := range facts {
		if k == "ansible_local" {
			stripped[k] = v // namespace_facts keeps ansible_local as-is
			continue
		}
		stripped[strings.TrimPrefix(k, "ansible_")] = v
	}
	for _, h := range r.factHosts(host, target, task) {
		r.Store.SetFacts(h, r.deprecatedFacts(facts))
		merged := stripped
		if old, ok := r.Store.Fact(h, "ansible_facts"); ok {
			if m, ok := asStringMap(old); ok {
				merged = maps.Clone(m)
				maps.Copy(merged, stripped)
			}
		}
		r.Store.SetFacts(h, map[string]any{"ansible_facts": merged})
	}
}

// markRoleRan records that a task of a role ran on host (its result was
// ok or failed, not skipped or unreachable).
func (r *Runner) markRoleRan(task *playbook.Task, host string) {
	if task.Role == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.roleRan[task.Role.Key] == nil {
		r.roleRan[task.Role.Key] = map[string]bool{}
	}
	r.roleRan[task.Role.Key][host] = true
}

// roleComplete is the implicit role_complete meta: the role completed on
// each host one of its tasks ran on.
func (r *Runner) roleComplete(task *playbook.Task, playHosts, restrict []string) {
	active := r.activeOf(playHosts)
	if restrict != nil {
		active = intersect(active, restrict)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range active {
		if !r.roleRan[task.Role.Key][h] {
			continue
		}
		if r.roleDone[task.Role.Key] == nil {
			r.roleDone[task.Role.Key] = map[string]bool{}
		}
		r.roleDone[task.Role.Key][h] = true
	}
}

// roleNotDone narrows the hosts a role task may run on (restrict, else
// the play's) to those its role has not completed on.
func (r *Runner) roleNotDone(task *playbook.Task, playHosts, restrict []string) []string {
	hosts := restrict
	if hosts == nil {
		hosts = playHosts
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	done := r.roleDone[task.Role.Key]
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !done[h] {
			out = append(out, h)
		}
	}
	return out
}

// warnReserved shows warn_if_reserved's warning for each variable named
// with a reserved name, once per distinct origin.
func (r *Runner) warnReserved(origins []template.KeyOrigin) {
	for _, o := range origins {
		r.warnBlock(template.ReservedWarning(o))
	}
}

// warnReservedFor checks the variables a task sees on host, as get_vars
// does before the task runs: the play's roles' defaults, the host's
// inventory variables (its groups' then its own), the roles' vars, then
// the task's (and its blocks') vars.
func (r *Runner) warnReservedFor(host string, task *playbook.Task) {
	if play := r.curPlay; play != nil {
		r.warnReserved(play.RoleDefaultOrigins)
	}
	if r.Inv != nil {
		if h := r.Inv.Hosts[host]; h != nil {
			for _, g := range r.Inv.OrderedGroups(h) {
				r.warnReserved(g.VarOrigins)
			}
			r.warnReserved(h.VarOrigins)
		}
	}
	if play := r.curPlay; play != nil {
		r.warnReserved(play.RoleVarOrigins)
	}
	r.warnReserved(task.VarOrigins)
}

// isUnreachable reports whether a host has an unreachable result.
func (r *Runner) isUnreachable(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[host]
	return st != nil && st.Unreachable > 0
}

// startAt implements --start-at-task: tasks are skipped until one whose
// name matches, exactly or as a glob. It reports whether the task should
// run.
func (r *Runner) startAt(name string) bool {
	if name != r.Opts.StartAtTask {
		if ok, _ := path.Match(r.Opts.StartAtTask, name); !ok {
			return false
		}
	}
	r.startedAt = true
	return true
}

// stepTask implements --step: it asks whether to run the task (or
// handler), reporting whether it should run.
func (r *Runner) stepTask(task *playbook.Task, handler bool) bool {
	if !r.stepContinue {
		// StrategyBase._take_step: the prompt names the task as its repr
		// does (the untemplated name, else the action), then the prompt
		// repeats as a banner whatever the answer.
		stepName := task.Name
		if stepName == "" {
			stepName = task.DisplayAction()
		}
		if task.RoleName != "" {
			stepName = task.RoleName + " : " + stepName
		}
		kind := "TASK: "
		if handler {
			kind = "HANDLER: " // Handler.__repr__
		}
		msg := "Perform task: " + kind + stepName + " (N)o/(y)es/(c)ontinue: "
		fmt.Fprint(os.Stdout, msg)
		answer, err := r.debugIn().ReadString('\n')
		if err != nil && answer == "" {
			// input() at end of input raises EOFError, which
			// ansible-playbook reports as an unexpected exception.
			fmt.Fprint(os.Stderr, "[ERROR]: Unexpected Exception, this is probably a bug: EOF when reading a line\n")
			r.quit(250)
			return false
		}
		run := true
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		case "c", "continue":
			r.stepContinue = true
		default:
			run = false
		}
		msg = strings.TrimSpace(msg)
		fmt.Fprintf(os.Stdout, "\n%s %s\n", msg, strings.Repeat("*", max(3, displayColumns()-utf8.RuneCountInString(msg))))
		return run
	}
	return true
}

// displayColumns is Display.columns: the terminal width less one, at
// least 79.
func displayColumns() int {
	fd := int(os.Stdout.Fd())
	if term.IsTerminal(fd) {
		if w, _, err := term.GetSize(fd); err == nil && w-1 > 79 {
			return w - 1
		}
	}
	return 79
}

// pollAsync waits for an async job started with poll > 0, checking it
// every poll seconds with async_status like ansible's strategy does.
func (r *Runner) pollAsync(ctx context.Context, host string, task *playbook.Task, actx *actions.Context, started *agentproto.Result) *agentproto.Result {
	jid, _ := started.Extra["ansible_job_id"].(string)
	poll := task.Poll
	if poll < 0 {
		poll = 10 // ansible's default poll interval
	}
	left := task.Async
	var last *agentproto.Result
	for left > 0 {
		time.Sleep(time.Duration(poll) * time.Second)
		left -= poll
		st, err := actx.RunModule(ctx, &agentproto.TaskRequest{
			Proto: agentproto.ProtoVersion, Op: "task", Module: "async_status",
			Args: map[string]any{"jid": jid},
		}, nil)
		if err != nil {
			return agentproto.Fail("async_status: %v", err)
		}
		last = st
		if f, _ := st.Extra["finished"].(bool); f {
			r.Callback.AsyncDone(host, jid, st.Failed)
			return st
		}
		r.Callback.AsyncPoll(host, jid)
	}
	r.Callback.AsyncDone(host, jid, true)
	status := map[string]any{"ansible_job_id": jid, "changed": false, "deprecations": []any{},
		"failed": false, "finished": false, "started": true, "stderr": "", "stderr_lines": []any{},
		"stdout": "", "stdout_lines": []any{}, "warnings": []any{}}
	if last != nil {
		if rf, ok := last.Extra["results_file"]; ok {
			status["results_file"] = rf
		}
	}
	// ansible-core 2.19+'s wording when a job outlives async:.
	// Not an exception: no [ERROR] block, no exception key.
	return &agentproto.Result{Failed: true, Msg: "async task produced unparsable results",
		Origin: "plain", Extra: map[string]any{"async_result": status}}
}

// parallelBlock finds the outermost not-yet-running parallel block
// (understudy_parallel) enclosing task, with its nesting level.
func (r *Runner) parallelBlock(task *playbook.Task) (playbook.BlockRef, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for level, ref := range task.Blocks {
		if ref.Parallel && !r.parallelActive[ref.ID] {
			return ref, level, true
		}
	}
	return playbook.BlockRef{}, 0, false
}

func hasBlockRef(t *playbook.Task, id, level int) bool {
	return len(t.Blocks) > level && t.Blocks[level].ID == id && t.Blocks[level].Section == playbook.SectionBlock
}

// runParallel runs a parallel block's direct children concurrently: each
// task, or each nested block as a whole, is one unit that runs across the
// hosts as usual. All units finish before execution continues. Units must
// be independent: a failing unit does not stop its siblings (the block's
// rescue/always still see the failure afterwards).
func (r *Runner) runParallel(ctx context.Context, play *playbook.Play, tasks []*playbook.Task, level, id int, playHosts, restrict []string, depth int) error {
	var units [][]*playbook.Task
	for i := 0; i < len(tasks); {
		j := i + 1
		if len(tasks[i].Blocks) > level+1 {
			child := tasks[i].Blocks[level+1].ID
			for j < len(tasks) && len(tasks[j].Blocks) > level+1 && tasks[j].Blocks[level+1].ID == child {
				j++
			}
		}
		units = append(units, tasks[i:j])
		i = j
	}
	r.mu.Lock()
	if r.parallelActive == nil {
		r.parallelActive = map[int]bool{}
	}
	r.parallelActive[id] = true
	inner := r.Callback
	if _, already := inner.(*freeCallback); !already {
		// Results of concurrent tasks interleave: print each with its banner.
		r.Callback = &freeCallback{Callback: inner, names: map[*playbook.Task]freeBanner{}}
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.parallelActive, id)
		r.Callback = inner
		r.mu.Unlock()
	}()
	g, gctx := errgroup.WithContext(ctx)
	for _, unit := range units {
		g.Go(func() error {
			return r.runTaskList(gctx, play, unit, playHosts, restrict, depth)
		})
	}
	return g.Wait()
}

func isAssertModule(m string) bool {
	return m == "assert" || m == "ansible.builtin.assert" || m == "ansible.legacy.assert"
}

func assertThat(vctx *vars.Context, raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	if !(strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") ||
		strings.HasPrefix(s, "{%") && strings.HasSuffix(s, "%}")) {
		return raw
	}
	v, err := vctx.TemplateValue(raw)
	if err != nil {
		return raw
	}
	if l, isList := v.([]any); isList {
		return l
	}
	return raw
}

func (r *Runner) quitRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.userQuit
}

// requestQuit stops the run after the debugger's quit: sys.exit(99), as
// on KeyboardInterrupt.
func (r *Runner) requestQuit() { r.quit(99) }

// quit stops the run at once, with no recap, exiting with code.
func (r *Runner) quit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userQuit, r.aborted, r.playEnded = true, true, true
	r.quitCode = code
}

// hostSnapshot is the per-host state a debugger redo rolls back.
type hostSnapshot struct {
	failed   bool
	failedIn map[int]bool
	blockF   map[int]bool
}

func (r *Runner) snapshotHost(host string) hostSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return hostSnapshot{failed: r.failed[host],
		failedIn: maps.Clone(r.failedIn[host]), blockF: maps.Clone(r.blockFailed[host])}
}

// restoreHost undoes a recorded result for a redo the way ansible-core's
// debugger does: the host's failed state is rolled back, and the stats
// are decremented (never below zero) for each of failed, unreachable,
// changed and skipped the result has, plus ok, whether or not the result
// was counted that way.
func (r *Runner) restoreHost(host string, snap hostSnapshot, res *agentproto.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[host]
	dec := func(n *int) {
		if *n > 0 {
			*n--
		}
	}
	if res.Failed {
		dec(&st.Failed)
	}
	if res.Extra != nil && res.Extra["unreachable"] == true {
		dec(&st.Unreachable)
	}
	if res.Changed {
		dec(&st.Changed)
	}
	if res.Skipped {
		dec(&st.Skipped)
	}
	dec(&st.OK)
	r.failed[host] = snap.failed
	r.failedIn[host] = snap.failedIn
	r.blockFailed[host] = snap.blockF
}
