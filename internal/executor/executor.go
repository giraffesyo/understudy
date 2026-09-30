// Package executor runs plays with the linear strategy: every host finishes
// task N before task N+1 starts, with per-task parallelism bounded by forks.
package executor

import (
	"bufio"
	"context"
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

	"golang.org/x/sync/errgroup"

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

// Options configure a run.
type Options struct {
	Forks        int
	CheckMode    bool
	Diff         bool
	Verbosity    int
	ExtraVars    map[string]any
	Become       bool
	BecomeUser   string
	BecomeMethod string // --become-method ("" = sudo)
	BecomePass   string
	Connection   string // "" = per-host behavioral vars; "local" forces local
	BaseDir      string // playbook directory
	Tags         []string
	SkipTags     []string
	RolesPath    []string                  // roles_path search directories (after <playbook>/roles)
	ConfigFile   string                    // ansible.cfg in effect ("" = none): ansible_config_file
	Inventory    []string                  // inventory sources: ansible_inventory_sources
	ConnOpts     connection.ManagerOptions // ssh-level settings (user, keys, host key checking)

	ForceHandlers bool           // --force-handlers: notified handlers run on failed hosts too
	StartAtTask   string         // --start-at-task: skip tasks until one matches
	Step          bool           // --step: confirm each task interactively
	Vault         *vault.Secrets // vault passwords for !vault values and encrypted files

	// NoDeprecationWarnings is deprecation_warnings=False.
	NoDeprecationWarnings bool
	// InjectFactsSet is INJECT_FACTS_AS_VARS set explicitly (not left at
	// its deprecated default): top-level facts then do not warn.
	InjectFactsSet bool
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

	// DebugIn/DebugOut are the task debugger's terminal (default stdin
	// and stdout).
	DebugIn  io.Reader
	DebugOut io.Writer

	stats          map[string]*HostStats
	order          []string
	failed         map[string]bool
	notified       map[string]map[string]bool // handler name -> hosts to run on
	blockFailed    map[string]map[int]bool    // host -> block ID -> failure caught by rescue
	nextBlockID    int                        // fresh IDs for blocks of included files
	failedIn       map[string]map[int]bool    // host -> blocks it was inside when it failed hard
	ended          map[string]bool            // meta: end_host (per play)
	runOnceHosts   []string                   // hosts a running run_once task fans out to
	curPlay        *playbook.Play
	warned         map[string]bool
	freeSem        *turnstile   // free strategy: forks shared fairly across hosts
	parallelActive map[int]bool // parallel blocks currently running
	startedAt      bool
	stepContinue   bool
	aborted        bool // any_errors_fatal: stop the playbook
	userQuit       bool // task debugger: quit (exit 99, no recap)
	dbgReader      *bufio.Reader
	dbgMu          sync.Mutex
	playEnded      bool // meta: end_play
	batchEnded     bool // meta: end_batch
	mu             sync.Mutex
}

// NewRunner builds a runner over a loaded inventory.
func NewRunner(inv *inventory.Inventory, cb Callback, opts Options) *Runner {
	if opts.Forks <= 0 {
		opts.Forks = 5
	}
	engine := template.New()
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
	return r
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
		if _, ok := r.stats[h.Name]; !ok {
			r.stats[h.Name] = &HostStats{}
			r.order = append(r.order, h.Name)
		}
	}
	return names, nil
}

