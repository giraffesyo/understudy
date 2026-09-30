package modules

import (
	"io"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// LocalAgentArg is the hidden argv[1] under which the control binary acts
// as the agent ("<binary> __understudy_agent run"). Local connections run
// modules in-process, except under become: then the module runs in a child
// process of this binary started through the become method, as the agent
// does on a remote host.
const LocalAgentArg = "__understudy_agent"

// LocalAgent is set by binaries that dispatch LocalAgentArg to ServeFrame
// (the understudy CLI, and programs embedding the understudy package).
var LocalAgent bool

// ServeFrame is the agent's "run" command: read one task frame from r, run
// the module, and write the sentinel-guarded result to w. A broken frame
// still produces a well-formed result so the control side gets a diagnosis
// instead of a parse failure. It returns the process exit code.
func ServeFrame(r io.Reader, w io.Writer) int {
	req, payload, err := agentproto.ReadFrame(r)
	if err != nil {
		agentproto.WriteResult(w, agentproto.Fail("%v", err))
		return 0
	}
	if err := agentproto.WriteResult(w, Run(req, payload)); err != nil {
		return 1
	}
	return 0
}
