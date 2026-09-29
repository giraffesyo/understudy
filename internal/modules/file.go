package modules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

func init() {
	Register(fileModule, "file", "ansible.builtin.file")
}

var fileSpec = args.Spec{
	"path":                     {Required: true, Aliases: []string{"dest", "name"}},
	"state":                    {Choices: []string{"absent", "directory", "file", "hard", "link", "touch"}},
	"mode":                     {Type: "any"},
	"owner":                    {},
	"group":                    {},
	"src":                      {},
	"force":                    {Type: "bool", Default: false},
	"recurse":                  {Type: "bool", Default: false},
	"follow":                   {Type: "bool", Default: true},
	"modification_time":        {},
	"modification_time_format": {Default: "%Y%m%d%H%M.%S"},
	"access_time":              {},
	"access_time_format":       {Default: "%Y%m%d%H%M.%S"},
	"unsafe_writes":            {Type: "bool", Default: false},
	"attributes":               {Aliases: []string{"attr"}},
	"seuser":                   {},
	"serole":                   {},
	"setype":                   {},
	"selevel":                  {},
	"_original_basename":       {},
	"_diff_peek":               {Type: "bool"},
}

// fileRun carries one invocation of the file module (a port of
// ansible.builtin.file's module-global state).
type fileRun struct {
	env *RunEnv
	p   *args.Parsed
}

// fileTime is get_timestamp_for_time's result: preserve (nil), now, or a
// fixed epoch time.
type fileTime struct {
	preserve bool
	now      bool
	at       float64
}

// fileModule ports ansible.builtin.file state by state, including its
// result shapes and failure messages.
func fileModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := fileSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	f := &fileRun{env: env, p: p}
	path := pyExpandPath(p.Str("path"))
	state := p.Str("state")
	src := ""
	if p.Has("src") {
		src = pyExpandPath(p.Str("src"))
	}

	// additional_parameter_handling
	if state != "link" && state != "absent" {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			base := p.Str("_original_basename")
			if base == "" && src != "" {
				base = filepath.Base(src)
			}
			if base != "" {
				path = filepath.Join(path, base)
			}
		}
	}
	prev, crash := getState(path)
	if crash != nil {
		return crash
	}
	if state == "" {
		switch {
		case prev != "absent":
			state = prev
		case p.Bool("recurse"):
			state = "directory"
		default:
			state = "file"
		}
	}
	if p.Bool("recurse") && state != "directory" {
		return failPath(path, "recurse option requires state to be 'directory'", nil)
	}
	if src != "" && state != "link" && state != "hard" {
		return failPath(path, "src option requires state to be 'link' or 'hard'", nil)
	}

	mtime, fail := f.timestamp(keepTimestampCompat(p, "modification_time", state), p.Str("modification_time_format"))
	if fail != nil {
		return fail
	}
	atime, fail := f.timestamp(keepTimestampCompat(p, "access_time", state), p.Str("access_time_format"))
	if fail != nil {
		return fail
	}

	if p.Has("_diff_peek") {
		binary := false
		if fh, err := os.Open(path); err == nil {
			head := make([]byte, 8192)
			n, _ := fh.Read(head)
			fh.Close()
			binary = strings.IndexByte(string(head[:n]), 0) >= 0
		}
		return &agentproto.Result{Extra: map[string]any{"path": path, "appears_binary": binary}}
	}

	var warnings []any
	if env.CheckMode && state != "absent" {
		// check_owner_exists / check_group_exists
		fa := loadFileAttrs(p, path, p.Bool("follow"))
		if fa.Owner != nil && *fa.Owner != "" {
			if uid, ok := pyInt(*fa.Owner); ok {
				if _, err := user.LookupId(strconv.FormatInt(uid, 10)); err != nil {
					warnings = append(warnings, fmt.Sprintf("failed to look up user with uid %d. Create user up to this point in real play", uid))
				}
			} else if _, err := user.Lookup(*fa.Owner); err != nil {
				warnings = append(warnings, fmt.Sprintf("failed to look up user %s. Create user up to this point in real play", *fa.Owner))
			}
		}
		if fa.Group != nil && *fa.Group != "" {
			if gid, ok := pyInt(*fa.Group); ok {
				if _, err := user.LookupGroupId(strconv.FormatInt(gid, 10)); err != nil {
					warnings = append(warnings, fmt.Sprintf("failed to look up group with gid %d. Create group up to this point in real play", gid))
				}
			} else if _, err := user.LookupGroup(*fa.Group); err != nil {
				warnings = append(warnings, fmt.Sprintf("failed to look up group %s. Create group up to this point in real play", *fa.Group))
			}
		}
	}
	res, diff := f.dispatch(state, path, src, mtime, atime)
	if len(warnings) > 0 {
		if res.Extra == nil {
			res.Extra = map[string]any{}
		}
		res.Extra["warnings"] = warnings
	}
	if res.Failed {
		return res
	}
	if env.DiffMode && diff != nil {
		res.Diff = diff.value()
	}
	return res
}

