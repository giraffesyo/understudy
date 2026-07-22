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
	PreTasks    []*Task
	Tasks       []*Task
	PostTasks   []*Task
	Handlers    []*Task
	Tags        []string
	Environment map[string]any
	Src         Pos
}

// BecomeFields are the become-related keywords, inheritable play -> task.
type BecomeFields struct {
	Become     *bool
	BecomeUser string
}

// Task is one task (or handler).
type Task struct {
	Name         string
	Module       string
	Args         map[string]any // raw (untemplated) module args
	FreeForm     string         // raw params for command/shell/raw
	When         []string       // list of expressions, ANDed
	Loop         any            // raw list or template string; nil if absent
	LoopVar      string         // default "item"
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
	Src          Pos
}
