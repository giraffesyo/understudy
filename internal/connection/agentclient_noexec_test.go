package connection

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// noexecConn runs commands locally as the login user (become is
// ignored), with a setfacl stub from bin first on PATH. An agent run is
// recorded and answered; test -x fails under the noexec dirs, and for
// the cached agent under become when the become user cannot run it.
type noexecConn struct {
	*Local
	bin      string
	noexec   []string
	agent    string
	inPlace  bool
	commands []string
	runs     []string
}

func (c *noexecConn) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	c.commands = append(c.commands, cmd)
	if strings.HasSuffix(cmd, " run") {
		c.runs = append(c.runs, strings.TrimSuffix(cmd, " run"))
		var out bytes.Buffer
		if err := agentproto.WriteResult(&out, &agentproto.Result{Msg: "ran"}); err != nil {
			return ExecResult{}, err
		}
		return ExecResult{Stdout: out.Bytes()}, nil
	}
	if path, ok := strings.CutPrefix(cmd, "test -x "); ok {
		for _, d := range c.noexec {
			if strings.HasPrefix(path, ShellQuote(d+"/")) || strings.HasPrefix(path, "'"+d+"/") {
				return ExecResult{RC: 1}, nil
			}
		}
		if opts.Become != nil && path == ShellQuote(c.agent) && !c.inPlace {
			return ExecResult{RC: 1}, nil
		}
	}
	opts.Become = nil
	if c.bin != "" {
		cmd = "PATH=" + ShellQuote(c.bin) + ":$PATH; export PATH; " + cmd
	}
	return c.Local.Exec(ctx, cmd, opts)
}

// ran counts the commands that contain every one of subs.
func (c *noexecConn) ran(subs ...string) int {
	n := 0
	for _, cmd := range c.commands {
		all := true
		for _, sub := range subs {
			all = all && strings.Contains(cmd, sub)
		}
		if all {
			n++
		}
	}
	return n
}

type noexecSetup struct {
	conn   *noexecConn
	client *AgentClient
	become *BecomeSpec
	base   string
	home   string
}

func newNoexecSetup(t *testing.T, noexecBase, inPlace bool) noexecSetup {
	t.Helper()
	agent := filepath.Join(t.TempDir(), "agent-0123456789ab-linux-amd64")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base, home, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(bin, "setfacl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	conn := &noexecConn{Local: NewLocal(), bin: bin, agent: agent, inPlace: inPlace}
	if noexecBase {
		conn.noexec = []string{base}
	}
	client := &AgentClient{Conn: conn, AgentPath: agent, Login: &LoginInfo{User: "deploy"},
		AgentFile: func() (string, io.ReadCloser, error) {
			return "agent-fedcba987654-linux-amd64", io.NopCloser(strings.NewReader("#!/bin/sh\n# home copy\n")), nil
		}}
	become := &BecomeSpec{User: "app", Shell: &ShellOptions{RemoteUser: "deploy", SystemTmpdirs: []string{base}}}
	return noexecSetup{conn, client, become, base, home}
}

func (s noexecSetup) run(t *testing.T, pipelined bool) {
	t.Helper()
	req := &agentproto.TaskRequest{Module: "command", Pipelined: pipelined}
	res, err := s.client.Run(t.Context(), req, nil, s.become)
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg != "ran" {
		t.Fatalf("result %+v", res)
	}
}

// A pipelined module's agent is staged under system_tmpdirs and runs
// from there when the mount allows execution.
func TestBecomeAgentStagedRuns(t *testing.T) {
	s := newNoexecSetup(t, false, false)
	s.run(t, true)
	s.run(t, true)
	for _, run := range s.conn.runs {
		if !strings.HasPrefix(run, "'"+s.base+"/understudy-") || !strings.HasSuffix(run, "/agent'") {
			t.Errorf("ran %s, want the agent staged under %s", run, s.base)
		}
	}
	if n := s.conn.ran("test -x"); n != 1 {
		t.Errorf("checked execution %d times, want once per temp dir", n)
	}
}

// Under a noexec system temp dir, the become user runs its own copy of
// the agent in its home, installed once, for pipelined and staged
// modules alike.
func TestBecomeAgentNoexecHomeCopy(t *testing.T) {
	for _, pipelined := range []bool{true, false} {
		s := newNoexecSetup(t, true, false)
		s.run(t, pipelined)
		s.run(t, pipelined)
		want := ShellQuote(filepath.Join(s.home, ".understudy", "agent-fedcba987654-linux-amd64"))
		if len(s.conn.runs) != 2 || s.conn.runs[0] != want || s.conn.runs[1] != want {
			t.Errorf("pipelined=%v: ran %q, want %s twice", pipelined, s.conn.runs, want)
		}
		got, err := os.ReadFile(filepath.Join(s.home, ".understudy", "agent-fedcba987654-linux-amd64"))
		if err != nil || string(got) != "#!/bin/sh\n# home copy\n" {
			t.Errorf("pipelined=%v: home copy %q, %v", pipelined, got, err)
		}
		if n := s.conn.ran("cat > \"$f.upload\""); n != 1 {
			t.Errorf("pipelined=%v: installed %d times, want once", pipelined, n)
		}
		if n := s.conn.ran("cp ", s.client.AgentPath); n != 1 {
			t.Errorf("pipelined=%v: staged the agent %d times, want only before the temp dir was known noexec", pipelined, n)
		}
	}
}

// A become user that can run the cached agent runs it where it is.
func TestBecomeAgentNoexecInPlace(t *testing.T) {
	s := newNoexecSetup(t, true, true)
	s.run(t, true)
	if want := ShellQuote(s.client.AgentPath); len(s.conn.runs) != 1 || s.conn.runs[0] != want {
		t.Errorf("ran %q, want %s", s.conn.runs, want)
	}
	if _, err := os.Stat(filepath.Join(s.home, ".understudy")); !os.IsNotExist(err) {
		t.Errorf("installed a home copy: %v", err)
	}
}

// With nowhere to run the agent, the error says what to configure.
func TestBecomeAgentNoexecNowhere(t *testing.T) {
	s := newNoexecSetup(t, true, false)
	s.client.AgentFile = nil
	_, err := s.client.Run(t.Context(), &agentproto.TaskRequest{Module: "command", Pipelined: true}, nil, s.become)
	if err == nil || !strings.Contains(err.Error(), s.base+" does not allow execution") || !strings.Contains(err.Error(), "set system_tmpdirs") {
		t.Errorf("err = %v", err)
	}
}
