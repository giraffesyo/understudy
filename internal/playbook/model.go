// Package playbook models plays and tasks and loads them from YAML with
// position information preserved for error messages.
package playbook

import "github.com/giraffesyo/understudy/internal/template"

// Pos is a document position (file:line:col).
type Pos = template.Position

// Play is one play in a playbook.
type Play struct {
	Name        string
	HostPattern string
	Vars        map[string]any
	VarsFiles   []string
	GatherFacts *bool // nil = default (true)
	// GatherArgs holds the play's gather_subset / gather_timeout /
	// fact_path keywords, passed as arguments to the implicit setup task.
	GatherArgs  map[string]any
	Become      BecomeFields
	Roles       []*RoleRef
	PreTasks    []*Task
	Tasks       []*Task
	PostTasks   []*Task
	Handlers    []*Task
	Tags        []string
	Environment map[string]any

	// Serial batches the play across hosts (rolling execution). Entries are
	// host counts or "N%" strings; nil runs all hosts in one batch.
	RemoteUser     string // remote_user keyword
	Connection     string // connection keyword
	AnyErrorsFatal bool
	ForceHandlers  bool
	VarsPrompt     []VarPrompt
	Dir            string // absolute directory of the playbook file (playbook_dir)
	Strategy       string // linear (default), free, host_pinned
	Serial         []any
	// MaxFailPercentage aborts the play when more than this percent of a
	// batch fails. -1 means unset (any failure removes only that host).
	MaxFailPercentage float64

	// Filled by ResolveRoles: per-role vars for the store's role layers.
	RoleDefaults []map[string]any
	RoleVars     []map[string]any

	Src Pos
}

// BecomeFields are the become-related keywords, inheritable play -> task.
type BecomeFields struct {
	Become     *bool
	BecomeUser string
}

// Block sections, recorded on tasks via BlockRef.
const (
	SectionBlock = iota
	SectionRescue
	SectionAlways
)

// BlockRef ties a task to one enclosing block. Blocks are flattened at
// parse time; the executor uses these refs for rescue/always semantics.
type BlockRef struct {
	ID        int
	Section   int // the section of this block the task sits in
	HasRescue bool
	// Parallel: the block's vars set understudy_parallel (an opt-in that
	// ansible-playbook ignores): its direct tasks run concurrently.
	Parallel bool
}

// Task is one task (or handler).
type Task struct {
	Name           string
	Module         string
	Args           map[string]any // raw (untemplated) module args
	FreeForm       string         // raw params for command/shell/raw
	When           []string       // list of expressions, ANDed
	Loop           any            // raw list or template string; nil if absent
	LoopWith       string         // lookup plugin name for with_<X> loops ("" = plain loop)
	LoopVar        string         // default "item"
	IndexVar       string         // loop_control.index_var (0-based); "" = none
	LoopLabel      any            // loop_control.label (raw template); nil = show the item
	Async          int            // async timeout seconds (0 = synchronous)
	Poll           int            // poll interval; -1 = unset, 0 = fire-and-forget
	CheckMode      *bool          // per-task check_mode override (nil = inherit run)
	Diff           *bool          // per-task diff override (nil = inherit run)
	Register       string
	IgnoreErrors   bool
	FailedWhen     []string
	ChangedWhen    []string
	Until          string
	Retries        int
	RetriesSet     bool // retries keyword given (else 3 when until is set)
	Delay          int
	Become         BecomeFields
	Vars           map[string]any
	Environment    map[string]any
	Notify         []string
	Tags           []string
	NoLog          bool
	Delegate       string
	DelegateFacts  bool       // delegate_facts: facts land on the delegate
	RunOnce        bool       // run_once: first host runs, results fan out
	AnyErrorsFatal *bool      // any_errors_fatal (nil = inherit from play)
	RemoteUser     string     // remote_user keyword
	Connection     string     // connection keyword
	Blocks         []BlockRef // enclosing blocks, outermost first
	SrcDir         string     // role root for src resolution ("" = playbook dir)
	RoleName       string     // owning role (for "role : task" banners); "" = play task
	Src            Pos
}

// RoleRef is one entry in a play's roles: list.
type RoleRef struct {
	Name   string
	Params map[string]any // role vars from the ref (high precedence)
	When   []string
	Tags   []string
	Src    Pos
}

// VarPrompt is one vars_prompt entry.
type VarPrompt struct {
	Name, Prompt string
	Default      any
	Private      bool
	Confirm      bool
	Encrypt      string
	Salt         string
	SaltSize     int
	Unsafe       bool
}
