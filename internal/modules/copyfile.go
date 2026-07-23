package modules

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(copyModule, "copy", "ansible.builtin.copy")
}

var copySpec = args.Spec{
	"dest":           {Required: true},
	"mode":           {Type: "any"},
	"owner":          {},
	"group":          {},
	"backup":         {Type: "bool", Default: false},
	"force":          {Type: "bool", Default: true},
	"directory_mode": {Type: "any"},
	// Set by the control-side action, not by users directly:
	"_checksum": {}, // sha256 of the incoming payload
	"_original": {}, // original src path (for result reporting)
	"content":   {}, // only when invoked without an action (local shortcut)
}

const maxDiffBytes = 200 * 1024

// copyModule receives file content as the frame payload and places it at
// dest with checksum-based change detection and an atomic write.
func copyModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := copySpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	dest := p.Str("dest")

	// Content arrives via payload (agent path) or the content arg (rare
	// direct invocation).
	var content []byte
	if env.Payload != nil {
		content, err = io.ReadAll(env.Payload)
		if err != nil {
			return agentproto.Fail("reading payload: %v", err)
		}
	} else if p.Has("content") {
		content = []byte(p.Str("content"))
	} else {
		return agentproto.Fail("copy: no content provided (control action should have attached a payload)")
	}

	newSum := fsutil.Sha256Bytes(content)
	if want := p.Str("_checksum"); want != "" && want != newSum {
		return agentproto.Fail("payload checksum mismatch (%s != %s): transfer corrupted", newSum, want)
	}

	// Trailing-slash dest or existing directory: place inside it.
	if strings.HasSuffix(dest, "/") {
		dest = filepath.Join(dest, filepath.Base(p.Str("_original")))
	} else if info, err := os.Stat(dest); err == nil && info.IsDir() {
		base := filepath.Base(p.Str("_original"))
		if base == "" || base == "." {
			return agentproto.Fail("dest %s is a directory and no source filename is known", dest)
		}
		dest = filepath.Join(dest, base)
	}

	res := &agentproto.Result{Extra: map[string]any{
		"dest":     dest,
		"checksum": newSum,
		"size":     len(content),
	}}

	oldSum := ""
	destExists := false
	if _, err := os.Lstat(dest); err == nil {
		destExists = true
		oldSum, _ = fsutil.Sha256File(dest)
	}

	contentChanged := !destExists || oldSum != newSum
	if destExists && !p.Bool("force") {
		contentChanged = false // force=no: never overwrite existing
	}

	if env.DiffMode && contentChanged {
		res.Diff = buildDiff(dest, content, destExists)
	}

	if contentChanged {
		if env.CheckMode {
			res.Changed = true
			return res
		}
		if p.Bool("backup") && destExists {
			backupPath := dest + ".understudy-backup"
			if data, err := os.ReadFile(dest); err == nil {
				os.WriteFile(backupPath, data, 0o600)
				res.Extra["backup_file"] = backupPath
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return agentproto.Fail("creating parent directory: %v", err)
		}
		// AtomicRewrite preserves an existing file's mode+owner across the
		// overwrite (Ansible's atomic_move semantics); explicit mode/owner/
		// group are applied afterward. New files default to 0644.
		if err := fsutil.AtomicRewrite(dest, bytes.NewReader(content), 0o644); err != nil {
			return agentproto.Fail("writing %s: %v", dest, err)
		}
		res.Changed = true
	}

	// Attribute changes apply even when content is unchanged.
	if !env.CheckMode && (p.Has("mode") || p.Str("owner") != "" || p.Str("group") != "") {
		var mode any
		if p.Has("mode") {
			mode = p.Any("mode")
		}
		attrChanged, err := fsutil.ApplyFileAttrs(dest, mode, p.Str("owner"), p.Str("group"), true)
		if err != nil {
			return agentproto.Fail("%v", err)
		}
		if attrChanged {
			res.Changed = true
		}
	}
	return res
}

func buildDiff(dest string, newContent []byte, destExists bool) []agentproto.Diff {
	d := agentproto.Diff{AfterHeader: dest, BeforeHeader: dest}
	if len(newContent) > maxDiffBytes || bytes.IndexByte(newContent, 0) >= 0 {
		d.After = "[content omitted: binary or too large]\n"
	} else {
		d.After = string(newContent)
	}
	if destExists {
		old, err := os.ReadFile(dest)
		switch {
		case err != nil:
			d.Before = ""
		case len(old) > maxDiffBytes || bytes.IndexByte(old, 0) >= 0:
			d.Before = "[content omitted: binary or too large]\n"
		default:
			d.Before = string(old)
		}
	}
	return []agentproto.Diff{d}
}
