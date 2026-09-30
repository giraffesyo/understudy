package actions

import (
	"context"
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

func init() {
	Register("unarchive", actionFunc(runUnarchive))
}

// runUnarchive is ansible.builtin.unarchive's action plugin: resolve a
// controller-side src (files/ search path) and transfer it, then run the
// unarchive module on the target. With remote_src (or a URL) the module
// reads src there. The remote "dest must be an existing dir" check runs
// in the module, in the same round trip as the transfer.
func runUnarchive(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	fwd := make(map[string]any, len(args)+1)
	for k, v := range args {
		fwd[k] = v
	}
	remoteSrc := isTruthy(args["remote_src"])
	if c, ok := args["copy"]; ok {
		if _, both := args["remote_src"]; both {
			return actionRaise("parameters are mutually exclusive: ('copy', 'remote_src')")
		}
		delete(fwd, "copy")
		remoteSrc = !isTruthy(c)
		fwd["remote_src"] = remoteSrc
	}
	source, _ := args["src"].(string)
	if args["src"] == nil || args["dest"] == nil {
		return actionRaise("src (or content) and dest are required")
	}
	if creates, _ := args["creates"].(string); creates != "" {
		res, err := actx.RunModule(ctx, &agentproto.TaskRequest{
			Proto: agentproto.ProtoVersion, Op: "task", Module: "stat",
			Args: map[string]any{"path": creates, "get_checksum": false, "get_mime": false, "get_attributes": false},
		}, nil)
		if err != nil {
			return agentproto.Fail("unarchive: %v", err)
		}
		if st, _ := res.Extra["stat"].(map[string]any); st != nil && st["exists"] == true {
			return &agentproto.Result{Skipped: true, Msg: "skipped, since " + creates + " exists"}
		}
	}
	if strings.HasPrefix(source, "~") {
		if home, err := os.UserHomeDir(); err == nil && (source == "~" || strings.HasPrefix(source, "~/")) {
			source = home + source[1:]
		}
	}
	delete(fwd, "decrypt")
	marker := map[string]any{"transfer": !remoteSrc, "remote_tmp": actx.RemoteTmp}
	fwd["_unarchive_action"] = marker
	req := &agentproto.TaskRequest{
		Proto:     agentproto.ProtoVersion,
		Op:        "task",
		Module:    "unarchive",
		Args:      fwd,
		CheckMode: actx.CheckMode,
		Diff:      actx.Diff,
	}
	if remoteSrc {
		res, err := actx.RunModule(ctx, req, nil)
		if err != nil {
			return agentproto.Fail("module execution failed: %v", err)
		}
		return res
	}
	found, searched := searchNeedle(actx, "files", source)
	if found == "" {
		// The AnsibleFileNotFound escapes the action: ansible-core shows
		// a bare "Task failed." caused by it.
		cause := fileNotFound(source, searched)
		res := actionRaise("Task failed: %s", cause)
		res.Origin = "verbatim"
		res.ErrorChain = &agentproto.ErrorChain{
			Outer: "Task failed.",
			Inner: cause,
			Help:  "If you are using a module and expect the file to exist on the remote, see the remote_src option.",
		}
		return res
	}
	f, err := os.Open(found)
	if err != nil {
		return actionRaise("%v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return actionRaise("%v", err)
	}
	req.PayloadLen = info.Size()
	res, err := actx.RunModule(ctx, req, f)
	if err != nil {
		return agentproto.Fail("module execution failed: %v", err)
	}
	return res
}
