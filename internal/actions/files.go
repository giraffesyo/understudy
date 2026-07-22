package actions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

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
			return agentproto.Fail("copy: directory sources are not supported yet (src=%s)", src)
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
	rendered, err := tvars.RenderFile(string(raw), pos)
	if err != nil {
		return agentproto.Fail("template: error rendering %s: %v", src, err)
	}

	return forwardToCopy(ctx, actx, args, []byte(rendered), resolved)
}

// forwardToCopy builds the copy-module request with the payload attached.
func forwardToCopy(ctx context.Context, actx *Context, args map[string]any, content []byte, original string) *agentproto.Result {
	fwd := make(map[string]any, len(args)+2)
	for k, v := range args {
		switch k {
		case "src", "content", "backup", "dest", "mode", "owner", "group", "force", "directory_mode":
			fwd[k] = v
		case "validate", "variable_start_string", "variable_end_string",
			"block_start_string", "block_end_string", "trim_blocks", "lstrip_blocks", "newline_sequence":
			return agentproto.Fail("the %q option is not supported yet", k)
		}
	}
	delete(fwd, "src")
	delete(fwd, "content")
	fwd["_checksum"] = fsutil.Sha256Bytes(content)
	fwd["_original"] = original

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

// resolveSrc finds a source file: absolute, playbook-relative, or under the
// conventional subdirectory (files/ or templates/).
func resolveSrc(actx *Context, src, subdir string) string {
	if filepath.IsAbs(src) {
		if _, err := os.Stat(src); err == nil {
			return src
		}
		return ""
	}
	candidates := []string{
		filepath.Join(actx.BaseDir, subdir, src),
		filepath.Join(actx.BaseDir, src),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

var _ = fmt.Sprintf
