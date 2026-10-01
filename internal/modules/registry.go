// Package modules implements the task modules compiled into both the remote
// agent and the control binary (which runs them in-process for local
// connections). It must depend only on the stdlib and agentproto — the
// Makefile's depcheck enforces this so the agent stays small.
package modules

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// sbinDirs are searched in addition to PATH when resolving a binary.
// System tools (sysctl, iptables, setenforce, useradd, ...) live here, and
// they are often absent from a minimal or sudo-restricted PATH.
var sbinDirs = []string{"/usr/sbin", "/sbin", "/usr/local/sbin"}

// lookPath resolves a command like exec.LookPath but falls back to the
// conventional sbin directories, mirroring how Ansible finds system tools.
func lookPath(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	if !filepath.IsAbs(name) {
		for _, dir := range sbinDirs {
			candidate := filepath.Join(dir, name)
			if info, err := exec.LookPath(candidate); err == nil {
				return info, nil
			}
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH or %v", name, sbinDirs)
}

// RunEnv carries per-invocation context into a module.
type RunEnv struct {
	CheckMode  bool
	DiffMode   bool
	Payload    io.Reader         // exactly PayloadLen bytes, or nil
	FreeForm   string            // raw params for command/shell/script
	Env        map[string]string // task environment: applied to shell-outs
	EnvOrder   []string          // Env's variables in the task's order
	Background bool              // fire-and-forget (async + poll: 0)

	// PythonInterpreter is the task's ansible_python_interpreter ("" for
	// discovery): output that depends on the target's Python follows it.
	PythonInterpreter string
	// PythonFallback is ansible_interpreter_python_fallback, the list
	// discovery tries (nil: INTERPRETER_PYTHON_FALLBACK's default).
	PythonFallback []string
	// DiscoveryPath is the PATH discovery searches: the login user's,
	// when the module runs as another user ("": this process's).
	DiscoveryPath string
	// LoginHome, LoginUID and LoginGID describe the login user when the
	// module runs as another one (LoginHome "" otherwise).
	LoginHome          string
	LoginUID, LoginGID int
	// StageDir is where an unprivileged become user's transferred files
	// are reported (see transferDir).
	StageDir string
	// ModuleRemoteTmp is remote_tmp when the module got no tmpdir of its
	// own (an unprivileged become user): see moduleTmpdir.
	ModuleRemoteTmp string
	// PkgShim: the package managers the module's commands run take the
	// package lock (a task in a parallel block).
	PkgShim bool

	shimDir     string
	modTmp      string
	modTmpMade  bool
	modWarnings []string

	// Ctx is the run's context (nil = never cancelled): a task that
	// timed out cancels it, and the processes the module started
	// through Command are killed.
	Ctx context.Context
}

// Context is the run's context.
func (env *RunEnv) Context() context.Context {
	if env == nil || env.Ctx == nil {
		return context.Background()
	}
	return env.Ctx
}

// Command is exec.Command bound to the run's context: when the run is
// cancelled (its task timed out and was abandoned, as a worker is
// terminated) the process is killed, with every process it started in
// turn (it leads its own process group), rather than left running.
func (env *RunEnv) Command(name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(env.Context(), name, args...)
	ownGroup(cmd)
	return cmd
}

// Environ is a shell-out's environment under the task's environment
// keyword: the task's variables first, in the task's order (ansible-core
// prefixes them to the module's command line, so they lead its
// environment), then this process's other variables. extra (a module's
// own environ_update) then updates it as a dict update does: a variable
// already set keeps its place.
func (env *RunEnv) Environ(extra ...map[string]string) []string {
	var out []string
	at := map[string]int{}
	set := func(k, v string) {
		if i, ok := at[k]; ok {
			out[i] = k + "=" + v
			return
		}
		at[k] = len(out)
		out = append(out, k+"="+v)
	}
	for _, k := range env.EnvOrder {
		if v, ok := env.Env[k]; ok {
			set(k, v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(env.Env)) {
		if _, done := at[k]; !done {
			set(k, env.Env[k])
		}
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if _, done := at[k]; !done {
			set(k, v)
		}
	}
	for _, m := range extra {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			set(k, m[k])
		}
	}
	return out
}

// ModuleFunc executes one module invocation. Failures are reported in the
// Result, never as a Go error — the process exit code is reserved for
// infrastructure problems.
type ModuleFunc func(env *RunEnv, args map[string]any) *agentproto.Result

var registry = map[string]ModuleFunc{}

// Register installs a module under one or more names.
func Register(fn ModuleFunc, names ...string) {
	for _, n := range names {
		registry[n] = fn
	}
}

// Names returns whether a module exists (used by playbook validation).
func Exists(name string) bool {
	_, ok := registry[name]
	return ok
}

// Run dispatches a request to its module, converting panics into failed
// results so a module bug cannot take down the agent mid-frame.
func Run(req *agentproto.TaskRequest, payload io.Reader) *agentproto.Result {
	return RunContext(context.Background(), req, payload)
}

// RunContext is Run under a context: cancelling it kills the processes
// the module runs.
func RunContext(ctx context.Context, req *agentproto.TaskRequest, payload io.Reader) (res *agentproto.Result) {
	fn, ok := registry[req.Module]
	if !ok {
		return agentproto.Fail("unknown module %q", req.Module)
	}
	if req.Background {
		return StartAsync(req, payload)
	}
	defer func() {
		if r := recover(); r != nil {
			res = agentproto.Fail("module %s panicked: %v\n%s", req.Module, r, debug.Stack())
		}
	}()
	env := &RunEnv{
		CheckMode:  req.CheckMode,
		DiffMode:   req.Diff,
		Payload:    payload,
		FreeForm:   req.FreeForm,
		Env:        req.Env,
		EnvOrder:   req.EnvOrder,
		Background: req.Background,

		PythonInterpreter: req.PythonInterpreter,
		PythonFallback:    req.PythonFallback,
		DiscoveryPath:     req.DiscoveryPath,
		LoginHome:         req.LoginHome,
		LoginUID:          req.LoginUID,
		LoginGID:          req.LoginGID,
		StageDir:          req.StageDir,
		ModuleRemoteTmp:   req.ModuleRemoteTmp,
		PkgShim:           req.PkgShim,
		Ctx:               ctx,
	}
	env.moduleSetCwd()
	_, copyAction := req.Args[copyActionKey]
	args := req.Args
	var aliasWarnings []string
	if spec, ok := specs[req.Module]; ok {
		// An option set along with its alias: the alias wins, with a
		// warning (AnsibleModule's argument validation).
		if aliasWarnings = spec.AliasWarnings(args); len(aliasWarnings) > 0 {
			args = spec.ResolveAliases(args)
		}
	}
	res = fn(env, args)
	if res == nil {
		res = agentproto.Fail("module %s returned no result", req.Module)
	}
	env.moduleCleanup(res)
	if env.shimDir != "" {
		os.RemoveAll(env.shimDir)
	}
	if len(aliasWarnings) > 0 {
		warnings := make([]any, 0, len(aliasWarnings))
		for _, w := range aliasWarnings {
			warnings = append(warnings, w)
		}
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		prior, _ := res.Extra["warnings"].([]any)
		res.Extra["warnings"] = append(warnings, prior...)
	}
	if pathInfoModules[req.Module] && !res.Skipped && !copyAction {
		addPathInfo(res)
	}
	return res
}

// argString fetches an optional string arg, coercing scalars the way
// Ansible does.
func argString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "yes", true
		}
		return "no", true
	default:
		return fmt.Sprintf("%v", t), true
	}
}

// argBool fetches an optional boolean arg with Ansible's string coercions.
func argBool(args map[string]any, key string, def bool) (bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "yes", "on", "1", "true", "True", "TRUE", "Yes", "YES":
			return true, nil
		case "no", "off", "0", "false", "False", "FALSE", "No", "NO":
			return false, nil
		}
		return false, fmt.Errorf("%s: %q is not a valid boolean", key, t)
	case int64:
		return t != 0, nil
	case float64:
		return t != 0, nil
	}
	return false, fmt.Errorf("%s: cannot interpret %T as a boolean", key, v)
}
