package modules

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

func init() {
	Register(scriptModule, "script", "ansible.builtin.script")
}

// scriptModule receives a script as the frame payload, writes it to a temp
// file, and executes it with the given arguments. The control-side action
// resolves the local path and attaches the payload.
func scriptModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if env.CheckMode {
		return &agentproto.Result{Skipped: true, Msg: "remote module (script) does not support check mode"}
	}
	if env.Payload == nil {
		return agentproto.Fail("script: no script content attached (control action missing?)")
	}

	// creates/removes guards, same semantics as command.
	if creates, ok := argString(rawArgs, "creates"); ok && creates != "" {
		if _, err := os.Stat(creates); err == nil {
			return &agentproto.Result{Msg: fmt.Sprintf("Did not run script since %q exists", creates)}
		}
	}
	if removes, ok := argString(rawArgs, "removes"); ok && removes != "" {
		if _, err := os.Stat(removes); err != nil {
			return &agentproto.Result{Msg: fmt.Sprintf("Did not run script since %q does not exist", removes)}
		}
	}

	tmp, err := os.CreateTemp("", "understudy-script-*")
	if err != nil {
		return agentproto.Fail("script: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, env.Payload); err != nil {
		tmp.Close()
		return agentproto.Fail("script: %v", err)
	}
	if err := tmp.Chmod(0o700); err != nil {
		tmp.Close()
		return agentproto.Fail("script: %v", err)
	}
	tmp.Close()

	var cmd *exec.Cmd
	scriptArgs, _ := argString(rawArgs, "_args")
	if executable, ok := argString(rawArgs, "executable"); ok && executable != "" {
		argv := append([]string{tmp.Name()}, strings.Fields(scriptArgs)...)
		cmd = exec.Command(executable, argv...)
	} else if scriptArgs != "" {
		cmd = exec.Command("/bin/sh", "-c", tmp.Name()+" "+scriptArgs)
	} else {
		cmd = exec.Command(tmp.Name())
	}
	if chdir, ok := argString(rawArgs, "chdir"); ok && chdir != "" {
		cmd.Dir = chdir
	}
	if len(env.Env) > 0 {
		cmd.Env = env.Environ()
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			return agentproto.Fail("script failed to start: %v", err)
		}
	}
	res := &agentproto.Result{
		Changed: true,
		RC:      agentproto.IntPtr(rc),
		Stdout:  strings.TrimRight(stdout.String(), "\r\n"),
		Stderr:  strings.TrimRight(stderr.String(), "\r\n"),
	}
	if rc != 0 {
		res.Failed = true
		res.Msg = "non-zero return code"
	}
	return res
}
