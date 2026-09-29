package modules

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports the file-attribute half of Ansible's AnsibleModule
// (load_file_common_arguments, set_fs_attributes_if_different and its
// owner/group/mode helpers) so every file-family module reports changes,
// diffs and failures the way Ansible does.

// fileAttrs is load_file_common_arguments' result: the (possibly
// link-followed) path and the requested owner/group/mode, each nil when
// not given.
type fileAttrs struct {
	Path  string
	Mode  any
	Owner *string
	Group *string
}

// fileDiff is a module diff of before/after dicts (file-module style).
type fileDiff struct {
	Before map[string]any
	After  map[string]any
}

func newFileDiff() *fileDiff {
	return &fileDiff{Before: map[string]any{}, After: map[string]any{}}
}

func (d *fileDiff) set(key string, before, after any) {
	if d == nil {
		return
	}
	if d.Before == nil {
		d.Before = map[string]any{}
	}
	if d.After == nil {
		d.After = map[string]any{}
	}
	d.Before[key] = before
	d.After[key] = after
}

// value renders the diff as the module's result "diff" value.
func (d *fileDiff) value() map[string]any {
	return map[string]any{"before": d.Before, "after": d.After}
}

// textDiff is a before/after content diff entry.
func textDiff(beforeHeader, before, afterHeader, after string) map[string]any {
	return map[string]any{"before_header": beforeHeader, "before": before,
		"after_header": afterHeader, "after": after}
}

// attrDiffValue is the "(file attributes)" diff entry lineinfile-style
// modules return: the headers plus whatever set_fs_attributes recorded.
func attrDiffValue(d *fileDiff, path string) map[string]any {
	m := map[string]any{
		"before_header": path + " (file attributes)",
		"after_header":  path + " (file attributes)",
	}
	if d.Before != nil {
		m["before"] = d.Before
	}
	if d.After != nil {
		m["after"] = d.After
	}
	return m
}

// checkFileAttrs is the check_file_attrs helper lineinfile, blockinfile
// and replace share: attribute changes extend the message.
func checkFileAttrs(env *RunEnv, fa fileAttrs, changed bool, msg string, diff *fileDiff) (string, bool, *agentproto.Result) {
	attrChanged, fail := setFSAttrs(env, fa, false, diff)
	if fail != nil {
		return msg, changed, fail
	}
	if attrChanged {
		if changed {
			msg += " and "
		}
		changed = true
		msg += "ownership, perms or SE linux context changed"
	}
	return msg, changed, nil
}

// writeChanges is the write_changes helper of lineinfile/blockinfile:
// content goes to a temp file, is optionally validated, then atomically
// moved onto the real path.
func writeChanges(env *RunEnv, content []byte, dest, validate string, unsafeWrites bool) *agentproto.Result {
	tmp, err := os.CreateTemp("", "tmp")
	if err != nil {
		return moduleCrash(err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return moduleCrash(err)
	}
	tmp.Close()
	if validate != "" {
		if !strings.Contains(validate, "%s") {
			return agentproto.Fail("validate must contain %%s: %s", validate)
		}
		rc, _, errOut := runValidate(env, strings.ReplaceAll(validate, "%s", tmpName))
		if rc != 0 {
			return agentproto.Fail("failed to validate: rc:%d error:%s", rc, errOut)
		}
	}
	if err := fsutil.AtomicMove(tmpName, dest, true); err != nil {
		return moduleCrash(err)
	}
	return nil
}

// failErr is a module failure carried as a Go error through the helpers.
type failErr struct{ res *agentproto.Result }

func (f *failErr) Error() string { return f.res.Msg }

// failPath builds fail_json(msg=..., path=path, **extra).
func failPath(path, msg string, extra map[string]any) *agentproto.Result {
	res := &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{"path": path}}
	for k, v := range extra {
		res.Extra[k] = v
	}
	return res
}

// failCause is fail_json(msg=..., path=path, exception=err): the error's
// message chains onto msg in the error display only.
func failCause(path, msg string, err error) *agentproto.Result {
	res := failPath(path, msg, nil)
	res.Cause = pyOSError(err)
	return res
}

