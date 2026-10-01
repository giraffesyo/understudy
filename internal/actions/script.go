package actions

import (
	"bytes"
	"context"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

func init() {
	Register("script", actionFunc(runScript))
}

// runScript resolves the local script (first word of the free-form line),
// then forwards it as the payload of the remote script module with the
// remaining words as arguments.
func runScript(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result {
	if freeForm == "" {
		if cmd, ok := args["cmd"].(string); ok {
			freeForm = cmd
		}
	}
	if freeForm == "" {
		return agentproto.Fail("script requires a script path")
	}
	words := strings.Fields(freeForm)
	localPath := words[0]
	scriptArgs := strings.Join(words[1:], " ")

	resolved := resolveSrc(actx, localPath, "files")
	if resolved == "" {
		return agentproto.Fail("script: could not find %q (searched relative to the playbook and its files/ directory)", localPath)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return agentproto.Fail("script: %v", err)
	}

	fwd := map[string]any{"_args": scriptArgs}
	for _, k := range []string{"creates", "removes", "executable", "chdir"} {
		if v, ok := args[k]; ok {
			fwd[k] = v
		}
	}
	req := &agentproto.TaskRequest{
		Proto:      agentproto.ProtoVersion,
		Op:         "task",
		Module:     "script",
		Args:       fwd,
		CheckMode:  actx.CheckMode,
		Diff:       actx.Diff,
		PayloadLen: int64(len(content)),
		PkgShim:    actx.InParallel,
	}
	res, err := actx.RunModule(ctx, req, bytes.NewReader(content))
	if err != nil {
		return agentproto.Fail("script: %v", err)
	}
	return res
}
