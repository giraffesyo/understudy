package executor

import (
	"os"
	"strings"

	"github.com/giraffesyo/understudy/internal/connection"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
)

// Pipelining is ActionBase._is_pipelining_enabled: the connection's
// pipelining option (ssh's and local's), not for async tasks, nor through
// a become method that cannot pipeline (su), nor over ssh with a tty
// requested (-t). A pipelined module is fed to the interpreter on stdin:
// no temporary directory is made for it, so it makes its own under
// remote_tmp when it needs one, and an unprivileged become user needs no
// files made readable to it. An action that transfers files still makes
// its temporary directory.

// pipelined reports whether a module the task runs is pipelined.
func (r *Runner) pipelined(vctx *vars.Context, task *playbook.Task, local bool, become *connection.BecomeSpec, async bool) bool {
	if async || transfersFiles[task.Module] { // their action makes its temporary directory
		return false
	}
	if become != nil && become.Method == "su" {
		return false
	}
	varNames := []string{"ansible_pipelining"}
	env := []string{"ANSIBLE_PIPELINING"}
	ini := []string{"defaults.pipelining", "connection.pipelining"}
	if !local {
		varNames = append(varNames, "ansible_ssh_pipelining")
		env = append(env, "ANSIBLE_SSH_PIPELINING")
		ini = append(ini, "ssh_connection.pipelining")
		if sshTTYRequested(vctx) {
			return false
		}
	}
	var value any
	found := false
	for _, n := range varNames {
		if v, ok := vctx.Get(n); ok {
			if tv, err := vctx.TemplateValue(v); err == nil {
				v = tv
			}
			value, found = v, true
		}
	}
	if !found && r.Opts.PluginOption != nil {
		if v, _, ok := r.Opts.PluginOption(env, ini); ok {
			value, found = v, true
		}
	}
	if !found {
		for _, e := range env {
			if v, ok := os.LookupEnv(e); ok {
				value, found = v, true
			}
		}
	}
	return found && pyBoolean(value)
}

// sshTTYRequested is the ssh connection's _is_tty_requested: -t among
// its ssh_args, ssh_common_args or ssh_extra_args.
func sshTTYRequested(vctx *vars.Context) bool {
	var args []string
	for _, n := range []string{"ansible_ssh_args", "ansible_ssh_common_args", "ansible_ssh_extra_args"} {
		if v, ok := vctx.Get(n); ok && v != nil {
			if tv, err := vctx.TemplateValue(v); err == nil {
				v = tv
			}
			args = append(args, strings.Fields(template.PyStr(v))...)
		}
	}
	for _, e := range []string{"ANSIBLE_SSH_ARGS", "ANSIBLE_SSH_COMMON_ARGS", "ANSIBLE_SSH_EXTRA_ARGS"} {
		args = append(args, strings.Fields(os.Getenv(e))...)
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Trim(a[1:], "t") == "" && len(a) > 1 {
			return true
		}
	}
	return false
}

// pyBoolean is boolean(value, strict=False).
func pyBoolean(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "y", "yes", "on", "1", "true", "t", "1.0":
			return true
		}
	case int64:
		return t == 1
	case int:
		return t == 1
	case float64:
		return t == 1
	}
	return false
}

// ensureLocalRemoteTmp is _make_tmp_path's first step for a module run on
// the local connection without pipelining: remote_tmp is made (mode
// 0700), as the module's temporary directory goes there.
func ensureLocalRemoteTmp(remoteTmp string) {
	if remoteTmp == "" {
		return
	}
	if remoteTmp == "~" || strings.HasPrefix(remoteTmp, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		remoteTmp = home + remoteTmp[1:]
	}
	// umask 77 && mkdir -p: every directory made is 0700.
	os.MkdirAll(os.ExpandEnv(remoteTmp), 0o700)
}
