// Package modules implements the task modules compiled into both the remote
// agent and the control binary (which runs them in-process for local
// connections). It must depend only on the stdlib and agentproto — the
// Makefile's depcheck enforces this so the agent stays small.
package modules

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime/debug"

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
	Background bool              // fire-and-forget (async + poll: 0)
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
func Run(req *agentproto.TaskRequest, payload io.Reader) (res *agentproto.Result) {
	fn, ok := registry[req.Module]
	if !ok {
		return agentproto.Fail("unknown module %q", req.Module)
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
		Background: req.Background,
	}
	res = fn(env, req.Args)
	if res == nil {
		res = agentproto.Fail("module %s returned no result", req.Module)
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