func (f *fileRun) dispatch(state, path, src string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	var res *agentproto.Result
	var diff *fileDiff
	switch state {
	case "file":
		res, diff = f.ensureFile(path, mtime, atime)
	case "directory":
		res, diff = f.ensureDirectory(path, mtime, atime)
	case "link":
		res, diff = f.ensureSymlink(path, src, mtime, atime)
	case "hard":
		res, diff = f.ensureHardlink(path, src, mtime, atime)
	case "touch":
		res, diff = f.touch(path, mtime, atime)
	case "absent":
		res, diff = f.ensureAbsent(path)
	}
	return res, diff
}

func keepTimestampCompat(p *args.Parsed, key, state string) string {
	if p.Has(key) {
		return p.Str(key)
	}
	switch state {
	case "file", "hard", "directory", "link":
		return "preserve"
	case "touch":
		return "now"
	}
	return ""
}

// timestamp is get_timestamp_for_time.
func (f *fileRun) timestamp(formatted, format string) (fileTime, *agentproto.Result) {
	switch formatted {
	case "", "preserve":
		return fileTime{preserve: true}, nil
	case "now":
		return fileTime{now: true}, nil
	}
	t, err := pyStrptime(formatted, format)
	if err != nil {
		return fileTime{}, agentproto.Fail("Error while obtaining timestamp for time %s using format %s: %v", formatted, format, err)
	}
	return fileTime{at: float64(t.Unix())}, nil
}

// getState is the module's get_state: lstat-based absent/link/directory/
// hard/file. Errors other than ENOENT/EACCES escape as a module crash.
func getState(path string) (string, *agentproto.Result) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "absent", nil
		}
		if errors.Is(err, fs.ErrPermission) {
			return "absent", nil
		}
		return "", moduleCrash(err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "link", nil
	case info.IsDir():
		return "directory", nil
	case nlinkOf(info) > 1:
		return "hard", nil
	}
	return "file", nil
}

func stateOf(path string) string {
	s, _ := getState(path)
	return s
}

// initialDiff is the module's initial_diff.
func initialDiff(path, state, prev string) *fileDiff {
	d := newFileDiff()
	d.Before["path"] = path
	d.After["path"] = path
	if prev != state {
		d.Before["state"] = prev
		d.After["state"] = state
		if state == "absent" && prev == "directory" {
			dirs, files := []any{}, []any{}
			filepath.WalkDir(path, func(p string, de fs.DirEntry, err error) error {
				if err != nil || p == path {
					return nil
				}
				if de.IsDir() {
					dirs = append(dirs, p)
				} else {
					files = append(files, p)
				}
				return nil
			})
			d.Before["path_content"] = map[string]any{"directories": dirs, "files": files}
		}
	}
	return d
}

