package actions

import "github.com/giraffesyo/understudy/internal/connection"

// execOptions builds connection exec options from the action context.
func execOptions(actx *Context) connection.ExecOptions {
	return connection.ExecOptions{Become: actx.Become}
}
