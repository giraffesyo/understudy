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

		// Ansible reports cmd as the argv list for command, the string
		// for shell.
		var cmdField any = cmdline
		if !shell {
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
			list := make([]any, len(argv))
			for i, a := range argv {
				list[i] = a
			}
			cmdField = list
		}

		// creates/removes idempotence guards.
		notRun := func(msg, stdout string) *agentproto.Result {
			return &agentproto.Result{
				Msg:    msg,
				RC:     agentproto.IntPtr(0),
				Stdout: stdout,
				Extra: map[string]any{
					"cmd": cmdField, "start": nil, "end": nil, "delta": nil,
				},
			}
		}
		if creates != "" {
			if _, err := os.Stat(expandPath(creates, chdir)); err == nil {
				return notRun(fmt.Sprintf("Did not run command since '%s' exists", creates),
					fmt.Sprintf("skipped, since %s exists", creates))
			}
		}
		if removes != "" {
			if _, err := os.Stat(expandPath(removes, chdir)); err != nil {
				return notRun(fmt.Sprintf("Did not run command since '%s' does not exist", removes),
					fmt.Sprintf("skipped, since %s does not exist", removes))
			}
		}

		if env.CheckMode {
			// Partial check-mode support, as in Ansible: without a
			// creates/removes guard the command is skipped.
			res := &agentproto.Result{
				Changed: true,
				RC:      agentproto.IntPtr(0),
				Msg:     "Command would have run if not in check mode",
				Extra:   map[string]any{"cmd": cmdField, "start": nil, "end": nil, "delta": nil},
			}
			if creates == "" && removes == "" {
				res.Changed = false
				res.Skipped = true
			}
			return res
		}

		var cmd *exec.Cmd
		if shell {
			sh := "/bin/sh"
			if v, ok := argString(args, "executable"); ok && v != "" {
				sh = v
			}
			cmd = exec.Command(sh, "-c", cmdline)
		} else {
			path, err := lookPath(argv[0])
			if err != nil {
				// run_command's OSError branch: Popen could not exec it.
				quoted := make([]string, len(argv))
				for i, a := range argv {
					quoted[i] = shQuote(a)
				}
				res := agentproto.Fail("Error executing command.")
				res.Cause = "[Errno 2] No such file or directory: " + pyBytesRepr(argv[0])
				res.RC = agentproto.IntPtr(2)
				res.Extra = map[string]any{"cmd": strings.Join(quoted, " ")}
				return res
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
		end := time.Now()

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
				"cmd":   cmdField,
				"start": pyDatetime(start),
				"end":   pyDatetime(end),
				"delta": pyTimedelta(end.Sub(start)),
				"msg":   "",
			},
		}
		if rc != 0 {
			res.Failed = true
			res.Msg = "The command exited with a non-zero return code."
		}
		return res
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

// pyDatetime formats like Python's str(datetime.datetime.now()).
func pyDatetime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05.000000")
}

// pyTimedelta formats like Python's str(timedelta): "H:MM:SS.ffffff".
func pyTimedelta(d time.Duration) string {
	us := d.Microseconds()
	h := us / 3_600_000_000
	us -= h * 3_600_000_000
	m := us / 60_000_000
	us -= m * 60_000_000
	s := us / 1_000_000
	us -= s * 1_000_000
	if us == 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d:%02d.%06d", h, m, s, us)
}
