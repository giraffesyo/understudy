package connection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// AgentClient executes module tasks by invoking the remote agent with a
// JSON frame on stdin and parsing the sentinel-guarded result from stdout.
type AgentClient struct {
	Conn      Connection
	AgentPath string
}

// Run executes one task through the agent.
func (c *AgentClient) Run(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader, become *BecomeSpec) (*agentproto.Result, error) {
	var stdin bytes.Buffer
	if err := agentproto.WriteFrame(&stdin, req, payload); err != nil {
		return nil, err
	}

	res, err := c.Conn.Exec(ctx, c.AgentPath+" run", ExecOptions{
		Stdin:  &stdin,
		Become: become,
	})
	if err != nil {
		return nil, err
	}
	if res.RC != 0 {
		// Nonzero agent exit = infrastructure error, not a module failure.
		if become != nil && IsSudoPasswordError(res.Stderr) {
			return nil, fmt.Errorf("Missing sudo password (configure NOPASSWD or use --ask-become-pass)")
		}
		return nil, fmt.Errorf("agent exited with rc=%d: %s", res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	return agentproto.ParseResult(res.Stdout)
}
