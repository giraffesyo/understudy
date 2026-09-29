package actions

import (
	"context"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

func init() {
	Register("raw", actionFunc(runRaw))
}

// runRaw executes a command over the bare connection — no agent, no shell
// helpers. Works before bootstrap and on targets the agent doesn't support.
func runRaw(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result {
	if freeForm == "" {
		return agentproto.Fail("raw requires a command")
	}
	if actx.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (raw) does not support check mode"}
	}
	res, err := actx.Conn.Exec(ctx, freeForm, execOptions(actx))
	if err != nil {
		return agentproto.Fail("raw: %v", err)
	}
	out := &agentproto.Result{
		Changed: true,
		RC:      agentproto.IntPtr(res.RC),
		// raw returns output untouched (no trailing-newline strip).
		Stdout: string(res.Stdout),
		Stderr: string(res.Stderr),
	}
	if res.RC != 0 {
		out.Failed = true
		out.Msg = "non-zero return code"
	}
	return out
}
