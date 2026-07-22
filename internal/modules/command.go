package modules

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

func init() {
	Register(mkCommand(false), "command", "ansible.builtin.command")
	Register(mkCommand(true), "shell", "ansible.builtin.shell")
}

// mkCommand builds the command/shell module. shell runs through `sh -c`;
// command execs the argv directly after shlex-splitting.
func mkCommand(shell bool) ModuleFunc {
	return func(env *RunEnv, args map[string]any) *agentproto.Result {
		cmdline := env.FreeForm
		if v, ok := argString(args, "cmd"); ok {
			cmdline = v
		}
		var argv []string
		if v, ok := args["argv"]; ok {
			list, isList := v.([]any)
			if !isList {
				return agentproto.Fail("argv must be a list")
			}
			for _, item := range list {
				argv = append(argv, fmt.Sprintf("%v", item))
			}
		}
		if cmdline == "" && len(argv) == 0 {
			return agentproto.Fail("no command given")
		}
		if cmdline != "" && len(argv) > 0 {
			return agentproto.Fail("only one of cmd/free-form and argv may be given")
		}

		chdir, _ := argString(args, "chdir")
		creates, _ := argString(args, "creates")
		removes, _ := argString(args, "removes")
		stdinStr, hasStdin := argString(args, "stdin")

		// creates/removes idempotence guards.
		if creates != "" {
			if _, err := os.Stat(expandPath(creates, chdir)); err == nil {
				return &agentproto.Result{
					Msg:   fmt.Sprintf("Did not run command since %q exists", creates),
					RC:    agentproto.IntPtr(0),
					Extra: map[string]any{"cmd": cmdline, "stdout_lines": []string{}},
				}
			}
		}
		if removes != "" {
			if _, err := os.Stat(expandPath(removes, chdir)); err != nil {
				return &agentproto.Result{
					Msg:   fmt.Sprintf("Did not run command since %q does not exist", removes),
					RC:    agentproto.IntPtr(0),
					Extra: map[string]any{"cmd": cmdline},
				}
			}
		}

		if env.CheckMode {
			return &agentproto.Result{
				Skipped: true,
				Msg:     "remote module (command) does not support check mode",
			}
		}

		if env.Background {
			return launchDetached(env, shell, cmdline, argv, chdir)
		}

		var cmd *exec.Cmd
		if shell {
			sh := "/bin/sh"
			if v, ok := argString(args, "executable"); ok && v != "" {
				sh = v
			}
			cmd = exec.Command(sh, "-c", cmdline)
		} else {
			if len(argv) == 0 {
				var err error
				argv, err = shlexSplit(cmdline)
				if err != nil {
					return agentproto.Fail("failed to parse command: %v", err)
				}
				if len(argv) == 0 {
					return agentproto.Fail("no command given")
				}
			}
			path, err := exec.LookPath(argv[0])
			if err != nil {
				return agentproto.Fail("Cannot find command %q: %v", argv[0], err)
			}
			cmd = exec.Command(path, argv[1:]...)
		}
		if chdir != "" {
			cmd.Dir = chdir
		}
		if len(env.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range env.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		if hasStdin {
			if nl, err := argBool(args, "stdin_add_newline", true); err == nil && nl && !strings.HasSuffix(stdinStr, "\n") {
				stdinStr += "\n"
			}
			cmd.Stdin = strings.NewReader(stdinStr)
		}

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		start := time.Now()
		err := cmd.Run()
		delta := time.Since(start)

		rc := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else {
				return agentproto.Fail("failed to run command: %v", err)
			}
		}

		res := &agentproto.Result{
			Changed: true,
			RC:      agentproto.IntPtr(rc),
			Stdout:  strings.TrimRight(stdout.String(), "\r\n"),
			Stderr:  strings.TrimRight(stderr.String(), "\r\n"),
			Extra: map[string]any{
				"cmd":   cmdline,
				"delta": fmt.Sprintf("%f", delta.Seconds()),
			},
		}
		if rc != 0 {
			res.Failed = true
			res.Msg = "non-zero return code"
		}
		return res
	}
}

// launchDetached implements fire-and-forget async (poll: 0): the process
// starts in its own session, survives the agent's exit, and the task
// returns immediately with started=1.
func launchDetached(env *RunEnv, shell bool, cmdline string, argv []string, chdir string) *agentproto.Result {
	var cmd *exec.Cmd
	if shell {
		cmd = exec.Command("/bin/sh", "-c", cmdline)
	} else {
		if len(argv) == 0 {
			var err error
			argv, err = shlexSplit(cmdline)
			if err != nil || len(argv) == 0 {
				return agentproto.Fail("failed to parse command: %v", err)
			}
		}
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return agentproto.Fail("Cannot find command %q: %v", argv[0], err)
		}
		cmd = exec.Command(path, argv[1:]...)
	}
	if chdir != "" {
		cmd.Dir = chdir
	}
	if len(env.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return agentproto.Fail("failed to start background command: %v", err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if we're still alive; harmless otherwise
	return &agentproto.Result{
		Changed: true,
		Extra: map[string]any{
			"started":        1,
			"finished":       0,
			"ansible_job_id": fmt.Sprintf("%d.%d", time.Now().Unix(), pid),
		},
	}
}

func expandPath(p, chdir string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	if chdir != "" && !strings.HasPrefix(p, "/") {
		p = chdir + "/" + p
	}
	return p
}

// shlexSplit splits a command line the way POSIX shells tokenize: spaces
// separate, single/double quotes group, backslash escapes.
func shlexSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unbalanced single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case '"', '\\', '$', '`':
						i++
					}
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unbalanced double quote")
			}
		case c == '\\' && i+1 < len(s):
			inWord = true
			cur.WriteByte(s[i+1])
			i++
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
