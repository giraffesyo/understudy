package modules

import (
	"fmt"
	"os"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(fileModule, "file", "ansible.builtin.file")
}

var fileSpec = args.Spec{
	"path":    {Required: true, Aliases: []string{"dest", "name"}},
	"state":   {Choices: []string{"file", "touch", "absent", "directory", "link", "hard"}},
	"mode":    {Type: "any"},
	"owner":   {},
	"group":   {},
	"src":     {},
	"force":   {Type: "bool", Default: false},
	"recurse": {Type: "bool", Default: false},
	"follow":  {Type: "bool", Default: true},
}

// fileModule is an lstat-driven state machine: compute current state,
// compare with desired, mutate only on difference.
func fileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := fileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := p.Str("path")
	state := p.Str("state")

	info, lerr := os.Lstat(path)
	exists := lerr == nil

	// Default state: directory if it is one, else file.
	if state == "" {
		if exists && info.IsDir() {
			state = "directory"
		} else {
			state = "file"
		}
	}

	res := &agentproto.Result{Extra: map[string]any{"path": path, "state": state}}

	switch state {
	case "absent":
		if !exists {
			return res
		}
		if env.CheckMode {
			res.Changed = true
			return res
		}
		if err := os.RemoveAll(path); err != nil {
			return agentproto.Fail("failed to remove %s: %v", path, err)
		}
		res.Changed = true
		return res

	case "directory":
		if !exists {
			if env.CheckMode {
				res.Changed = true
				return res
			}
			mode := os.FileMode(0o755)
			if p.Has("mode") && fsutil.IsSymbolicMode(p.Any("mode")) {
				// Created umask-default like Ansible; applyAttrs then
				// resolves the symbolic mode against it.
				mode = 0o777
			} else if p.Has("mode") {
				if m, err := fsutil.ParseMode(p.Any("mode")); err == nil {
					mode = m
				} else {
					return agentproto.Fail("%v", err)
				}
			}
			if err := os.MkdirAll(path, mode); err != nil {
				return agentproto.Fail("failed to create directory %s: %v", path, err)
			}
			res.Changed = true
		} else if !info.IsDir() {
			return agentproto.Fail("%s exists and is not a directory (use state=absent first)", path)
		}
		return applyAttrs(env, p, path, res)

	case "file":
		if !exists {
			return agentproto.Fail("file %s does not exist (state=file does not create; use state=touch)", path)
		}
		return applyAttrs(env, p, path, res)

	case "touch":
		if env.CheckMode {
			res.Changed = true // touch always updates times
			return res
		}
		if !exists {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return agentproto.Fail("failed to touch %s: %v", path, err)
			}
			f.Close()
			res.Changed = true
		} else {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				return agentproto.Fail("failed to update times on %s: %v", path, err)
			}
			res.Changed = true
		}
		return applyAttrs(env, p, path, res)

	case "link", "hard":
		src := p.Str("src")
		if src == "" {
			return agentproto.Fail("state=%s requires 'src'", state)
		}
		if exists {
			if info.Mode()&os.ModeSymlink != 0 && state == "link" {
				current, err := os.Readlink(path)
				if err == nil && current == src {
					return res // already correct
				}
			}
			if !p.Bool("force") && info.Mode()&os.ModeSymlink == 0 {
				return agentproto.Fail("%s already exists and is not a link (use force=true)", path)
			}
			if env.CheckMode {
				res.Changed = true
				return res
			}
			if err := os.Remove(path); err != nil {
				return agentproto.Fail("failed to replace %s: %v", path, err)
			}
		} else if env.CheckMode {
			res.Changed = true
			return res
		}
		var linkErr error
		if state == "link" {
			linkErr = os.Symlink(src, path)
		} else {
			linkErr = os.Link(src, path)
		}
		if linkErr != nil {
			return agentproto.Fail("failed to create %s: %v", state, linkErr)
		}
		res.Changed = true
		return res
	}
	return agentproto.Fail("unknown state %q", state)
}

// applyAttrs applies mode/owner/group and merges the changed flag.
func applyAttrs(env *RunEnv, p *args.Parsed, path string, res *agentproto.Result) *agentproto.Result {
	if !p.Has("mode") && p.Str("owner") == "" && p.Str("group") == "" {
		return res
	}
	if env.CheckMode {
		// Conservative: report would-change only if attrs differ.
		changed, err := attrsWouldChange(p, path)
		if err == nil && changed {
			res.Changed = true
		}
		return res
	}
	var mode any
	if p.Has("mode") {
		mode = p.Any("mode")
	}
	changed, err := fsutil.ApplyFileAttrs(path, mode, p.Str("owner"), p.Str("group"), p.Bool("follow"))
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	if changed {
		res.Changed = true
	}
	return res
}

func attrsWouldChange(p *args.Parsed, path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if p.Has("mode") {
		want, err := fsutil.ResolveMode(p.Any("mode"), info.Mode())
		if err != nil {
			return false, err
		}
		if info.Mode().Perm() != want.Perm() {
			return true, nil
		}
	}
	// Owner/group comparison would need the same stat plumbing; be
	// conservative and report unchanged when only names match up.
	return false, nil
}

var _ = fmt.Sprintf // reserved