// loadFileAttrs is load_file_common_arguments(params, path).
func loadFileAttrs(p *args.Parsed, path string, follow bool) fileAttrs {
	path = pyExpandPath(path)
	if follow {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			path = pyRealpath(path)
		}
	}
	fa := fileAttrs{Path: path}
	if p.Has("mode") {
		fa.Mode = p.Any("mode")
	}
	if p.Has("owner") {
		s := p.Str("owner")
		fa.Owner = &s
	}
	if p.Has("group") {
		s := p.Str("group")
		fa.Group = &s
	}
	return fa
}

// setFSAttrs is set_fs_attributes_if_different (owner, group, mode). diff
// may be nil. A failure is returned as a result to hand back verbatim.
func setFSAttrs(env *RunEnv, fa fileAttrs, changed bool, diff *fileDiff) (bool, *agentproto.Result) {
	var fail *agentproto.Result
	if changed, fail = setOwner(env, fa.Path, fa.Owner, changed, diff); fail != nil {
		return changed, fail
	}
	if changed, fail = setGroup(env, fa.Path, fa.Group, changed, diff); fail != nil {
		return changed, fail
	}
	return setMode(env, fa.Path, fa.Mode, changed, diff)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func setOwner(env *RunEnv, path string, owner *string, changed bool, diff *fileDiff) (bool, *agentproto.Result) {
	if owner == nil {
		return changed, nil
	}
	if env.CheckMode && !pathExists(path) {
		return true, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return changed, moduleCrash(err)
	}
	origUID, _, _ := statIDsOf(info)
	uid, ok := pyInt(*owner)
	if !ok {
		u, err := user.Lookup(*owner)
		if err != nil {
			return changed, failPath(path, "chown failed: failed to look up user "+*owner, nil)
		}
		n, _ := strconv.Atoi(u.Uid)
		uid = int64(n)
	}
	if int64(origUID) != uid {
		diff.set("owner", int64(origUID), uid)
		if env.CheckMode {
			return true, nil
		}
		if err := os.Lchown(path, int(uid), -1); err != nil {
			return changed, failCause(path, "chown failed", err)
		}
		changed = true
	}
	return changed, nil
}

func setGroup(env *RunEnv, path string, group *string, changed bool, diff *fileDiff) (bool, *agentproto.Result) {
	if group == nil {
		return changed, nil
	}
	if env.CheckMode && !pathExists(path) {
		return true, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return changed, moduleCrash(err)
	}
	_, origGID, _ := statIDsOf(info)
	gid, ok := pyInt(*group)
	if !ok {
		g, err := user.LookupGroup(*group)
		if err != nil {
			return changed, failPath(path, "chgrp failed: failed to look up group "+*group, nil)
		}
		n, _ := strconv.Atoi(g.Gid)
		gid = int64(n)
	}
	if int64(origGID) != gid {
		diff.set("group", int64(origGID), gid)
		if env.CheckMode {
			return true, nil
		}
		if err := os.Lchown(path, -1, int(gid)); err != nil {
			return changed, failPath(path, "chgrp failed", nil)
		}
		changed = true
	}
	return changed, nil
}

// resolveMode is set_mode_if_different's parsing: ints are used as-is,
// strings are int(mode, 8) or else a symbolic mode applied to the current
// stat; the result must be pure permission bits.
func resolveMode(path string, mode any, info os.FileInfo) (int64, *agentproto.Result) {
	switch t := mode.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	}
	s := fmt.Sprintf("%v", mode)
	n, ok := pyIntBase(s, 8)
	if !ok {
		sym, err := fsutil.SymbolicToOctal(s, sIMode(info.Mode()), info.IsDir())
		if err != nil {
			return 0, failPath(path, "mode must be in octal or symbolic form", map[string]any{"details": err.Error()})
		}
		n = int64(sym)
		if n != n&0o7777 {
			return 0, failPath(path, "Invalid mode supplied, only permission info is allowed", map[string]any{"details": n})
		}
	}
	return n, nil
}

