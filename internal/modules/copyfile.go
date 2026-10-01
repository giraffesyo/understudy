package modules

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(copyModule, "copy", "ansible.builtin.copy")
}

var copySpec = args.Spec{
	"src":                {},
	"_original_basename": {},
	"content":            {},
	"dest":               {Required: true},
	"backup":             {Type: "bool", Default: false},
	"force":              {Type: "bool", Default: true},
	"validate":           {},
	"directory_mode":     {Type: "any"},
	"remote_src":         {Type: "bool", Default: false},
	"local_follow":       {Type: "bool"},
	"checksum":           {},
	"follow":             {Type: "bool", Default: false},
	"mode":               {Type: "any"},
	"owner":              {},
	"group":              {},
	"seuser":             {},
	"serole":             {},
	"setype":             {},
	"selevel":            {},
	"attributes":         {Aliases: []string{"attr"}},
	"unsafe_writes":      {Type: "bool", Default: false},
}

// copyActionKey marks a request from the control-side copy/template
// action: the payload is the source content and the agent performs the
// action plugin's per-file work (ActionModule._copy_file) in one round
// trip.
const copyActionKey = "_copy_action"

// realFileArgs is the copy action's REAL_FILE_ARGS: what it forwards to
// the file module.
var realFileArgs = map[string]bool{
	"mode": true, "owner": true, "group": true, "seuser": true, "serole": true,
	"selevel": true, "setype": true, "attributes": true, "attr": true, "unsafe_writes": true,
	"state": true, "path": true, "_original_basename": true, "recurse": true,
	"force": true, "_diff_peek": true, "src": true,
}

func copyModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	if a, ok := rawArgs[copyActionKey].(map[string]any); ok {
		user := make(map[string]any, len(rawArgs))
		for k, v := range rawArgs {
			if k != copyActionKey {
				user[k] = v
			}
		}
		return copyAction(env, user, a)
	}
	return copyCore(env, rawArgs)
}

// remoteStat is what ActionBase._execute_remote_stat reports.
type remoteStat struct {
	exists, isdir, islnk bool
	checksum, lnkSource  string
}

func statForCopy(path string, follow bool) remoteStat {
	statFn := os.Lstat
	if follow {
		statFn = os.Stat
	}
	info, err := statFn(path)
	if err != nil {
		return remoteStat{checksum: "1"} // never matches, as in Ansible
	}
	st := remoteStat{exists: true, isdir: info.IsDir(), islnk: info.Mode()&os.ModeSymlink != 0}
	if info.Mode().IsRegular() {
		if sum, err := sha1File(path); err == nil {
			st.checksum = sum
		}
	}
	if st.islnk {
		st.lnkSource = pyRealpath(path)
	}
	return st
}

