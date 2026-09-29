package actions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/template"
)

func init() {
	Register("copy", actionFunc(runCopy))
	Register("template", actionFunc(runTemplate))
}

// runCopy reads the local src (or inline content), then forwards the bytes
// to the target's copy module as the frame payload.
func runCopy(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	var content []byte
	original := ""

	if err := checkFileArgs("copy", args); err != nil {
		return agentproto.Fail("%v", err)
	}
	if rs, _ := args["remote_src"].(bool); rs || args["remote_src"] == "yes" || args["remote_src"] == "true" {
		// The source lives on the target: the module reads it there.
		return forwardToCopy(ctx, actx, args, nil, "")
	}

	if c, ok := args["content"]; ok {
		switch t := c.(type) {
		case string:
			content = []byte(t)
		default:
			return agentproto.Fail("copy: 'content' must be a string (got %T)", c)
		}
	} else if src, ok := args["src"].(string); ok && src != "" {
		resolved := resolveSrc(actx, src, "files")
		if resolved == "" {
			return agentproto.Fail("copy: could not find src %q (searched relative to the playbook and its files/ directory)", src)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return agentproto.Fail("copy: %v", err)
		}
		if info.IsDir() {
			return copyLocalTree(ctx, actx, args, src, resolved)
		}
		content, err = os.ReadFile(resolved)
		if err != nil {
			return agentproto.Fail("copy: %v", err)
		}
		original = resolved
	} else {
		return agentproto.Fail("copy requires 'src' or 'content'")
	}

	return forwardToCopy(ctx, actx, args, content, original)
}

// runTemplate renders the local template with host vars, then places the
// rendered bytes via the copy module (identical change semantics).
func runTemplate(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	if err := checkFileArgs("template", args); err != nil {
		return agentproto.Fail("%v", err)
	}
	src, ok := args["src"].(string)
	if !ok || src == "" {
		return agentproto.Fail("template requires 'src'")
	}
	resolved := resolveSrc(actx, src, "templates")
	if resolved == "" {
		return agentproto.Fail("template: could not find src %q (searched relative to the playbook and its templates/ directory)", src)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return agentproto.Fail("template: %v", err)
	}

	tvars := actx.Vars.WithOverlay(map[string]any{
		"template_path":   resolved,
		"template_host":   actx.Host,
		"ansible_managed": "Ansible managed",
	})
	pos := template.Position{File: resolved, Line: 1, Col: 1}
	// Ansible's template search path: the template's own directory, then
	// the role's and the playbook's templates directories.
	searchPath := []string{filepath.Dir(resolved)}
	if actx.SrcDir != "" {
		searchPath = append(searchPath, filepath.Join(actx.SrcDir, "templates"), actx.SrcDir)
	}
	searchPath = append(searchPath, filepath.Join(actx.BaseDir, "templates"), actx.BaseDir)
	rendered, err := tvars.RenderFile(string(raw), pos, searchPath...)
	if err != nil {
		return agentproto.Fail("template: error rendering %s: %v", src, err)
	}

	return forwardToCopy(ctx, actx, args, []byte(rendered), resolved)
}

// forwardToCopy builds the copy-module request with the payload attached.
func forwardToCopy(ctx context.Context, actx *Context, args map[string]any, content []byte, original string) *agentproto.Result {
	fwd := make(map[string]any, len(args)+2)
	for k, v := range args {
		if forwardedFileArgs[k] {
			fwd[k] = v
		}
	}
	remote := content == nil && original == ""
	if !remote {
		delete(fwd, "src")
		delete(fwd, "remote_src")
		fwd["_checksum"] = fsutil.Sha256Bytes(content)
		fwd["_original"] = original
	}

	req := &agentproto.TaskRequest{
		Proto:      agentproto.ProtoVersion,
		Op:         "task",
		Module:     "copy",
		Args:       fwd,
		CheckMode:  actx.CheckMode,
		Diff:       actx.Diff,
		PayloadLen: int64(len(content)),
	}
	res, err := actx.RunModule(ctx, req, bytes.NewReader(content))
	if err != nil {
		return agentproto.Fail("copy: %v", err)
	}
	return res
}

