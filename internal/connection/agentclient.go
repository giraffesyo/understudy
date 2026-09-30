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
	// Login is the login user (from the bootstrap probe), nil when
	// unknown.
	Login *LoginInfo
}

// Run executes one task through the agent.
func (c *AgentClient) Run(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader, become *BecomeSpec) (*agentproto.Result, error) {
	agentPath := c.AgentPath
	if become != nil && c.Login != nil {
		// Under become the task still reports paths and finds Python as
		// the login user would: remote_tmp is under the login user's
		// home, interpreter discovery searches the login PATH.
		req.LoginHome, req.LoginUID, req.LoginGID = c.Login.Home, c.Login.UID, c.Login.GID
		req.DiscoveryPath = c.Login.Path
	}
	if becomeUnprivileged(become) {
		// An unprivileged become user cannot reach the login user's
		// files: like a module's AnsiballZ payload, the agent runs from a
		// system temp dir made readable to it (the shell reports the dir
		// with a trailing slash).
		dir, err := systemTmp(ctx, c.Conn, become.Shell)
		if err != nil {
			return nil, err
		}
		defer c.Conn.Exec(context.WithoutCancel(ctx), "rm -f -r "+ShellQuote(dir)+" > /dev/null 2>&1", ExecOptions{})
		mod := dir + "/AnsiballZ_" + moduleShortName(req.Module) + ".py"
		prep := "cp " + ShellQuote(c.AgentPath) + " " + ShellQuote(mod)
		if err := fixupPerms(ctx, c.Conn, prep, []string{dir + "/", mod}, become); err != nil {
			return nil, err
		}
		req.StageDir = dir
		agentPath = ShellQuote(mod)
	}
	var stdin bytes.Buffer
	if err := agentproto.WriteFrame(&stdin, req, payload); err != nil {
		return nil, err
	}

	res, err := c.Conn.Exec(ctx, agentPath+" run", ExecOptions{
		Stdin:  &stdin,
		Become: become,
	})
	if err != nil {
		return nil, err
	}
	if res.RC != 0 {
		// Nonzero agent exit = infrastructure error, not a module failure.
		if become != nil && become.method() == "sudo" && IsSudoPasswordError(res.Stderr) {
			return nil, fmt.Errorf("Missing sudo password (configure NOPASSWD or use --ask-become-pass)")
		}
		return nil, fmt.Errorf("agent exited with rc=%d: %s", res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	return agentproto.ParseResult(res.Stdout)
}

// moduleShortName is a module's name without its collection.
func moduleShortName(module string) string {
	return module[strings.LastIndexByte(module, '.')+1:]
}