// copyAction is ActionModule._copy_file for one source plus the result
// assembly of ActionModule.run for a single file.
func copyAction(env *RunEnv, user map[string]any, a map[string]any) *agentproto.Result {
	var payload []byte
	if env.Payload != nil {
		var err error
		if payload, err = io.ReadAll(env.Payload); err != nil {
			return agentproto.Fail("reading payload: %v", err)
		}
	}
	force, _ := argBool(user, "force", true)
	follow, _ := argBool(user, "follow", false)
	isContent, _ := a["content"].(bool)
	source, _ := a["source"].(string)
	sourceRel, _ := a["source_rel"].(string)
	dest, _ := argString(user, "dest")
	dest = pyExpandUser(dest)

	diffs := []any{}
	destFile := dest
	if strings.HasSuffix(dest, "/") {
		destFile = pyJoin(dest, sourceRel)
	}
	st := statForCopy(destFile, follow)
	if st.exists && st.isdir {
		if isContent {
			return &agentproto.Result{Failed: true, Msg: "can not use content with a dir as dest",
				Diff: diffs, Origin: "action"}
		}
		destFile = pyJoin(dest, sourceRel)
		st = statForCopy(destFile, follow)
	}
	if st.exists && !force {
		// _copy_file returns None; the action reports dest and src.
		return &agentproto.Result{Origin: "action", Extra: map[string]any{"dest": dest, "src": source, "_copy_none": true}}
	}

	localSum := sha1Hex(payload)
	var mr *agentproto.Result
	if localSum != st.checksum {
		if env.DiffMode {
			diffs = append(diffs, copyDiffData(destFile, payload, isContent, source))
		}
		if env.CheckMode {
			return &agentproto.Result{Changed: true, Diff: diffs}
		}
		remoteTmp, _ := a["remote_tmp"].(string)
		tmpDir, reportSrc, tmpSrc, err := stagePayload(env, payload, pySplitExt(destFile), remoteTmp)
		if err != nil {
			return agentproto.Fail("staging the source file: %v", err)
		}
		defer os.RemoveAll(tmpDir)
		margs := map[string]any{}
		for k, v := range user {
			if k != "content" && k != "decrypt" {
				margs[k] = v
			}
		}
		margs["src"] = tmpSrc
		margs["dest"] = dest
		margs["_original_basename"] = sourceRel
		margs["follow"] = follow
		if s, _ := argString(user, "checksum"); s == "" {
			margs["checksum"] = localSum
		}
		mr = copyCore(env, margs)
		if mr != nil && mr.Extra != nil && mr.Extra["src"] == tmpSrc {
			mr.Extra["src"] = reportSrc
		}
	} else {
		if follow {
			if nf := statForCopy(destFile, false); nf.islnk && nf.lnkSource != "" {
				dest = nf.lnkSource
			}
		}
		fargs := map[string]any{}
		for k, v := range user {
			if realFileArgs[k] {
				fargs[k] = v
			}
		}
		fargs["dest"] = dest
		fargs["_original_basename"] = sourceRel
		fargs["recurse"] = false
		fargs["state"] = "file"
		delete(fargs, "src")
		mr = fileModule(env, fargs)
	}
	addPathInfo(mr)
	if mr.Extra == nil {
		mr.Extra = map[string]any{}
	}
	if s, _ := mr.Extra["checksum"].(string); s == "" {
		mr.Extra["checksum"] = localSum
	}
	if mr.Diff == nil {
		mr.Diff = diffs
	}
	if mr.Failed {
		return mr
	}
	if p, ok := mr.Extra["path"]; ok {
		if _, has := mr.Extra["dest"]; !has {
			mr.Extra["dest"] = p
		}
	}
	return mr
}

// copyDiffData is ActionBase._get_diff_data for a copy.
func copyDiffData(dest string, content []byte, isContent bool, source string) map[string]any {
	const maxDiff = 104448 // C.MAX_FILE_SIZE_FOR_DIFF
	d := map[string]any{}
	info, err := os.Stat(dest)
	switch {
	case err != nil:
		d["before"] = ""
	case peekBinary(dest):
		d["dst_binary"] = int64(1)
	case info.Size() > 0 && info.Size() > maxDiff:
		d["dst_larger"] = int64(maxDiff)
	case info.IsDir():
	default:
		if old, err := os.ReadFile(dest); err == nil {
			d["before_header"] = dest
			d["before"] = pyToText(old)
		}
	}
	switch {
	case len(content) > maxDiff:
		d["src_larger"] = int64(maxDiff)
	case bytes.IndexByte(content, 0) >= 0:
		d["src_binary"] = int64(1)
	default:
		if isContent {
			d["after_header"] = dest
		} else {
			d["after_header"] = source
		}
		d["after"] = pyToText(content)
	}
	return d
}

// peekBinary is the file module's _diff_peek.
func peekBinary(path string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	head := make([]byte, 8192)
	n, _ := fh.Read(head)
	return bytes.IndexByte(head[:n], 0) >= 0
}