// updateTimestamp is update_timestamp_for_file.
func (f *fileRun) updateTimestamp(path string, mtime, atime fileTime, diff *fileDiff) (bool, *agentproto.Result) {
	fail := func(err error) (bool, *agentproto.Result) {
		return false, failPath(path, "Error while updating modification or access time: "+pyOSError(err), nil)
	}
	if mtime.now && atime.now {
		now := float64(time.Now().UnixNano()) / 1e9
		st, err := os.Stat(path)
		if err != nil {
			return fail(err)
		}
		prevM, prevA := statTimes(st)
		if !f.env.CheckMode {
			t := time.Now()
			if err := os.Chtimes(path, t, t); err != nil {
				return fail(err)
			}
		}
		if diff != nil {
			if now != prevM {
				diff.set("mtime", prevM, now)
			}
			if now != prevA {
				diff.set("atime", prevA, now)
			}
		}
		return true, nil
	}
	if mtime.preserve && atime.preserve {
		return false, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	prevM, prevA := statTimes(st)
	resolve := func(t fileTime, prev float64) float64 {
		switch {
		case t.preserve:
			return prev
		case t.now:
			return float64(time.Now().UnixNano()) / 1e9
		}
		return t.at
	}
	m, a := resolve(mtime, prevM), resolve(atime, prevA)
	if m == prevM && a == prevA {
		return false, nil
	}
	if !f.env.CheckMode {
		if err := os.Chtimes(path, floatTime(a), floatTime(m)); err != nil {
			return fail(err)
		}
	}
	if diff != nil {
		if m != prevM {
			diff.set("mtime", prevM, m)
		}
		if a != prevA {
			diff.set("atime", prevA, a)
		}
	}
	return true, nil
}

func floatTime(f float64) time.Time {
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9))
}

// attrs runs set_fs_attributes_if_different then the timestamp update.
func (f *fileRun) attrs(fa fileAttrs, changed bool, diff *fileDiff, mtime, atime fileTime) (bool, *agentproto.Result) {
	changed, fail := setFSAttrs(f.env, fa, changed, diff)
	if fail != nil {
		return changed, fail
	}
	tc, fail := f.updateTimestamp(fa.Path, mtime, atime, diff)
	return changed || tc, fail
}

func (f *fileRun) ensureAbsent(path string) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	if prev == "absent" {
		return &agentproto.Result{Extra: map[string]any{"path": path, "state": "absent"}}, nil
	}
	diff := initialDiff(path, "absent", prev)
	if !f.env.CheckMode {
		if prev == "directory" {
			if err := os.RemoveAll(path); err != nil {
				return agentproto.Fail("rmtree failed: %s", pyOSError(err)), nil
			}
		} else if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return failCause(path, "Unlinking failed.", err), nil
		}
	}
	return &agentproto.Result{Changed: true, Extra: map[string]any{"path": path, "state": "absent"}}, diff
}

func (f *fileRun) touch(path string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	res := &agentproto.Result{Extra: map[string]any{"dest": path}}
	changed := false
	if prev == "absent" {
		if f.env.CheckMode {
			res.Changed = true
			return res, nil
		}
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			return failCause(path, "Error, could not touch target.", err), nil
		}
		fh.Close()
		changed = true
	}
	diff := initialDiff(path, "touch", prev)
	fa := loadFileAttrs(f.p, path, f.p.Bool("follow"))
	changed, fail := f.attrs(fa, changed, diff, mtime, atime)
	if fail != nil {
		if prev == "absent" {
			os.Remove(path)
		}
		return fail, nil
	}
	res.Changed = changed
	return res, diff
}

