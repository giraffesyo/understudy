package actions

import (
	"context"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/connection"
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
	cmd := freeForm
	// _low_level_execute_command(executable=...): run the line through
	// the given shell instead of the default one.
	if exe, ok := args["executable"].(string); ok && exe != "" {
		cmd = exe + " -c " + connection.ShellQuote(cmd)
	}
	if actx.Connecting != nil {
		actx.Connecting()
	}
	res, err := actx.Conn.Exec(ctx, cmd, execOptions(actx))
	if err != nil {
		if bf := BecomeFailure(err); bf != nil {
			return bf
		}
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
