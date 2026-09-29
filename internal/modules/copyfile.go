package modules

import (
	"bytes"
	"fmt"
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
	"remote_src":     {Type: "bool", Default: false},
	"src":            {}, // with remote_src: a path on the target
	"validate":       {},
	"follow":         {Type: "bool", Default: false},
	"unsafe_writes":  {Type: "bool", Default: false},
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

	if p.Bool("remote_src") {
		src := p.Str("src")
		if src == "" {
			return agentproto.Fail("src is required with remote_src")
		}
		info, err := os.Stat(src)
		if err != nil {
			return agentproto.Fail("Source %s not found", src)
		}
		if info.IsDir() {
			return copyRemoteTree(env, p, src, dest)
		}
		env.Payload = nil
		data, err := os.ReadFile(src)
		if err != nil {
			return agentproto.Fail("Source %s not readable: %v", src, err)
		}
		rawArgs["content"] = string(data)
		rawArgs["_original"] = src
		delete(rawArgs, "remote_src")
		delete(rawArgs, "src")
		return copyModule(env, rawArgs)
	}

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

	sha1sum := fsutil.Sha1Bytes(content)
	res := &agentproto.Result{Extra: map[string]any{
		"dest":     dest,
		"checksum": sha1sum, // Ansible's checksum is SHA-1
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
		if info, err := os.Stat(filepath.Dir(dest)); err != nil || !info.IsDir() {
			// Ansible's copy does not create missing parent directories.
			return &agentproto.Result{Failed: true, Extra: map[string]any{"checksum": sha1sum},
				Msg: fmt.Sprintf("Destination directory %s does not exist", filepath.Dir(dest))}
		}
		if v := p.Str("validate"); v != "" {
			if err := fsutil.Validate(v, dest, content); err != nil {
				f := validateFailure(err)
				f.Extra["checksum"] = sha1sum
				return f
			}
		}
		if p.Bool("backup") && destExists {
			backupPath, err := fsutil.Backup(dest)
			if err != nil {
				return agentproto.Fail("backup of %s failed: %v", dest, err)
			}
			res.Extra["backup_file"] = backupPath
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

// validateFailure renders a failed validate command the way Ansible does:
// exit_status plus the command's output (and no rc).
func validateFailure(err error) *agentproto.Result {
	if ve, ok := err.(*fsutil.ValidateError); ok {
		stdout := strings.TrimRight(ve.Stdout, "\n")
		stderr := strings.TrimRight(ve.Stderr, "\n")
		return &agentproto.Result{Failed: true, Msg: "failed to validate", Extra: map[string]any{
			"exit_status": int64(ve.RC),
			"stdout":      stdout, "stdout_lines": lines(stdout),
			"stderr": stderr, "stderr_lines": lines(stderr),
		}}
	}
	return agentproto.Fail("%v", err)
}

func lines(s string) []any {
	out := []any{}
	if s == "" {
		return out
	}
	for _, l := range strings.Split(s, "\n") {
		out = append(out, l)
	}
	return out
}

// copyRemoteTree implements copy with remote_src and a directory src: like
// cp -r, "src/" copies the directory's contents into dest, "src" copies
// the directory itself into dest.
func copyRemoteTree(env *RunEnv, p *args.Parsed, src, dest string) *agentproto.Result {
	root := dest
	if !strings.HasSuffix(src, "/") {
		root = filepath.Join(dest, filepath.Base(src))
	}
	res := &agentproto.Result{Extra: map[string]any{"dest": dest, "src": src}}
	var dirMode any = p.Any("directory_mode")
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(root, rel)
		if d.IsDir() {
			if _, err := os.Stat(target); os.IsNotExist(err) {
				res.Changed = true
				if env.CheckMode {
					return nil
				}
				info, _ := d.Info()
				if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
					return err
				}
			}
			if dirMode != nil && !env.CheckMode {
				changed, err := fsutil.ApplyFileAttrs(target, dirMode, p.Str("owner"), p.Str("group"), true)
				if err != nil {
					return err
				}
				res.Changed = res.Changed || changed
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		old, rerr := os.ReadFile(target)
		exists := rerr == nil
		if !exists || (p.Bool("force") && !bytes.Equal(old, data)) {
			res.Changed = true
			if !env.CheckMode {
				info, _ := d.Info()
				if err := fsutil.AtomicRewrite(target, bytes.NewReader(data), info.Mode().Perm()); err != nil {
					return err
				}
			}
		}
		if env.CheckMode || (!p.Has("mode") && p.Str("owner") == "" && p.Str("group") == "") {
			return nil
		}
		var mode any
		if p.Has("mode") {
			mode = p.Any("mode")
		}
		changed, err := fsutil.ApplyFileAttrs(target, mode, p.Str("owner"), p.Str("group"), true)
		res.Changed = res.Changed || changed
		return err
	})
	if err != nil {
		return agentproto.Fail("copy %s -> %s: %v", src, dest, err)
	}
	return res
}