func (f *fileRun) ensureFile(path string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	fa := loadFileAttrs(f.p, path, f.p.Bool("follow"))
	if prev != "file" && f.p.Bool("follow") && prev == "link" {
		path = pyRealpath(path)
		prev = stateOf(path)
		fa.Path = path
	}
	if prev != "file" && prev != "hard" {
		return failPath(path, fmt.Sprintf("file (%s) is %s, cannot continue", path, prev),
			map[string]any{"state": prev}), nil
	}
	diff := initialDiff(path, "file", prev)
	changed, fail := f.attrs(fa, false, diff, mtime, atime)
	if fail != nil {
		return fail, nil
	}
	return &agentproto.Result{Changed: changed, Extra: map[string]any{"path": path}}, diff
}

func (f *fileRun) ensureDirectory(path string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	fa := loadFileAttrs(f.p, path, f.p.Bool("follow"))
	if f.p.Bool("follow") && prev == "link" {
		path = pyRealpath(path)
		fa.Path = path
		prev = stateOf(path)
	}
	changed := false
	diff := initialDiff(path, "directory", prev)

	if prev == "absent" {
		if f.env.CheckMode {
			return &agentproto.Result{Changed: true, Extra: map[string]any{"path": path}}, diff
		}
		cur := ""
		for _, name := range strings.Split(strings.Trim(path, "/"), "/") {
			cur = cur + "/" + name
			if !filepath.IsAbs(path) {
				cur = strings.TrimLeft(cur, "/")
			}
			if _, err := os.Stat(cur); err == nil {
				continue
			}
			if err := os.Mkdir(cur, 0o777); err != nil {
				if !(errors.Is(err, fs.ErrExist) && isDir(cur)) {
					return failPath(path, fmt.Sprintf("There was an issue creating %s as requested: %s", cur, pyOSError(err)), nil), nil
				}
			}
			changed = true
			tmp := fa
			tmp.Path = cur
			var fail *agentproto.Result
			if changed, fail = setFSAttrs(f.env, tmp, changed, diff); fail != nil {
				return fail, nil
			}
			tc, fail := f.updateTimestamp(fa.Path, mtime, atime, diff)
			if fail != nil {
				return fail, nil
			}
			changed = changed || tc
		}
		return &agentproto.Result{Changed: changed, Extra: map[string]any{"path": path}}, diff
	}
	if prev != "directory" {
		return failPath(path, fmt.Sprintf("%s already exists as a %s", path, prev), nil), nil
	}
	changed, fail := f.attrs(fa, changed, diff, mtime, atime)
	if fail != nil {
		return fail, nil
	}
	if f.p.Bool("recurse") {
		rc, fail := f.recursiveAttrs(path, fa, mtime, atime)
		if fail != nil {
			return fail, nil
		}
		changed = changed || rc
	}
	return &agentproto.Result{Changed: changed, Extra: map[string]any{"path": path}}, diff
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// recursiveAttrs is recursive_set_attributes: os.walk order (a directory's
// subdirectories, then its files, then descend).
func (f *fileRun) recursiveAttrs(root string, fa fileAttrs, mtime, atime fileTime) (bool, *agentproto.Result) {
	changed := false
	var walk func(dir string) *agentproto.Result
	walk = func(dir string) *agentproto.Result {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var dirs, files []string
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				dirs = append(dirs, full)
			} else {
				files = append(files, full)
			}
		}
		for _, p := range append(append([]string{}, dirs...), files...) {
			tmp := fa
			tmp.Path = p
			var fail *agentproto.Result
			if changed, fail = setFSAttrs(f.env, tmp, changed, nil); fail != nil {
				return fail
			}
			tc, fail := f.updateTimestamp(p, mtime, atime, nil)
			if fail != nil {
				return fail
			}
			changed = changed || tc
			if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 && f.p.Bool("follow") {
				target, _ := os.Readlink(p)
				tp := filepath.Join(dir, target)
				if ti, err := os.Stat(tp); err == nil {
					if ti.IsDir() {
						rc, fail := f.recursiveAttrs(tp, fa, mtime, atime)
						if fail != nil {
							return fail
						}
						changed = changed || rc
					}
					tmp.Path = tp
					if changed, fail = setFSAttrs(f.env, tmp, changed, nil); fail != nil {
						return fail
					}
					tc, fail := f.updateTimestamp(tp, mtime, atime, nil)
					if fail != nil {
						return fail
					}
					changed = changed || tc
				}
			}
		}
		for _, d := range dirs {
			if fail := walk(d); fail != nil {
				return fail
			}
		}
		return nil
	}
	return changed, walk(root)
}

