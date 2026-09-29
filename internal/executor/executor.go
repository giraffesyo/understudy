// Package executor runs plays with the linear strategy: every host finishes
// task N before task N+1 starts, with per-task parallelism bounded by forks.
package executor

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
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
	Forks      int
	CheckMode  bool
	Diff       bool
	Verbosity  int
	ExtraVars  map[string]any
	Become     bool
	BecomeUser string
	BecomePass string
	Connection string // "" = per-host behavioral vars; "local" forces local
	BaseDir    string // playbook directory
	Tags       []string
	SkipTags   []string
	ConnOpts   connection.ManagerOptions // ssh-level settings (user, keys, host key checking)
	Vault      *vault.Secrets            // vault passwords for !vault values and encrypted files
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

	stats        map[string]*HostStats
	order        []string
	failed       map[string]bool
	notified     map[string]map[string]bool // handler name -> hosts to run on
	blockFailed  map[string]map[int]bool    // host -> block ID -> failure caught by rescue
	failedIn     map[string]map[int]bool    // host -> blocks it was inside when it failed hard
	ended        map[string]bool            // meta: end_host (per play)
	runOnceHosts []string                   // hosts a running run_once task fans out to
	curPlay      *playbook.Play
	aborted      bool // any_errors_fatal: stop the playbook
	playEnded    bool // meta: end_play
	batchEnded   bool // meta: end_batch
	mu           sync.Mutex
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
	playbook.ModuleKnown = actions.Known
	return r
}

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

// Run executes all plays and returns the exit code (0 ok, 2 failures).
func (r *Runner) Run(ctx context.Context, plays []*playbook.Play) (int, error) {
	for _, play := range plays {
		if err := r.runPlay(ctx, play); err != nil {
			return 1, err
		}
		if r.aborted {
			break
		}
	}
	r.Callback.Recap(r.stats, r.order)
	for _, st := range r.stats {
		if st.Failed > 0 || st.Unreachable > 0 {
			return 2, nil
		}
	}
	return 0, nil
}