// pyToText is to_text(bytes): invalid UTF-8 becomes U+FFFD.
func pyToText(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

// stagePayload writes the transferred content where Ansible's transfer
// would: <tmpdir>/.source<ext> (see transferDir). It returns the staging
// directory to remove afterwards, the path the file is reported at and
// the path it was written to (the same unless an unprivileged become
// user runs the module).
func stagePayload(env *RunEnv, content []byte, ext, remoteTmp string) (dir, report, actual string, err error) {
	if env != nil && env.StageDir != "" {
		// The login user staged the file in the system temp dir. Where
		// the module may write there (a common group's rwx), the file is
		// used in place, the login user's as in ansible; otherwise the
		// module works from its own copy.
		staged := filepath.Join(env.StageDir, agentproto.StagedPayload)
		named := filepath.Join(env.StageDir, ".source"+ext)
		if os.Rename(staged, named) == nil {
			return "", named, named, nil
		}
	}
	reportDir, dir, err := transferDir(env, remoteTmp)
	if err != nil {
		return "", "", "", err
	}
	actual = filepath.Join(dir, ".source"+ext)
	if err := os.WriteFile(actual, content, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", "", err
	}
	return dir, filepath.Join(reportDir, ".source"+ext), actual, nil
}

// transferDir makes the directory a transferred file lands in, as the
// action's _make_tmp_path does: <remote_tmp>/ansible-tmp-<time>-<pid>-<random>,
// remote_tmp ("~/.ansible/tmp" by default, its ~ the login user's home)
// made 0700 like the shell plugin's mkdir, and owned by the login user
// when made under become. An unusable remote_tmp falls back to the
// system temp directory. When an unprivileged become user runs the
// module, the login user made the directory (env.StageDir), which the
// module cannot write to: files are reported there but written to a
// private directory. It returns the directory to report and the one
// made (to write to and remove afterwards).
func transferDir(env *RunEnv, remoteTmp string) (report, dir string, err error) {
	if env != nil && env.StageDir != "" {
		dir, err := os.MkdirTemp("", "ansible-stage-")
		return env.StageDir, dir, err
	}
	name := fmt.Sprintf("ansible-tmp-%s-%d-%d", pyFloat(float64(time.Now().UnixNano())/1e9), os.Getpid(), rand.Int63n(1<<48))
	base := os.TempDir()
	if remoteTmp != "" {
		if rt := loginExpandUser(env, remoteTmp); mkdirAllAsLogin(env, rt) == nil {
			base = rt
		}
	}
	dir = filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", "", err
	}
	return dir, dir, nil
}

// loginExpandUser expands a leading ~ to the login user's home: the
// action plugin expands remote_tmp without become.
func loginExpandUser(env *RunEnv, p string) string {
	if env != nil && env.LoginHome != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return env.LoginHome + p[1:]
	}
	return pyExpandUser(p)
}

// mkdirAllAsLogin is os.MkdirAll(p, 0700), giving the directories it
// makes to the login user when running as root under become (the login
// user would have made them).
func mkdirAllAsLogin(env *RunEnv, p string) error {
	var made []string
	for d := filepath.Clean(p); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		made = append(made, d)
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	if env != nil && env.LoginHome != "" && env.LoginUID > 0 && os.Geteuid() == 0 {
		for _, d := range made {
			os.Chown(d, env.LoginUID, env.LoginGID)
		}
	}
	return nil
}

// pyFloat is repr(float) for ordinary magnitudes.
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sha1File(path string) (string, error) { return digestFile(path, sha1.New()) }

func md5File(path string) (string, error) { return digestFile(path, md5.New()) }