func (f *fileRun) ensureSymlink(path, src string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	follow, force := f.p.Bool("follow"), f.p.Bool("force")
	haveSrc := f.p.Has("src")
	if !haveSrc && follow && pathExists(path) {
		if t, err := os.Readlink(path); err == nil {
			src, haveSrc = t, true
		}
	}
	relpath := filepath.Dir(path)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		relpath = path
	}
	absrc := ""
	if haveSrc {
		absrc = pyJoin(relpath, src)
	}
	srcVal := any(nil)
	if haveSrc {
		srcVal = src
	}
	if !force && haveSrc && !pathExists(absrc) {
		return failPath(path, "src file does not exist, use 'force=yes' if you really want to create the link: "+absrc,
			map[string]any{"src": src}), nil
	}
	switch {
	case prev == "directory":
		if !force {
			return failPath(path, fmt.Sprintf("refusing to convert from %s to symlink for %s", prev, path), nil), nil
		}
		if entries, _ := os.ReadDir(path); len(entries) > 0 {
			return failPath(path, fmt.Sprintf("the directory %s is not empty, refusing to convert it", path), nil), nil
		}
	case (prev == "file" || prev == "hard") && !force:
		return failPath(path, fmt.Sprintf("refusing to convert from %s to symlink for %s", prev, path), nil), nil
	}

	diff := initialDiff(path, "link", prev)
	changed := false
	switch prev {
	case "hard", "file", "directory", "absent":
		if !haveSrc {
			return agentproto.Fail("src is required for creating new symlinks"), nil
		}
		changed = true
	case "link":
		if haveSrc {
			old, _ := os.Readlink(path)
			if old != src {
				diff.set("src", old, src)
				changed = true
			}
		}
	}

	if changed && !f.env.CheckMode {
		if prev != "absent" {
			tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%d.%f.tmp", os.Getpid(), float64(time.Now().UnixNano())/1e9))
			err := func() error {
				if prev == "directory" {
					if err := os.Remove(path); err != nil {
						return err
					}
				}
				if err := os.Symlink(src, tmp); err != nil {
					return err
				}
				return os.Rename(tmp, path)
			}()
			if err != nil {
				os.Remove(tmp)
				return failPath(path, "Error while replacing: "+pyOSError(err), nil), nil
			}
		} else if err := os.Symlink(src, path); err != nil {
			return failPath(path, "Error while linking: "+pyOSError(err), nil), nil
		}
	}
	res := &agentproto.Result{Changed: changed, Extra: map[string]any{"dest": path, "src": srcVal}}
	if f.env.CheckMode && !pathExists(path) {
		return res, diff
	}
	fa := loadFileAttrs(f.p, path, follow)
	if info, err := os.Lstat(path); follow && err == nil && info.Mode()&os.ModeSymlink != 0 && !pathExists(fa.Path) {
		// Ansible warns: cannot set attributes on a missing link target.
	} else {
		var fail *agentproto.Result
		if changed, fail = f.attrs(fa, changed, diff, mtime, atime); fail != nil {
			return fail, nil
		}
	}
	res.Changed = changed
	return res, diff
}

