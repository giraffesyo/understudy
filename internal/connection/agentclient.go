package connection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// AgentClient executes module tasks by invoking the remote agent with a
// JSON frame on stdin and parsing the sentinel-guarded result from stdout.
type AgentClient struct {
	Conn      Connection
	AgentPath string
	// AgentArg, when set, is the argument that makes the binary at
	// AgentPath act as the agent (a control binary serving local
	// become).
	AgentArg string
	// Login is the login user (from the bootstrap probe), nil when
	// unknown.
	Login *LoginInfo

	tmpMu   sync.Mutex
	tmpMade map[string]bool // remote_tmp values made (as _make_tmp_path does)
}

// stagingModules are the modules whose payload is a file the action
// transfers next to the module (copy and template).
var stagingModules = map[string]bool{"copy": true, "template": true}

// Run executes one task through the agent.
func (c *AgentClient) Run(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader, become *BecomeSpec) (*agentproto.Result, error) {
	agentPath := ShellQuote(c.AgentPath)
	if c.AgentArg == "" {
		agentPath = c.AgentPath
	}
	if become != nil && c.Login != nil {
		// Under become the task still reports paths and finds Python as
		// the login user would: remote_tmp is under the login user's
		// home, interpreter discovery searches the login PATH.
		req.LoginHome, req.LoginUID, req.LoginGID = c.Login.Home, c.Login.UID, c.Login.GID
		req.DiscoveryPath = c.Login.Path
	}
	if !req.Pipelined {
		if res := c.unreadableAsAdmin(ctx, req, become); res != nil {
			return res, nil
		}
		if !becomeUnprivileged(become) {
			c.makeRemoteTmp(ctx, req.RemoteTmp)
		}
	}
	if req.Pipelined && become != nil && !c.agentReadableBy(become) {
		// A pipelined module needs no files made readable to the become
		// user, but the agent must still run as it: from a copy no other
		// file of the task's is next to (the frame comes on stdin).
		dir, err := c.publicAgent(ctx)
		if err != nil {
			return nil, err
		}
		defer c.Conn.Exec(context.WithoutCancel(ctx), "rm -f -r "+ShellQuote(dir)+" > /dev/null 2>&1", ExecOptions{})
		agentPath = ShellQuote(dir + "/agent")
	} else if becomeUnprivileged(become) && !req.Pipelined {
		// An unprivileged become user cannot reach the login user's
		// files: like a module's AnsiballZ payload, the agent runs from a
		// system temp dir made readable to it (the shell reports the dir
		// with a trailing slash). A transferred file is staged there by
		// the login user too, so the module meets it as ansible's does.
		dir, err := systemTmp(ctx, c.Conn, become.Shell)
		if err != nil {
			return nil, err
		}
		defer c.Conn.Exec(context.WithoutCancel(ctx), "rm -f -r "+ShellQuote(dir)+" > /dev/null 2>&1", ExecOptions{})
		mod := dir + "/AnsiballZ_" + moduleShortName(req.Module) + ".py"
		prep := "cp " + ShellQuote(c.AgentPath) + " " + ShellQuote(mod)
		paths := []string{dir + "/", mod}
		var stage io.Reader
		if payload != nil && req.PayloadLen > 0 && stagingModules[moduleShortName(req.Module)] {
			data, err := io.ReadAll(io.LimitReader(payload, req.PayloadLen))
			if err != nil {
				return nil, err
			}
			payload = bytes.NewReader(data)
			staged := dir + "/" + agentproto.StagedPayload
			prep += " && ( umask 77 && cat > " + ShellQuote(staged) + " )"
			paths = append(paths, staged)
			stage = bytes.NewReader(data)
		}
		if err := fixupPerms(ctx, c.Conn, prep, stage, paths, become); err != nil {
			return nil, err
		}
		req.StageDir = dir
		req.ModuleRemoteTmp = become.Shell.RemoteTmp
		if req.ModuleRemoteTmp == "" {
			req.ModuleRemoteTmp = "~/.ansible/tmp"
		}
		agentPath = ShellQuote(mod)
	}
	if c.AgentArg != "" {
		agentPath += " " + c.AgentArg
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
		// sudo prompted with no password to give: the connection raises
		// it as ansible-core's ssh plugin does ("Missing sudo password").
		if become != nil && become.method() == "sudo" && IsSudoPasswordError(res.Stderr) {
			return nil, &BecomeError{Msg: "Missing sudo password"}
		}
		return nil, fmt.Errorf("agent exited with rc=%d: %s", res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	return agentproto.ParseResult(res.Stdout)
}

// makeRemoteTmp makes remote_tmp as the login user, as _make_tmp_path
// does for every module that is not pipelined (once per value here).
func (c *AgentClient) makeRemoteTmp(ctx context.Context, remoteTmp string) {
	if remoteTmp == "" {
		return
	}
	c.tmpMu.Lock()
	defer c.tmpMu.Unlock()
	if c.tmpMade[remoteTmp] {
		return
	}
	cmd := "( umask 77 && mkdir -p \"`echo " + remoteTmp + "`\" )"
	if res, err := c.Conn.Exec(ctx, cmd, ExecOptions{}); err == nil && res.RC == 0 {
		if c.tmpMade == nil {
			c.tmpMade = map[string]bool{}
		}
		c.tmpMade[remoteTmp] = true
	}
}

// agentReadableBy reports whether the become user can run the cached
// agent (root, or the login user).
func (c *AgentClient) agentReadableBy(b *BecomeSpec) bool {
	user := b.user()
	if user == "root" || user == "0" || (b.Shell != nil && user == b.Shell.RemoteUser) {
		return true
	}
	return c.Login != nil && user == c.Login.User
}

// publicAgent copies the agent to a new directory any user can read
// (the agent is no secret: the task comes on its stdin).
func (c *AgentClient) publicAgent(ctx context.Context) (string, error) {
	cmd := `d=$(mktemp -d "${TMPDIR:-/tmp}/understudy-XXXXXX") && chmod 755 "$d" && cp ` + ShellQuote(c.AgentPath) +
		` "$d/agent" && chmod 755 "$d/agent" && echo "$d"`
	res, err := c.Conn.Exec(ctx, cmd, ExecOptions{})
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(res.Stdout))
	if res.RC != 0 || dir == "" {
		return "", fmt.Errorf("could not stage the agent for the become user: %s", strings.TrimSpace(string(res.Stderr)))
	}
	return dir, nil
}

// unreadableAsAdmin is a become user listed in admin_users that is
// neither root nor the login user: ansible treats it as privileged and
// stages the module in the login user's remote_tmp, a 0700 directory it
// cannot read, so Python fails to open the module and the action cannot
// read a result. That result, or nil for any other become.
func (c *AgentClient) unreadableAsAdmin(ctx context.Context, req *agentproto.TaskRequest, b *BecomeSpec) *agentproto.Result {
	if b == nil || b.Shell == nil || becomeUnprivileged(b) {
		return nil
	}
	user := b.user()
	if user == "root" || user == "0" || user == b.Shell.RemoteUser || (c.Login != nil && user == c.Login.User) {
		return nil
	}
	remoteTmp := b.Shell.RemoteTmp
	if remoteTmp == "" {
		remoteTmp = "~/.ansible/tmp"
	}
	if c.Login != nil && c.Login.Home != "" && (remoteTmp == "~" || strings.HasPrefix(remoteTmp, "~/")) {
		remoteTmp = c.Login.Home + remoteTmp[1:]
	}
	remoteTmp = strings.TrimRight(remoteTmp, "/")
	module := fmt.Sprintf("%s/ansible-tmp-%s-%d-%d/AnsiballZ_%s.py", remoteTmp, pyTime(time.Now()), os.Getpid(),
		rand.Int63n(1<<48), moduleShortName(req.Module))
	python := req.PythonInterpreter
	if python == "" {
		python = c.discoverPython(ctx, req.PythonFallback)
	}
	// Commands run on a terminal over ssh (newlines arrive as CRLF), and
	// OpenSSH reports the shared connection closing.
	eol, stderr := "\n", ""
	if n, ok := c.Conn.(interface{ closedNotice() string }); ok {
		eol, stderr = "\r\n", n.closedNotice()
	}
	msg := "Module result deserialization failed: No start of json char found"
	return &agentproto.Result{Failed: true, Msg: msg, Origin: "action",
		ErrorChain: &agentproto.ErrorChain{Outer: "Task failed: Action failed.", Inner: msg,
			Help: "See stdout/stderr for the returned output."},
		Extra: map[string]any{"rc": int64(2), "module_stderr": stderr,
			"module_stdout": fmt.Sprintf("%s: can't open file '%s': [Errno 13] Permission denied%s", python, module, eol)}}
}

// discoverPython is interpreter discovery's pick: the first of the
// fallback list found on the login user's PATH (/usr/bin/python3 when
// none is).
func (c *AgentClient) discoverPython(ctx context.Context, fallback []string) string {
	if len(fallback) == 0 {
		fallback = []string{"python3.14", "python3.13", "python3.12", "python3.11", "python3.10", "python3.9",
			"/usr/bin/python3", "python3"}
	}
	var script strings.Builder
	for _, p := range fallback {
		fmt.Fprintf(&script, "command -v %s; ", ShellQuote(p))
	}
	res, err := c.Conn.Exec(ctx, script.String(), ExecOptions{})
	if err == nil {
		for _, line := range strings.Split(string(res.Stdout), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return "/usr/bin/python3"
}

// moduleShortName is a module's name without its collection.
func moduleShortName(module string) string {
	return module[strings.LastIndexByte(module, '.')+1:]
}
