// Package executor runs plays with the linear strategy: every host finishes
// task N before task N+1 starts, with per-task parallelism bounded by forks.
package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// Callback receives execution events for display.
type Callback interface {
	PlayStart(play *playbook.Play)
	TaskStart(task *playbook.Task, handler bool)
	HostResult(host string, task *playbook.Task, res *agentproto.Result, ignored bool, item any)
	HostUnreachable(host string, task *playbook.Task, msg string)
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
}

// Runner executes playbooks.
type Runner struct {
	Hosts    []string // resolved host list (inventory integration lands in M5)
	Engine   *template.Engine
	Store    *vars.Store
	Callback Callback
	Opts     Options

	stats  map[string]*HostStats
	order  []string
	failed map[string]bool
	mu     sync.Mutex
}

// NewRunner builds a runner for a resolved host list.
func NewRunner(hosts []string, cb Callback, opts Options) *Runner {
	if opts.Forks <= 0 {
		opts.Forks = 5
	}
	engine := template.New()
	r := &Runner{
		Hosts:    hosts,
		Engine:   engine,
		Store:    vars.NewStore(engine),
		Callback: cb,
		Opts:     opts,
		stats:    map[string]*HostStats{},
		failed:   map[string]bool{},
	}
	for _, h := range hosts {
		r.stats[h] = &HostStats{}
		r.order = append(r.order, h)
	}
	if opts.ExtraVars != nil {
		r.Store.SetExtraVars(opts.ExtraVars)
	}
	playbook.ModuleKnown = actions.Known
	return r
}

// Run executes all plays and returns the exit code (0 ok, 2 failures).
func (r *Runner) Run(ctx context.Context, plays []*playbook.Play) (int, error) {
	for _, play := range plays {
		if err := r.runPlay(ctx, play); err != nil {
			return 1, err
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
	r.Callback.PlayStart(play)
	r.Store.SetPlayVars(play.Vars)

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
		v, err := yaml.Unmarshal(data, path)
		if err != nil {
			return err
		}
		m, ok := v.(map[string]any)
		if !ok && v != nil {
			return fmt.Errorf("%s: vars file must contain a mapping", path)
		}
		r.Store.AddVarsFile(m)
	}

	active := r.activeHosts()
	if len(active) == 0 {
		return nil
	}

	// Task sections run in order; handlers flush after each section (M5
	// completes handler semantics — the flush points are already correct).
	for _, task := range flattenSections(play) {
		active = r.activeHosts()
		if len(active) == 0 {
			break
		}
		r.Callback.TaskStart(task, false)
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(r.Opts.Forks)
		for _, host := range active {
			g.Go(func() error {
				r.runTaskOnHost(gctx, play, task, host)
				return nil
			})
		}
		g.Wait()
	}
	return nil
}

func flattenSections(play *playbook.Play) []*playbook.Task {
	out := make([]*playbook.Task, 0,
		len(play.PreTasks)+len(play.Tasks)+len(play.PostTasks))
	out = append(out, play.PreTasks...)
	out = append(out, play.Tasks...)
	out = append(out, play.PostTasks...)
	return out
}

func (r *Runner) activeHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, h := range r.order {
		if !r.failed[h] {
			out = append(out, h)
		}
	}
	return out
}

func playPos(play *playbook.Play) template.Position {
	return template.Position{File: play.Src.File, Line: play.Src.Line, Col: play.Src.Col}
}

// runTaskOnHost is the per-host task pipeline: when -> loop -> template args
// -> retries -> changed_when/failed_when -> register -> stats.
func (r *Runner) runTaskOnHost(ctx context.Context, play *playbook.Play, task *playbook.Task, host string) {
	pos := template.Position{File: task.Src.File, Line: task.Src.Line, Col: task.Src.Col}
	base := r.Store.NewContext(host, pos)
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
	for _, item := range items {
		itemCtx := base.WithOverlay(map[string]any{task.LoopVar: item})
		res := r.runOnce(ctx, play, task, host, itemCtx, item)
		r.Callback.HostResult(host, task, res, task.IgnoreErrors && res.Failed, item)
		m := res.ToVars()
		m[task.LoopVar] = item
		itemResults = append(itemResults, m)
		anyChanged = anyChanged || res.Changed
		anyFailed = anyFailed || res.Failed
		allSkipped = allSkipped && res.Skipped
	}
	agg.Changed = anyChanged
	agg.Failed = anyFailed
	agg.Skipped = allSkipped
	agg.Extra["results"] = itemResults
	if anyFailed {
		agg.Msg = "One or more items failed"
	} else {
		agg.Msg = "All items completed"
	}
	r.record(host, task, agg, itemResults)
}

// resolveLoop templates the loop value. Returns isLoop=false when absent.
func (r *Runner) resolveLoop(task *playbook.Task, vctx *vars.Context) ([]any, bool, error) {
	if task.Loop == nil {
		return nil, false, nil
	}
	v, err := vctx.TemplateValue(task.Loop)
	if err != nil {
		return nil, false, err
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

	actx := r.actionContext(host, task, play, vctx)

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
		}
		if len(task.FailedWhen) > 0 {
			ok, err := resCtx.EvalWhen(task.FailedWhen)
			if err != nil {
				return agentproto.Fail("error evaluating failed_when: %v", err)
			}
			res.Failed = ok
		}
	}
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

func (r *Runner) actionContext(host string, task *playbook.Task, play *playbook.Play, vctx *vars.Context) *actions.Context {
	become := r.effectiveBecome(play, task)
	conn := r.connFor(host)
	return &actions.Context{
		Host:      host,
		Vars:      vctx,
		Conn:      conn,
		Become:    become,
		CheckMode: r.Opts.CheckMode,
		Diff:      r.Opts.Diff,
		BaseDir:   r.Opts.BaseDir,
		Verbosity: r.Opts.Verbosity,
		RunModule: func(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
			return r.runModule(ctx, host, conn, become, task, req, payload)
		},
		SetFact: func(name string, value any) {
			r.Store.SetHostFact(host, name, value)
		},
	}
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

// connFor returns the connection for a host. M3: local only; the SSH
// connection manager replaces this in M6.
func (r *Runner) connFor(host string) connection.Connection {
	return connection.NewLocal()
}

// runModule executes a module request. For local connections the module
// registry runs in-process — no subprocess, no serialization.
func (r *Runner) runModule(ctx context.Context, host string, conn connection.Connection, become *connection.BecomeSpec, task *playbook.Task, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error) {
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
	// M3: in-process execution (local). M6 adds the agent path over SSH.
	return modules.Run(req, payload), nil
}

// dispatch routes to a control-side action or the module runtime.
func (r *Runner) dispatch(ctx context.Context, task *playbook.Task, actx *actions.Context, args map[string]any, freeForm string) *agentproto.Result {
	if a := actions.Lookup(task.Module); a != nil {
		return a.Run(ctx, actx, args, freeForm)
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
	ignored := task.IgnoreErrors && res.Failed
	if loopItems == nil {
		r.Callback.HostResult(host, task, res, ignored, nil)
	}
	if task.Register != "" {
		r.Store.SetHostFact(host, task.Register, res.ToVars())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stats[host]
	switch {
	case res.Failed && ignored:
		st.Ignored++
		st.OK++
	case res.Failed:
		st.Failed++
		r.failed[host] = true
	case res.Skipped:
		st.Skipped++
	case res.Changed:
		st.Changed++
		st.OK++
	default:
		st.OK++
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
