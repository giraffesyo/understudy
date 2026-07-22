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
	Become      BecomeFields
	Roles       []*RoleRef
	PreTasks    []*Task
	Tasks       []*Task
	PostTasks   []*Task
	Handlers    []*Task
	Tags        []string
	Environment map[string]any

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
}

// Task is one task (or handler).
type Task struct {
	Name         string
	Module       string
	Args         map[string]any // raw (untemplated) module args
	FreeForm     string         // raw params for command/shell/raw
	When         []string       // list of expressions, ANDed
	Loop         any            // raw list or template string; nil if absent
	LoopWith     string         // lookup plugin name for with_<X> loops ("" = plain loop)
	LoopVar      string         // default "item"
	Async        int            // async timeout seconds (0 = synchronous)
	Poll         int            // poll interval; -1 = unset, 0 = fire-and-forget
	Register     string
	IgnoreErrors bool
	FailedWhen   []string
	ChangedWhen  []string
	Until        string
	Retries      int
	Delay        int
	Become       BecomeFields
	Vars         map[string]any
	Environment  map[string]any
	Notify       []string
	Tags         []string
	NoLog        bool
	Delegate     string
	Blocks       []BlockRef // enclosing blocks, outermost first
	SrcDir       string     // role root for src resolution ("" = playbook dir)
	Src          Pos
}

// RoleRef is one entry in a play's roles: list.
type RoleRef struct {
	Name   string
	Params map[string]any // role vars from the ref (high precedence)
	When   []string
	Tags   []string
	Src    Pos
}