// Run executes all plays and returns the exit code (0 ok, 2 failures, 4
// unreachable hosts).
func (r *Runner) Run(ctx context.Context, plays []*playbook.Play) (int, error) {
	for _, play := range plays {
		if err := r.runPlay(ctx, play); err != nil {
			return 1, err
		}
		if r.quitRequested() {
			// The debugger's quit: sys.exit(99), as on KeyboardInterrupt.
			return 99, nil
		}
		if r.aborted {
			break
		}
	}
	r.Callback.Recap(r.stats, r.order)
	// TaskQueueManager's RUN_UNREACHABLE_HOSTS (4) outranks
	// RUN_FAILED_HOSTS (2): unreachable hosts are remembered for the run.
	code := 0
	for _, st := range r.stats {
		if st.Unreachable > 0 {
			return 4, nil
		}
		if st.Failed > 0 {
			code = 2
		}
	}
	return code, nil
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
	}
	// ansible-core reads vars_files before the banner: a file that fails
	// to parse ends the run without one.
	r.Callback.PlayStart(play)

	allHosts, err := r.resolvePlayHosts(play)
	if err != nil {
		return err
	}
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
		err := r.runPlayBatch(ctx, play, batch)
		// When every host of a batch fails, ansible-playbook stops the
		// whole run: no further batches, plays or playbooks.
		if err == nil && len(batch) > 0 && len(r.failedSet(batch))-failedBefore == len(batch) {
			r.mu.Lock()
			r.aborted = true
			r.mu.Unlock()
			return nil
		}
		if err == errBatchAborted || r.batchBreached(batch, play.MaxFailPercentage) {
			fmt.Printf("NO MORE HOSTS LEFT: batch failure exceeded max_fail_percentage (%.0f%%); aborting play\n",
				play.MaxFailPercentage)
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

// batchBreached reports whether a batch's failure/unreachable count exceeds
// max_fail_percentage (>=0). Ansible aborts when failures are STRICTLY
// greater than the threshold.
func (r *Runner) batchBreached(batch []string, maxFailPct float64) bool {
	if maxFailPct < 0 || len(batch) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := 0
	for _, h := range batch {
		if st := r.stats[h]; st != nil && (st.Failed > 0 || st.Unreachable > 0) {
			failed++
		}
	}
	return float64(failed)/float64(len(batch))*100.0 > maxFailPct
}

const maxIncludeDepth = 100

// runTaskList executes a task sequence with the linear strategy. restrict
// (non-nil) limits execution to a host subset — used by include_tasks,
// whose file may differ per host.
func (r *Runner) runTaskList(ctx context.Context, play *playbook.Play, tasks []*playbook.Task, playHosts, restrict []string, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("include_tasks nesting exceeds %d levels (include loop?)", maxIncludeDepth)
	}
	for i := 0; i < len(tasks); i++ {
		task := tasks[i]
		if r.playEnded || r.batchEnded {
			return nil
		}
		if ref, level, ok := r.parallelBlock(task); ok {
			end := i
			for end < len(tasks) && hasBlockRef(tasks[end], ref.ID, level) {
				end++
			}
			if err := r.runParallel(ctx, play, tasks[i:end], level, ref.ID, playHosts, restrict, depth); err != nil {
				return err
			}
			i = end - 1
			continue
		}
		if !r.tagsMatch(task, play) {
			continue
		}
		if (r.Opts.StartAtTask != "" && !r.startedAt) || r.Opts.Step {
			if task.Module != "include_tasks" && task.Module != "include_role" && task.Module != "import_role" &&
				!r.startGate(task, r.taskDisplayName(task, playHosts)) {
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
	tasks, err := r.loadIncludedRole(play, task, name)
	if err != nil {
		for _, host := range active {
			r.record(host, task, agentproto.Fail("%s: %v", task.Module, err), nil)
		}
		return nil
	}
	r.adoptBlocks(task, tasks)
	for _, t := range tasks {
		if len(task.Vars) > 0 {
			merged := maps.Clone(task.Vars)
			maps.Copy(merged, t.Vars)
			t.Vars = merged
		}
		t.Tags = append(append([]string{}, task.Tags...), t.Tags...)
		if len(task.When) > 0 {
			t.When = append(append([]string{}, task.When...), t.When...)
		}
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
	r.notified = map[string]map[string]bool{}
}

// notifyHandlers marks handlers notified by one host (called on change).
func (r *Runner) notifyHandlers(host string, names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if r.notified[name] == nil {
			r.notified[name] = map[string]bool{}
		}
		r.notified[name][host] = true
	}
}

// flushHandlers runs notified handlers in definition order across the hosts
// that notified them, clearing the notification set.
func (r *Runner) flushHandlers(ctx context.Context, play *playbook.Play, playHosts []string) error {
	r.mu.Lock()
	handlers := append([]*playbook.Task(nil), play.Handlers...)
	r.mu.Unlock()
	for _, handler := range handlers {
		key := handler.Name
		r.mu.Lock()
		hosts := r.notified[key]
		delete(r.notified, key)
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
		if h, ok := r.Inv.Hosts[host]; ok {
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
	if _, ok := h.r.Inv.Hosts[host]; !ok {
		return nil, false
	}
	// Build the target host's context on demand and expose it as a mapping.
	return h.r.newHostContext(host, h.pos, h.playHosts).AsMapping(), true
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
		vctx := r.newHostContext(host, pos, playHosts)
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
	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	base := r.newHostContext(host, pos, playHosts)
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
		return agentproto.Fail("error templating loop: %v", err), nil, task
	}

	if !isLoop {
		return r.runOnce(ctx, play, task, host, base, nil), nil, task
	}

	// Loop: aggregate per-item results Ansible-style.
	agg := &agentproto.Result{Extra: map[string]any{}}
	var itemResults []any
	anyChanged, anyFailed, allSkipped := false, false, true
	for i, item := range items {
		// Rebuild the per-item context from the store each iteration so a
		// set_fact from an earlier item is visible to later ones (Ansible's
		// accumulate-in-a-loop pattern).
		itemCtx := r.newHostContext(host, pos, playHosts)
		if len(task.Vars) > 0 {
			itemCtx = itemCtx.WithOverlay(task.Vars)
		}
		if len(override) > 0 {
			itemCtx = itemCtx.WithOverlay(override)
		}
		overlay := map[string]any{task.LoopVar: vars.Final{V: item}}
		if task.IndexVar != "" {
			overlay[task.IndexVar] = int64(i)
		}
		itemCtx = itemCtx.WithOverlay(overlay)
		res := r.runOnce(ctx, play, task, host, itemCtx, item)
		// Per-item results carry the loop variable(s), as Ansible's do.
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		res.Extra["ansible_loop_var"] = task.LoopVar
		res.Extra[task.LoopVar] = item
		if task.IndexVar != "" {
			res.Extra["ansible_index_var"] = task.IndexVar
			res.Extra[task.IndexVar] = int64(i)
		}
		label := item
		if task.LoopLabel != nil {
			if l, err := itemCtx.TemplateValue(task.LoopLabel); err == nil {
				label = l
			}
		}
		r.Callback.HostResult(host, task, shown(task, res), false, label)
		m := res.ToVars()
		itemResults = append(itemResults, m)
		anyChanged = anyChanged || res.Changed
		anyFailed = anyFailed || res.Failed
		allSkipped = allSkipped && res.Skipped
	}
	agg.Changed = anyChanged
	agg.Failed = anyFailed
	agg.Skipped = allSkipped
	if itemResults == nil {
		itemResults = []any{}
	}
	agg.Extra["results"] = itemResults
	switch {
	case len(items) == 0:
		agg.Extra["skip_reason"] = "No items in the list"
		agg.Extra["skipped_reason"] = deprecate(deprecatedSkippedReason, "No items in the list")
	case anyFailed:
		agg.Msg = "One or more items failed"
	case allSkipped:
		agg.Msg = "All items skipped"
	default:
		agg.Msg = "All items completed"
	}
	return agg, itemResults, task
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
		return nil, false, err
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
		return agentproto.Fail("The conditional check failed: %v", err)
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
	args := make(map[string]any, len(task.Args))
	for _, k := range slices.Sorted(maps.Keys(task.Args)) { // ansible-core templates them in key order
		raw := task.Args[k]
		if k == "that" && isAssertModule(task.Module) {
			// assert's finalize_task_arg: 'that' stays raw (each entry is
			// a conditional), except that a string that is entirely a
			// template may resolve to a list of conditionals.
			args[k] = assertThat(vctx.At(argPos(task, k)), raw)
			continue
		}
		v, err := argsCtx.At(argPos(task, k)).TemplateValue(raw)
		if err != nil {
			return agentproto.Fail("error templating argument %q: %v", k, err)
		}
		if _, isOmit := v.(template.Omit); isOmit {
			continue
		}
		args[k] = v
	}
	freeForm := task.FreeForm
	if freeForm != "" {
		v, err := vctx.At(task.ArgsPos).TemplateString(freeForm)
		if err != nil {
			return agentproto.Fail("error templating command: %v", err)
		}
		freeForm = fmt.Sprintf("%v", v)
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
	var res *agentproto.Result
	retriesExhausted := false
	for attempt := 1; attempt <= total; attempt++ {
		res = r.dispatch(ctx, task, actx, args, freeForm)
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
			ok, err := vctx.WithOverlay(registerOverlay(task, res)).EvalWhen([]string{task.Until})
			if err != nil {
				return agentproto.Fail("error evaluating until condition: %v", err)
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
			r.Callback.Retrying(host, task, name, total-(attempt+1), shown(task, res))
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
	return res
}

// applyChangedFailedWhen lets changed_when / failed_when override the
// module's own verdict; a non-nil return is an evaluation error result.
func applyChangedFailedWhen(task *playbook.Task, vctx *vars.Context, res *agentproto.Result) *agentproto.Result {
	if len(task.ChangedWhen) > 0 || len(task.FailedWhen) > 0 {
		resCtx := vctx.WithOverlay(registerOverlay(task, res))
		if len(task.ChangedWhen) > 0 {
			ok, err := resCtx.EvalWhen(task.ChangedWhen)
			if err != nil {
				return agentproto.Fail("error evaluating changed_when: %v", err)
			}
			res.Changed = ok
			setExtra(res, "changed_when_result", ok)
		}
		if len(task.FailedWhen) > 0 {
			ok, err := resCtx.EvalWhen(task.FailedWhen)
			if err != nil {
				return agentproto.Fail("error evaluating failed_when: %v", err)
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
	m := res.ToVars()
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
	return &actions.Context{
		Host:         host,
		Vars:         vctx,
		Conn:         conn,
		Become:       become,
		CheckMode:    r.effectiveCheckMode(play, task),
		Diff:         r.effectiveDiff(play, task),
		Background:   task.Async > 0,
		AsyncTimeout: task.Async,
		BaseDir:      r.Opts.BaseDir,
		SrcDir:       task.SrcDir,
		TaskDir:      taskDir(task),
		Verbosity:    r.Opts.Verbosity,
		RemoteTmp:    r.remoteTmp(vctx),
		ArgPos:       argPositions(task),
		RunModule: func(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
			return r.runModule(ctx, host, target, kw, inProcess, become, task, req, payload)
		},
		SetFact: func(name string, value any) {
			for _, h := range r.factHosts(host, target, task) {
				r.Store.SetHostFact(h, name, value)
			}
		},
		SetIncludeVars: func(vars map[string]any) {
			for _, h := range r.factHosts(host, target, task) {
				r.Store.SetIncludeVars(h, vars)
			}
		},
	}, target, nil
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

// runModule executes a module request: in-process for local connections,
// via the remote agent otherwise (bootstrapped lazily on first use).
func (r *Runner) runModule(ctx context.Context, host, target string, kw connection.Keywords, inProcess bool, become *connection.BecomeSpec, task *playbook.Task, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
	if len(task.Environment) > 0 {
		env := make(map[string]string, len(task.Environment))
		vctx := r.Store.NewContext(host, template.Position{File: task.Src.File, Line: task.Src.Line})
		for k, v := range task.Environment {
			tv, err := vctx.TemplateValue(v)
			if err != nil {
				return nil, err
			}
			env[k] = fmt.Sprintf("%v", tv)
		}
		req.Env = env
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
			res := modules.Run(req, payload)
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
		client := &connection.AgentClient{Conn: connection.NewLocal(), AgentPath: connection.ShellQuote(exe) + " " + modules.LocalAgentArg}
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
	if res.Failed && strings.HasPrefix(res.Msg, "Task failed: Module failed: ") {
		return "verbatim"
	}
	return "module"
}

// dispatch routes to a control-side action or the module runtime.
func (r *Runner) dispatch(ctx context.Context, task *playbook.Task, actx *actions.Context, args map[string]any, freeForm string) *agentproto.Result {
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
	return n.Run(ctx, actx, args, freeForm)
}

// record finalizes a task result for one host (non-loop path emits the
// callback here; loops emitted per item already).
func (r *Runner) record(host string, task *playbook.Task, res *agentproto.Result, loopItems []any) {
	if res.Extra != nil && res.Extra["unreachable"] == true {
		r.Callback.HostUnreachable(host, task, res.Msg)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stats[host].Unreachable++
		r.failed[host] = true
		return
	}
	ignored := task.IgnoreErrors && res.Failed
	if loopItems == nil {
		r.Callback.HostResult(host, task, shown(task, res), ignored, nil)
	} else {
		r.Callback.LoopResult(host, task, shown(task, res), ignored)
	}
	if task.Register != "" {
		for _, h := range r.fanOut(host, task) {
			if task.Module == "include_vars" {
				// include_vars data stays trusted (templated on use).
				r.Store.SetHostVarRaw(h, task.Register, res.ToVars())
				continue
			}
			r.Store.SetHostFact(h, task.Register, res.ToVars())
		}
	}
	// Gathered facts land in the facts layer, both prefixed at top level
	// (inject_facts_as_vars) and under the ansible_facts dict. set_fact
	// writes its own layer via the SetFact hook.
	if len(res.AnsibleFacts) > 0 && task.Module != "set_fact" {
		target := host
		if res.DelegatedTo != "" {
			target = res.DelegatedTo
		}
		stripped := make(map[string]any, len(res.AnsibleFacts))
		for k, v := range res.AnsibleFacts {
			if k == "ansible_local" {
				stripped[k] = v // namespace_facts keeps ansible_local as-is
				continue
			}
			stripped[strings.TrimPrefix(k, "ansible_")] = v
		}
		for _, h := range r.factHosts(host, target, task) {
			r.Store.SetFacts(h, r.deprecatedFacts(res.AnsibleFacts))
			r.Store.SetFacts(h, map[string]any{"ansible_facts": stripped})
		}
	}
	if res.Changed && !res.Failed && len(task.Notify) > 0 {
		for _, h := range r.fanOut(host, task) {
			r.notifyHandlers(h, task.Notify)
		}
	}
	if res.Failed && !ignored && r.catchInRescue(host, task) {
		// Rescued: the fatal line printed, but the host stays in the play
		// and the failure details flow into the rescue section's vars.
		r.Store.SetHostFact(host, "ansible_failed_result", res.ToVars())
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

// whenSkip evaluates when: conditions in order. Like Ansible, a skip
// reports the first condition that was false (a literal false as the
// boolean itself). nil means the task runs.
func whenSkip(vctx *vars.Context, when []string, pos map[string]template.Position) (*agentproto.Result, error) {
	for _, cond := range when {
		if cond == "" {
			continue
		}
		ok, err := vctx.At(pos[cond]).EvalWhen([]string{cond})
		if err != nil {
			return nil, err
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

// isUnreachable reports whether a host has an unreachable result.
func (r *Runner) isUnreachable(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[host]
	return st != nil && st.Unreachable > 0
}

// startGate implements --start-at-task (tasks are skipped until one whose
// name matches, exactly or as a glob) and --step (confirm each task).
// It reports whether the task should run.
func (r *Runner) startGate(task *playbook.Task, name string) bool {
	if r.Opts.StartAtTask != "" && !r.startedAt {
		if name != r.Opts.StartAtTask {
			if ok, _ := path.Match(r.Opts.StartAtTask, name); !ok {
				return false
			}
		}
		r.startedAt = true
	}
	if r.Opts.Step && !r.stepContinue {
		fmt.Fprintf(os.Stdout, "Perform task: TASK: %s (N)o/(y)es/(c)ontinue: ", name)
		var answer string
		fmt.Fscanln(os.Stdin, &answer)
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		case "c", "continue":
			r.stepContinue = true
		default:
			return false
		}
		fmt.Fprintln(os.Stdout)
	}
	return true
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

// requestQuit stops the run after the debugger's quit.
func (r *Runner) requestQuit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userQuit, r.aborted, r.playEnded = true, true, true
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