// pyJoin is os.path.join for two components.
func pyJoin(a, b string) string {
	if strings.HasPrefix(b, "/") || a == "" {
		return b
	}
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

func (f *fileRun) ensureHardlink(path, src string, mtime, atime fileTime) (*agentproto.Result, *fileDiff) {
	prev := stateOf(path)
	follow, force := f.p.Bool("follow"), f.p.Bool("force")
	haveSrc := f.p.Has("src")
	fa := loadFileAttrs(f.p, path, follow)
	if prev != "hard" && !haveSrc {
		return agentproto.Fail("src is required for creating new hardlinks"), nil
	}
	if haveSrc && !pathExists(src) {
		return &agentproto.Result{Failed: true, Msg: "src does not exist",
			Extra: map[string]any{"dest": path, "src": src}}, nil
	}
	srcVal := any(nil)
	if haveSrc {
		srcVal = src
	}
	failDest := func(msg string) *agentproto.Result {
		return &agentproto.Result{Failed: true, Msg: msg, Extra: map[string]any{"dest": path, "src": srcVal}}
	}
	diff := initialDiff(path, "hard", prev)
	changed := false
	switch prev {
	case "absent":
		changed = true
	case "link":
		old, _ := os.Readlink(path)
		if old != src {
			diff.set("src", old, src)
			changed = true
		}
	case "hard":
		if haveSrc && !sameInode(path, src) {
			changed = true
			if !force {
				return failDest("Cannot link, different hard link exists at destination"), nil
			}
		}
	case "file":
		changed = true
		if !force {
			return failDest(fmt.Sprintf("Cannot link, %s exists at destination", prev)), nil
		}
	case "directory":
		changed = true
		if pathExists(path) {
			if sameInode(path, src) {
				return &agentproto.Result{Extra: map[string]any{"path": path}}, nil
			} else if !force {
				return failDest("Cannot link: different hard link exists at destination"), nil
			}
		}
	}

	if changed && !f.env.CheckMode {
		if prev != "absent" {
			tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%d.%f.tmp", os.Getpid(), float64(time.Now().UnixNano())/1e9))
			err := func() error {
				if prev == "directory" && pathExists(path) {
					if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
				if err := os.Link(src, tmp); err != nil {
					return err
				}
				return os.Rename(tmp, path)
			}()
			if err != nil {
				os.Remove(tmp)
				return failPath(path, "Error while replacing: "+pyOSError(err), nil), nil
			}
		} else {
			s := src
			if info, err := os.Lstat(s); follow && err == nil && info.Mode()&os.ModeSymlink != 0 {
				s, _ = os.Readlink(s)
			}
			if err := os.Link(s, path); err != nil {
				return failPath(path, "Error while linking: "+pyOSError(err), nil), nil
			}
		}
	}
	res := &agentproto.Result{Changed: changed, Extra: map[string]any{"dest": path, "src": srcVal}}
	if f.env.CheckMode && !pathExists(path) {
		return res, diff
	}
	changed, fail := f.attrs(fa, changed, diff, mtime, atime)
	if fail != nil {
		return fail, nil
	}
	res.Changed = changed
	return res, diff
}

func sameInode(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

// pyStrptime supports the directives file's time formats use.
func pyStrptime(value, format string) (time.Time, error) {
	var layout strings.Builder
	repl := map[byte]string{'Y': "2006", 'm': "01", 'd': "02", 'H': "15", 'M': "04", 'S': "05", 'y': "06", 'b': "Jan", 'B': "January", 'p': "PM"}
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			i++
			if format[i] == '%' {
				layout.WriteByte('%')
				continue
			}
			r, ok := repl[format[i]]
			if !ok {
				return time.Time{}, fmt.Errorf("'%c' is a bad directive in format '%s'", format[i], format)
			}
			layout.WriteString(r)
			continue
		}
		layout.WriteByte(format[i])
	}
	t, err := time.ParseInLocation(layout.String(), value, time.Local)
	if err != nil {
		return t, fmt.Errorf("time data %s does not match format %s", pyStrRepr(value), pyStrRepr(format))
	}
	return t, nil
}

// pyStrRepr is repr() of a simple Python str.
func pyStrRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