func setMode(env *RunEnv, path string, mode any, changed bool, diff *fileDiff) (bool, *agentproto.Result) {
	if mode == nil {
		return changed, nil
	}
	if env.CheckMode && !pathExists(path) {
		return true, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return changed, moduleCrash(err)
	}
	want, fail := resolveMode(path, mode, info)
	if fail != nil {
		return changed, fail
	}
	prev := int64(sIMode(info.Mode()))
	if prev == want {
		return changed, nil
	}
	diff.set("mode", fmt.Sprintf("0%03o", prev), fmt.Sprintf("0%03o", want))
	if env.CheckMode {
		return true, nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if err := syscall.Chmod(path, uint32(want)); err != nil {
			if err != syscall.ENOENT && err != syscall.ELOOP {
				// Re-raised from the except-OSError handler: a module crash.
				return changed, moduleCrash(&os.PathError{Op: "chmod", Path: path, Err: err})
			}
		}
	}
	// Symlinks: Linux has no lchmod, and Ansible's chmod-then-restore
	// leaves the target unchanged, so nothing changes.
	if after, err := os.Lstat(path); err == nil && int64(sIMode(after.Mode())) != prev {
		changed = true
	}
	return changed, nil
}

// moduleCrash is an unhandled Python exception escaping a module: Ansible
// reports it with this verbatim message.
func moduleCrash(err error) *agentproto.Result {
	return &agentproto.Result{Failed: true, Msg: "Task failed: Module failed: " + pyOSError(err)}
}

// pyInt is Python's int(s) for base 10.
func pyInt(s string) (int64, bool) { return pyIntBase(s, 10) }

// pyIntBase is Python's int(s, base): surrounding whitespace, a sign, the
// base prefix (base 8: 0o) and digit-separating underscores are accepted.
func pyIntBase(s string, base int) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if base == 8 && len(s) > 2 && s[0] == '0' && (s[1] == 'o' || s[1] == 'O') {
		s = s[2:]
		if strings.HasPrefix(s, "_") {
			s = s[1:]
		}
	}
	if s == "" || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), base, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// pyExpandPath is the 'path' argument type: os.path.expanduser(
// os.path.expandvars(p)).
func pyExpandPath(p string) string {
	if p == "" {
		return p
	}
	p = pyExpandVars(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	return p
}

// pyExpandVars is os.path.expandvars: $name and ${name} are replaced when
// set; anything else is left untouched.
func pyExpandVars(p string) string {
	if !strings.Contains(p, "$") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '$' || i+1 >= len(p) {
			b.WriteByte(p[i])
			continue
		}
		if p[i+1] == '{' {
			if end := strings.IndexByte(p[i+2:], '}'); end >= 0 {
				name := p[i+2 : i+2+end]
				if v, ok := os.LookupEnv(name); ok {
					b.WriteString(v)
					i += 2 + end
					continue
				}
			}
			b.WriteByte('$')
			continue
		}
		j := i + 1
		for j < len(p) && (p[j] == '_' || p[j] >= 'a' && p[j] <= 'z' || p[j] >= 'A' && p[j] <= 'Z' || p[j] >= '0' && p[j] <= '9') {
			j++
		}
		if v, ok := os.LookupEnv(p[i+1 : j]); ok && j > i+1 {
			b.WriteString(v)
			i = j - 1
			continue
		}
		b.WriteByte('$')
	}
	return b.String()
}

// pyRealpath is os.path.realpath (non-strict): symlinks are resolved
// component by component, dangling ones included; missing components are
// kept as they are.
func pyRealpath(p string) string {
	path, _ := joinRealpath("", p, map[string]*string{})
	if !filepath.IsAbs(path) {
		wd, _ := os.Getwd()
		path = filepath.Join(wd, path)
	}
	return filepath.Clean(path)
}

// joinRealpath is posixpath._joinrealpath.
func joinRealpath(path, rest string, seen map[string]*string) (string, bool) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			switch {
			case path == "":
				path = ".."
			case path == "/":
			case filepath.Base(path) == "..":
				path = path + "/.."
			default:
				path = filepath.Dir(path)
				if path == "." {
					path = ""
				}
			}
			continue
		}
		newpath := pyJoin(path, name)
		if path == "" {
			newpath = name
		}
		info, err := os.Lstat(newpath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if prev, ok := seen[newpath]; ok {
			if prev != nil {
				path = *prev
				continue
			}
			return pyJoin(newpath, rest), false // symlink loop
		}
		seen[newpath] = nil
		target, _ := os.Readlink(newpath)
		var ok bool
		path, ok = joinRealpath(path, target, seen)
		if !ok {
			return pyJoin(path, rest), false
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, true
}