// resolveSrc finds a source file: absolute, role-relative (roles/x/files or
// roles/x/templates), playbook-relative, or under the conventional
// subdirectory.
func resolveSrc(actx *Context, src, subdir string) string {
	if filepath.IsAbs(src) {
		if _, err := os.Stat(src); err == nil {
			return src
		}
		return ""
	}
	var candidates []string
	if actx.SrcDir != "" {
		candidates = append(candidates,
			filepath.Join(actx.SrcDir, subdir, src),
			filepath.Join(actx.SrcDir, src))
	}
	candidates = append(candidates,
		filepath.Join(actx.BaseDir, subdir, src),
		filepath.Join(actx.BaseDir, src))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// forwardedFileArgs are the copy/template options the target-side copy
// module implements.
var forwardedFileArgs = map[string]bool{
	"src": true, "dest": true, "mode": true, "owner": true, "group": true,
	"backup": true, "force": true, "directory_mode": true, "validate": true,
	"follow": true, "unsafe_writes": true, "remote_src": true,
}

// fileActionArgs lists what each action accepts; anything else fails like
// Ansible's "Unsupported parameters" rather than being silently dropped.
var fileActionArgs = map[string]map[string]bool{
	"copy":     {"content": true, "local_follow": true, "decrypt": true},
	"template": {"newline_sequence": true, "trim_blocks": true, "lstrip_blocks": true},
}

func checkFileArgs(action string, args map[string]any) error {
	var bad []string
	for k := range args {
		if !forwardedFileArgs[k] && !fileActionArgs[action][k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("Unsupported parameters for (%s) module: %s", action, strings.Join(bad, ", "))
	}
	if action == "template" {
		// Ansible's defaults (trim_blocks on, lstrip_blocks off, \n) are the
		// engine's behavior; other values would silently render differently.
		if v, ok := args["trim_blocks"]; ok && !isTruthy(v) {
			return fmt.Errorf("template: trim_blocks: false is not supported yet")
		}
		if v, ok := args["lstrip_blocks"]; ok && isTruthy(v) {
			return fmt.Errorf("template: lstrip_blocks: true is not supported yet")
		}
		if v, ok := args["newline_sequence"]; ok && v != "\n" {
			return fmt.Errorf("template: newline_sequence other than \\n is not supported yet")
		}
	}
	return nil
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(t) {
		case "yes", "true", "on", "1", "y":
			return true
		}
	}
	return false
}

// copyLocalTree implements copy of a local directory: "dir/" copies its
// contents into dest, "dir" copies the directory itself (cp -r rules).
// Each file goes through the copy module; directories are created first.
func copyLocalTree(ctx context.Context, actx *Context, args map[string]any, src, resolved string) *agentproto.Result {
	dest, _ := args["dest"].(string)
	root := dest
	if !strings.HasSuffix(src, "/") {
		root = filepath.Join(dest, filepath.Base(resolved))
	}
	agg := &agentproto.Result{Extra: map[string]any{"dest": dest, "src": resolved}}
	err := filepath.WalkDir(resolved, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(resolved, path)
		target := filepath.Join(root, rel)
		if d.IsDir() {
			dirArgs := map[string]any{"path": target, "state": "directory"}
			if m, ok := args["directory_mode"]; ok {
				dirArgs["mode"] = m
			}
			for _, k := range []string{"owner", "group"} {
				if v, ok := args[k]; ok {
					dirArgs[k] = v
				}
			}
			res, err := actx.RunModule(ctx, &agentproto.TaskRequest{
				Proto: agentproto.ProtoVersion, Op: "task", Module: "file",
				Args: dirArgs, CheckMode: actx.CheckMode,
			}, nil)
			if err != nil {
				return err
			}
			if res.Failed {
				return fmt.Errorf("%s", res.Msg)
			}
			agg.Changed = agg.Changed || res.Changed
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileArgs := make(map[string]any, len(args))
		for k, v := range args {
			fileArgs[k] = v
		}
		fileArgs["dest"] = target
		res := forwardToCopy(ctx, actx, fileArgs, content, path)
		if res.Failed {
			return fmt.Errorf("%s", res.Msg)
		}
		agg.Changed = agg.Changed || res.Changed
		return nil
	})
	if err != nil {
		return agentproto.Fail("copy: %v", err)
	}
	return agg
}
