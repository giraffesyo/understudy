// Package actions implements Ansible's action-plugin split: some "modules"
// run on the control node (debug, set_fact, template rendering), the rest
// are forwarded to the module runtime (in-process for local connections,
// the remote agent over SSH). Both paths return the same Result shape, so
// the executor never knows where a task ran.
package actions

import (
	"context"
	"io"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/vars"
)

// Context carries everything one action invocation needs.
type Context struct {
	Host         string
	Vars         *vars.Context
	Conn         connection.Connection
	Become       *connection.BecomeSpec
	CheckMode    bool
	Diff         bool
	Background   bool   // run as an async job (async > 0)
	AsyncTimeout int    // async: seconds
	BaseDir      string // playbook directory, for src/vars_files resolution
	SrcDir       string // role root when the task came from a role ("" otherwise)
	TaskDir      string // directory of the file the task was defined in
	Verbosity    int

	// RunModule executes a module on the target (in-process or via agent).
	RunModule func(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (*agentproto.Result, error)
	// SetFact persists a fact for this host (set_fact, setup).
	SetFact func(name string, value any)
}

// Action runs one task occurrence. args/freeForm are already templated.
type Action interface {
	Run(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result
}

var registry = map[string]Action{}

// Register installs a control-side action.
func Register(name string, a Action) { registry[name] = a }

// Lookup returns the control-side action for a module name, or nil when the
// task should be forwarded to the module runtime.
func Lookup(name string) Action { return registry[name] }

// Known reports whether a name resolves to an action or module.
func Known(name string) bool {
	if _, ok := registry[name]; ok {
		return true
	}
	return modules.Exists(name)
}

// Normal forwards a task to the module runtime.
type Normal struct{ Module string }

func (n *Normal) Run(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result {
	req := &agentproto.TaskRequest{
		Proto:        agentproto.ProtoVersion,
		Op:           "task",
		Module:       n.Module,
		Args:         args,
		FreeForm:     freeForm,
		CheckMode:    actx.CheckMode,
		Diff:         actx.Diff,
		Background:   actx.Background,
		AsyncTimeout: actx.AsyncTimeout,
	}
	res, err := actx.RunModule(ctx, req, nil)
	if err != nil {
		return agentproto.Fail("module execution failed: %v", err)
	}
	return res
}
