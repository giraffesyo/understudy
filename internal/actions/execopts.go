package actions

import (
	"errors"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/connection"
)

// execOptions builds connection exec options from the action context.
func execOptions(actx *Context) connection.ExecOptions {
	return connection.ExecOptions{Become: actx.Become}
}

// BecomeFailure turns a privilege escalation failure raised by the
// connection into ansible-core's task result: "Task failed: <reason>",
// unreachable when escalation timed out. It returns nil for other errors.
func BecomeFailure(err error) *agentproto.Result {
	var be *connection.BecomeError
	if !errors.As(err, &be) {
		return nil
	}
	res := &agentproto.Result{Failed: true, Msg: "Task failed: " + be.Msg, Origin: "verbatim"}
	if be.Unreachable {
		res.Extra = map[string]any{"unreachable": true}
	}
	return res
}