func (r *Runner) runPlay(ctx context.Context, play *playbook.Play) error {
	if len(play.Roles) > 0 {
		return fmt.Errorf("internal error: play %q has unresolved roles (playbook.ResolveRoles was not called)", play.Name)
	}
	r.Callback.PlayStart(play)
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
		err := r.runPlayBatch(ctx, play, batch)
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
	r.resetNotified()
	r.mu.Lock()
	r.blockFailed = map[string]map[int]bool{}
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
		if err := r.runTaskList(ctx, play, section, playHosts, nil, 0); err != nil {
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
	for _, task := range tasks {
		if r.playEnded || r.batchEnded {
			return nil
		}
		if !r.tagsMatch(task, play) {
			continue
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
			name = task.Module
		}
		return task.RoleName + " : " + name
	}
	return name
}

// runTaskAcrossHosts executes one task on all active hosts with forks
// parallelism (one errgroup per task = the linear-strategy barrier).
func (r *Runner) runTaskAcrossHosts(ctx context.Context, play *playbook.Play, task *playbook.Task, active, playHosts []string, handler bool) {
	r.Callback.TaskStart(task, r.taskDisplayName(task, active), handler)
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
	g.SetLimit(r.Opts.Forks)
	for _, host := range active {
		g.Go(func() error {
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
	for _, handler := range play.Handlers {
		key := handler.Name
		r.mu.Lock()
		hosts := r.notified[key]
		delete(r.notified, key)
		r.mu.Unlock()
		if len(hosts) == 0 {
			continue
		}
		var active []string
		for _, h := range r.activeOf(playHosts) {
			if hosts[h] {
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
		c.SetMagic("play_hosts", list)
		c.SetMagic("ansible_play_hosts_all", list)
	}
	c.SetMagic("playbook_dir", r.Opts.BaseDir)
	c.SetMagic("ansible_check_mode", r.Opts.CheckMode)
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
	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	base := r.newHostContext(host, pos, playHosts)
	if len(task.Vars) > 0 {
		base = base.WithOverlay(task.Vars)
	}

	// Resolve the loop (nil = run once with no loop var).
	items, isLoop, err := r.resolveLoop(task, base)
	if err != nil {
		r.recordFailure(host, task, agentproto.Fail("error templating loop: %v", err))
		return
	}

	if !isLoop {
		res := r.runOnce(ctx, play, task, host, base, nil)
		r.record(host, task, res, nil)
		return
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
		overlay := map[string]any{task.LoopVar: item}
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
		r.Callback.HostResult(host, task, res, false, label)
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
		agg.Extra["skipped_reason"] = "No items in the list"
	case anyFailed:
		agg.Msg = "One or more items failed"
	case allSkipped:
		agg.Msg = "All items skipped"
	default:
		agg.Msg = "All items completed"
	}
	r.record(host, task, agg, itemResults)
}

// resolveLoop templates the loop value. Returns isLoop=false when absent.
// with_<lookup> loops feed the templated terms through the lookup plugin.
func (r *Runner) resolveLoop(task *playbook.Task, vctx *vars.Context) ([]any, bool, error) {
	if task.Loop == nil {
		return nil, false, nil
	}
	v, err := vctx.TemplateValue(task.Loop)
	if err != nil {
		return nil, false, err
	}
	if task.LoopWith != "" {
		if r.Engine.Lookup == nil {
			return nil, false, fmt.Errorf("with_%s: lookups are not available", task.LoopWith)
		}
		terms, ok := v.([]any)
		if !ok {
			terms = []any{v}
		}
		out, err := r.Engine.Lookup(nil, task.LoopWith, terms, nil)
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
	if len(task.When) > 0 {
		ok, err := vctx.EvalWhen(task.When)
		if err != nil {
			return agentproto.Fail("The conditional check failed: %v", err)
		}
		if !ok {
			return &agentproto.Result{Skipped: true, Msg: "Conditional result was False"}
		}
	}

	// Template module args, dropping omitted ones.
	args := make(map[string]any, len(task.Args))
	for k, raw := range task.Args {
		v, err := vctx.TemplateValue(raw)
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
		v, err := vctx.TemplateString(freeForm)
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

	// until/retries loop.
	attempts := task.Retries + 1
	if task.Until == "" {
		attempts = 1
	}
	var res *agentproto.Result
	for attempt := 1; attempt <= attempts; attempt++ {
		res = r.dispatch(ctx, task, actx, args, freeForm)
		if task.Until == "" {
			break
		}
		resCtx := vctx.WithOverlay(registerOverlay(task, res))
		ok, err := resCtx.EvalWhen([]string{task.Until})
		if err != nil {
			return agentproto.Fail("error evaluating until condition: %v", err)
		}
		if ok {
			if res.Extra == nil {
				res.Extra = map[string]any{}
			}
			res.Extra["attempts"] = attempt
			break
		}
		if attempt < attempts {
			time.Sleep(time.Duration(task.Delay) * time.Second)
		} else {
			res.Failed = true
			if res.Extra == nil {
				res.Extra = map[string]any{}
			}
			res.Extra["attempts"] = attempt
			if res.Msg == "" {
				res.Msg = "Retries exhausted"
			}
		}
	}

	// changed_when / failed_when override the module's own verdict.
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
			res.Failed = ok
			setExtra(res, "failed_when_result", ok)
			if ok {
				// ansible-core's wording; the callback shows it verbatim.
				res.Msg = "Task failed: Action failed: A 'failed_when' expression evaluated to 'True'."
				res.Origin = "verbatim"
			}
		}
	}
	res.DelegatedTo = delegated
	return res
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

// actionContext builds the execution context for one occurrence of a task
// on host. With delegate_to, the task keeps host's variables but runs over
// the delegate's connection (its own connection vars); the returned target
// names where it ran.
func (r *Runner) actionContext(ctx context.Context, host string, task *playbook.Task, play *playbook.Play, vctx *vars.Context) (*actions.Context, string, error) {
	become := r.effectiveBecome(play, task)
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
	var err error
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
		Host:       host,
		Vars:       vctx,
		Conn:       conn,
		Become:     become,
		CheckMode:  r.effectiveCheckMode(task),
		Diff:       r.Opts.Diff,
		Background: task.Async > 0 && task.Poll == 0,
		BaseDir:    r.Opts.BaseDir,
		SrcDir:     task.SrcDir,
		Verbosity:  r.Opts.Verbosity,
		RunModule: func(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
			return r.runModule(ctx, host, target, kw, inProcess, become, task, req, payload)
		},
		SetFact: func(name string, value any) {
			for _, h := range r.factHosts(host, target, task) {
				r.Store.SetHostFact(h, name, value)
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
func (r *Runner) effectiveCheckMode(task *playbook.Task) bool {
	if task.CheckMode != nil {
		return *task.CheckMode
	}
	return r.Opts.CheckMode
}

func (r *Runner) effectiveBecome(play *playbook.Play, task *playbook.Task) *connection.BecomeSpec {
	on := r.Opts.Become
	user := r.Opts.BecomeUser
	if play.Become.Become != nil {
		on = *play.Become.Become
	}
	if play.Become.BecomeUser != "" {
		user = play.Become.BecomeUser
	}
	if task.Become.Become != nil {
		on = *task.Become.Become
	}
	if task.Become.BecomeUser != "" {
		user = task.Become.BecomeUser
	}
	if !on {
		return nil
	}
	if user == "" {
		user = "root"
	}
	return &connection.BecomeSpec{User: user, Method: "sudo", Password: r.Opts.BecomePass}
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
		res := modules.Run(req, payload)
		res.Origin = "module"
		return res, nil
	}
	agentClient, err := r.Conns.AgentWith(ctx, target, kw)
	if err != nil {
		return nil, err
	}
	res, err := agentClient.Run(ctx, req, payload, become)
	if res != nil {
		res.Origin = "module"
	}
	return res, err
}

// dispatch routes to a control-side action or the module runtime.
func (r *Runner) dispatch(ctx context.Context, task *playbook.Task, actx *actions.Context, args map[string]any, freeForm string) *agentproto.Result {
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
		r.Callback.HostResult(host, task, res, ignored, nil)
	} else {
		r.Callback.LoopResult(host, task, res, ignored)
	}
	if task.Register != "" {
		for _, h := range r.fanOut(host, task) {
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
			r.Store.SetFacts(h, res.AnsibleFacts)
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