func digestFile(path string, h interface {
	io.Writer
	Sum([]byte) []byte
}) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pySplitExt is os.path.splitext's extension: from the last dot of the base
// name, ignoring leading dots (".hidden" has none).
func pySplitExt(p string) string {
	base := p[strings.LastIndexByte(p, '/')+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || strings.Trim(base[:dot], ".") == "" {
		return ""
	}
	return base[dot:]
}

// pyExpandUser is os.path.expanduser.
func pyExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isLink(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// accessOK is os.access(path, mode) for R_OK (4) / W_OK (2).
func accessOK(path string, mode uint32) bool {
	return syscall.Access(path, mode) == nil
}

// copyCore ports ansible.builtin.copy's module (main()).
func copyCore(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := copySpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	src := pyExpandPath(p.Str("src"))
	dest := pyExpandPath(p.Str("dest"))
	if !strings.Contains(dest, "/") {
		dest = "./" + dest
	}
	origBase := p.Str("_original_basename")
	remoteSrc := p.Bool("remote_src")
	follow := p.Bool("follow")
	force := p.Bool("force")

	if !pathExists(src) {
		return agentproto.Fail("Source %s not found", src)
	}
	if !accessOK(src, 4) {
		return agentproto.Fail("Source %s not readable", src)
	}
	var mode any
	if p.Has("mode") {
		mode = p.Any("mode")
		if mode == "preserve" {
			info, _ := os.Stat(src)
			mode = fmt.Sprintf("0%03o", sIMode(info.Mode()))
		}
	}

	changed := false
	var checksumSrc, md5Src any
	var checksumDest any
	if isFile(src) {
		if s, err := sha1File(src); err == nil {
			checksumSrc = s
		}
		if s, err := md5File(src); err == nil {
			md5Src = s
		}
	} else if remoteSrc && !isDir(src) {
		return agentproto.Fail("Cannot copy invalid source '%s': not a file", src)
	}
	if want := p.Str("checksum"); want != "" && checksumSrc != want {
		return &agentproto.Result{Failed: true, Msg: "Copied file does not match the expected checksum. Transfer failed.",
			Extra: map[string]any{"checksum": checksumSrc, "expected_checksum": want}}
	}

	fa := fileAttrs{Mode: mode}
	if p.Has("owner") {
		s := p.Str("owner")
		fa.Owner = &s
	}
	if p.Has("group") {
		s := p.Str("group")
		fa.Group = &s
	}

	if strings.HasSuffix(dest, "/") {
		if origBase != "" {
			dest = pyJoin(dest, origBase)
		}
		dirname := filepath.Dir(dest)
		if !pathExists(dirname) {
			pre, newDirs := splitPreExistingDir(dirname)
			if env.CheckMode {
				return &agentproto.Result{Changed: true, Msg: fmt.Sprintf("dest directory %s would be created", dirname),
					Extra: map[string]any{"src": src}}
			}
			if err := os.MkdirAll(dirname, 0o777); err != nil {
				return moduleCrash(err)
			}
			changed = true
			dirArgs := fa
			dirArgs.Mode = nil
			if p.Has("directory_mode") {
				dirArgs.Mode = p.Any("directory_mode")
			}
			cur := pre
			for _, d := range newDirs {
				cur = pyJoin(cur, d)
				dirArgs.Path = cur
				var fail *agentproto.Result
				if changed, fail = setFSAttrs(env, dirArgs, changed, nil); fail != nil {
					return fail
				}
			}
		}
	}
	if isDir(dest) {
		base := src[strings.LastIndex(src, "/")+1:] // os.path.basename: "" for "dir/"
		if origBase != "" {
			base = origBase
		}
		dest = pyJoin(dest, base)
	}

	if pathExists(dest) {
		if isLink(dest) && follow {
			dest = pyRealpath(dest)
		}
		if !force {
			return &agentproto.Result{Msg: "file already exists", Extra: map[string]any{"src": src, "dest": dest}}
		}
		if accessOK(dest, 4) && isFile(dest) {
			if s, err := sha1File(dest); err == nil {
				checksumDest = s
			}
		}
	} else if dir := filepath.Dir(dest); !pathExists(dir) {
		if _, err := os.Stat(dir); err != nil && errors.Is(err, os.ErrPermission) {
			return agentproto.Fail("Destination directory %s is not accessible", dir)
		}
		return agentproto.Fail("Destination directory %s does not exist", dir)
	}
	if !accessOK(filepath.Dir(dest), 2) && !p.Bool("unsafe_writes") {
		return agentproto.Fail("Destination %s not writable", filepath.Dir(dest))
	}

	var backupFile string
	if checksumSrc != checksumDest || isLink(dest) {
		if !env.CheckMode {
			if p.Bool("backup") && pathExists(dest) {
				b, err := fsutil.Backup(dest)
				if err != nil {
					return moduleCrash(err)
				}
				backupFile = b
			}
			if isLink(dest) {
				os.Remove(dest)
				if f, err := os.Create(dest); err == nil {
					f.Close()
				}
			}
			if v := p.Str("validate"); v != "" {
				if _, fail := setMode(env, src, mode, false, nil); fail != nil {
					return fail
				}
				if _, fail := setOwner(env, src, fa.Owner, false, nil); fail != nil {
					return fail
				}
				if _, fail := setGroup(env, src, fa.Group, false, nil); fail != nil {
					return fail
				}
				if !strings.Contains(v, "%s") {
					return agentproto.Fail("validate must contain %%s: %s", v)
				}
				rc, out, errOut := runValidate(env, strings.ReplaceAll(v, "%s", src))
				if rc != 0 {
					return &agentproto.Result{Failed: true, Msg: "failed to validate", Extra: map[string]any{
						"exit_status": int64(rc), "stdout": out, "stderr": errOut,
						"stdout_lines": lines(out), "stderr_lines": lines(errOut)}}
				}
			}
			mysrc := src
			if remoteSrc && isFile(src) {
				tmp, err := os.CreateTemp(filepath.Dir(dest), "tmp")
				if err != nil {
					return moduleCrash(err)
				}
				tmp.Close()
				mysrc = tmp.Name()
				if err := fsutil.CopyFile(src, mysrc, true); err != nil {
					os.Remove(mysrc)
					return moduleCrash(err)
				}
			}
			if err := fsutil.AtomicMove(mysrc, dest, !remoteSrc); err != nil {
				if res := seFailure(err); res != nil {
					return res
				}
				return &agentproto.Result{Failed: true,
					Msg: fmt.Sprintf("Task failed: Module failed: Failed to copy %s to %s: %s", pyStrRepr(src), pyStrRepr(dest), pyOSError(err))}
			}
		}
		changed = true
	}

	// remote_src directory trees: "src/" copies the contents into dest,
	// "src" the directory itself.
	attrDest := dest
	if checksumSrc == nil && checksumDest == nil && remoteSrc && isDir(p.Str("src")) &&
		(isDir(p.Str("dest")) || !pathExists(p.Str("dest"))) {
		tsrc, tdest := p.Str("src"), p.Str("dest")
		if !strings.HasSuffix(tsrc, "/") {
			tdest = pyJoin(tdest, filepath.Base(tsrc))
			tsrc = pyJoin(tsrc, "")
		}
		o := &treeOpts{env: env, owner: fa.Owner, group: fa.Group}
		if p.Has("local_follow") {
			b := p.Bool("local_follow")
			o.localFollow = &b
		}
		treeChanged, err := copyDirectory(o, tsrc, tdest)
		if err != nil {
			return treeFailure(err)
		}
		changed = changed || treeChanged
		attrDest = tdest
	}

	res := &agentproto.Result{Extra: map[string]any{
		"dest": dest, "src": src, "md5sum": md5Src, "checksum": checksumSrc,
	}}
	if backupFile != "" {
		res.Extra["backup_file"] = backupFile
	}
	fa.Path = dest
	if isDir(attrDest) && p.Has("directory_mode") {
		fa.Mode = p.Any("directory_mode")
	}
	changed, fail := setFSAttrs(env, fa, changed, nil)
	if fail != nil {
		return fail
	}
	res.Changed = changed
	return res
}

// splitPreExistingDir is the copy module's split_pre_existing_dir.
func splitPreExistingDir(dirname string) (string, []string) {
	head, tail := filepath.Split(dirname)
	head = strings.TrimSuffix(head, "/")
	if head == "" && strings.HasPrefix(dirname, "/") {
		head = "/"
	}
	if head == "" {
		return ".", []string{tail}
	}
	if !pathExists(head) {
		pre, list := splitPreExistingDir(head)
		return pre, append(list, tail)
	}
	return head, []string{tail}
}

// runValidate is module.run_command(cmd) for a validate string: split with
// shlex, no shell.
func runValidate(env *RunEnv, cmdline string) (int, string, string) {
	argv, err := shlexSplit(cmdline)
	if err != nil || len(argv) == 0 {
		return 1, "", fmt.Sprintf("%v", err)
	}
	c := env.Command(argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return agentproto.ExitCode(ee), stdout.String(), stderr.String()
		}
		return 2, "", err.Error()
	}
	return 0, stdout.String(), stderr.String()
}

// lines is str.splitlines() for the *_lines companions.
func lines(s string) []any {
	out := []any{}
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if s == "" {
			break
		}
		out = append(out, l)
	}
	return out
}
