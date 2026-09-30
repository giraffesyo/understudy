package modules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(mountModule, "mount", "ansible.posix.mount")
}

var mountSpec = args.Spec{
	"boot":        {Type: "bool", Default: true},
	"dump":        {Default: "0"},
	"fstab":       {},
	"fstype":      {},
	"path":        {Required: true, Aliases: []string{"name"}},
	"opts":        {},
	"opts_no_log": {Type: "bool", Default: false},
	"passno":      {Default: "0"},
	"src":         {},
	"backup":      {Type: "bool", Default: false},
	"state": {Required: true, Choices: []string{
		"absent", "absent_from_fstab", "mounted", "present", "unmounted", "remounted", "ephemeral"}},
}

// mountArgs is the module's args dict: the fstab fields plus fstab,
// boot and backup_file, returned as the result.
type mountArgs map[string]any

func (a mountArgs) s(k string) string { v, _ := a[k].(string); return v }

// mountModule ports ansible.posix.mount (Linux).
func mountModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := mountSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	state := p.Str("state")
	if state == "mounted" || state == "present" || state == "ephemeral" {
		var missing []string
		for _, k := range []string{"src", "fstype"} {
			if !p.Has(k) {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return agentproto.Fail("state is %s but all of the following are missing: %s", state, strings.Join(missing, ", "))
		}
	}
	var noLog []string
	if p.Bool("opts_no_log") && p.Str("opts") != "" {
		noLog = append(noLog, p.Str("opts"))
	}
	var warnings []any
	fail := func(format string, a ...any) *agentproto.Result {
		r := agentproto.Fail("%s", removeNoLogValues(fmt.Sprintf(format, a...), noLog))
		if len(warnings) > 0 {
			r.Extra = map[string]any{"warnings": warnings}
		}
		return r
	}

	name := pyExpandPath(p.Str("path"))
	a := mountArgs{"name": name, "opts": "defaults", "dump": "0", "passno": "0", "fstab": "/etc/fstab", "boot": "yes", "backup_file": ""}
	for _, k := range []string{"src", "fstype", "passno", "opts", "dump", "fstab"} {
		if p.Has(k) {
			v := p.Str(k)
			if k == "src" {
				v = pyExpandPath(v)
			}
			a[k] = v
		}
	}
	linuxMounts, ok := getLinuxMounts("/proc/self/mountinfo")
	if !ok {
		warnings = append(warnings, "Cannot open file /proc/self/mountinfo. Bind mounts might be misinterpreted.")
	}
	opts := strings.Split(a.s("opts"), ",")
	if p.Bool("boot") && containsStr(opts, "noauto") {
		warnings = append(warnings, "Ignore the 'boot' due to 'opts' contains 'noauto'.")
	} else if !p.Bool("boot") {
		a["boot"] = "no"
		a["opts"] = strings.Join(append(opts, "noauto"), ",")
	}

	fstab := a.s("fstab")
	if state != "ephemeral" {
		if _, err := os.Stat(fstab); err != nil {
			if _, err := os.Stat(filepath.Dir(fstab)); err != nil {
				os.MkdirAll(filepath.Dir(fstab), 0o777)
			}
			f, err := os.OpenFile(fstab, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
			if err != nil {
				if errors.Is(err, os.ErrPermission) {
					return fail("Failed to open %s due to permission issue", fstab)
				}
				return fail("Failed to open %s due to %s", fstab, pyStrOSError(err, fstab))
			}
			f.Close()
		}
	}

	backup := p.Bool("backup")
	check := env.CheckMode
	mounted := func(src, fstype string, withSrc bool) bool {
		return pyIsMount(name) || isBindMounted(env, linuxMounts, name, src, fstype, withSrc)
	}
	changed := false
	switch state {
	case "absent_from_fstab", "absent":
		c, err := unsetMount(a, check, backup)
		if err != nil {
			return fail("%s", err)
		}
		changed = c
		if state == "absent" && changed && !check {
			if mounted("", "", false) {
				if rc, msg := umountPath(env, name); rc != 0 {
					return fail("Error unmounting %s: %s", name, msg)
				}
			}
			if _, err := os.Stat(name); err == nil {
				if err := os.Remove(name); err != nil {
					return fail("Error rmdir %s: %s", name, pyStrOSError(err, name))
				}
			}
		}
	case "unmounted":
		if mounted("", "", false) {
			if !check {
				if rc, msg := umountPath(env, name); rc != 0 {
					return fail("Error unmounting %s: %s", name, msg)
				}
			}
			changed = true
		}
	case "mounted", "ephemeral":
		var dirsCreated []string
		if _, err := os.Stat(name); err != nil && !check {
			cur := ""
			for _, d := range strings.Split(strings.Trim(name, "/"), "/") {
				cur = cur + "/" + d
				if !filepath.IsAbs(name) {
					cur = strings.TrimLeft(cur, "/")
				}
				if _, err := os.Stat(cur); err != nil {
					if err := os.Mkdir(cur, 0o777); err != nil {
						if info, serr := os.Stat(cur); serr != nil || !info.IsDir() {
							return fail("Error making dir %s: %s", name, pyStrOSError(err, cur))
						}
					} else {
						dirsCreated = append(dirsCreated, cur)
					}
				}
			}
		}
		var oldLines []string
		if state != "ephemeral" {
			var err error
			oldLines, changed, err = setMount(a, check, backup)
			if err != nil {
				return fail("%s", err)
			}
		}
		rc, msg := 0, ""
		if mounted(a.s("src"), a.s("fstype"), true) {
			if changed && !check {
				rc, msg = remountPath(env, a, state)
				if rc < 0 {
					return fail("%s", msg)
				}
				changed = true
			}
			if state == "ephemeral" {
				if isSameMountSrc(env, a.s("src"), name, linuxMounts) {
					changed = true
					if !check {
						rc, msg = remountPath(env, a, state)
						if rc < 0 {
							return fail("%s", msg)
						}
					}
				} else {
					return fail("Ephemeral mount point is already mounted with a different " +
						"source than the specified one. Failing in order to prevent an " +
						"unwanted unmount or override operation. Try replacing this command with " +
						"a \"state: unmounted\" followed by a \"state: ephemeral\", or use " +
						"a different destination path.")
				}
			}
		} else {
			changed = true
			if !check {
				rc, msg = mountPath(env, a, state)
			}
		}
		if rc != 0 {
			if state != "ephemeral" {
				os.WriteFile(fstab, []byte(strings.Join(oldLines, "")), 0o644)
			}
			for i := len(dirsCreated) - 1; i >= 0; i-- {
				os.Remove(dirsCreated[i])
			}
			return fail("Error mounting %s: %s", name, msg)
		}
	case "present":
		var err error
		_, changed, err = setMount(a, check, backup)
		if err != nil {
			return fail("%s", err)
		}
	case "remounted":
		if !check {
			rc, msg := remountPath(env, a, state)
			if rc < 0 {
				return fail("%s", msg)
			}
			if rc != 0 {
				return fail("Error remounting %s: %s", name, msg)
			}
		}
		changed = true
	}

	res := &agentproto.Result{Changed: changed, Extra: map[string]any{}}
	for k, v := range a {
		res.Extra[k] = removeNoLogValues(v, noLog)
	}
	if len(warnings) > 0 {
		res.Extra["warnings"] = warnings
	}
	return res
}

// removeNoLogValues is remove_values() for one value: a string equal to a
// no_log value becomes the placeholder, occurrences inside are masked.
func removeNoLogValues(v any, noLog []string) any {
	s, ok := v.(string)
	if !ok || len(noLog) == 0 {
		return v
	}
	for _, n := range noLog {
		if s == n {
			return "VALUE_SPECIFIED_IN_NO_LOG_PARAMETER"
		}
	}
	for _, n := range noLog {
		s = strings.ReplaceAll(s, n, "********")
	}
	return s
}

// escapeFstab escapes space, ampersand and backslash in fstab fields.
func escapeFstab(v string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "&", `\046`).Replace(v)
}

// readFstabLines is readlines(), each line newline-terminated.
func readFstabLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s", pyStrOSError(err, path))
	}
	var lines []string
	for _, l := range strings.SplitAfter(string(data), "\n") {
		if l == "" {
			continue
		}
		if !strings.HasSuffix(l, "\n") {
			l += "\n"
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// writeFstabLines is write_fstab(): backup (optional) then rewrite.
func writeFstabLines(path string, lines []string, backup bool) (string, error) {
	backupFile := ""
	if backup {
		b, err := fsutil.Backup(path)
		if err != nil {
			return "", err
		}
		backupFile = b
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o666)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, ""))
	return backupFile, err
}

// setMount is _set_mount_save_old(): add or update the fstab entry; the
// old lines are returned for rollback.
func setMount(a mountArgs, check, backup bool) ([]string, bool, error) {
	lines, err := readFstabLines(a.s("fstab"))
	if err != nil {
		return nil, false, err
	}
	esc := map[string]string{}
	for _, k := range []string{"src", "name", "fstype", "opts", "dump", "passno"} {
		esc[k] = escapeFstab(a.s(k))
	}
	newLine := func(d map[string]string) string {
		return strings.ReplaceAll(fmt.Sprintf("%s %s %s %s %s %s\n", d["src"], d["name"], d["fstype"], d["opts"], d["dump"], d["passno"]), "\x00", "")
	}
	var toWrite []string
	exists, changed := false, false
	_, hasSrc := a["src"]
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			toWrite = append(toWrite, line)
			continue
		}
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) < 4 || len(fields) > 6 {
			toWrite = append(toWrite, line)
			continue
		}
		labels := []string{"src", "name", "fstype", "opts", "dump", "passno"}
		// Missing optional fields default to the int 0, which never
		// equals the (string) argument: such lines are always rewritten.
		ld := map[string]string{"dump": "\x000", "passno": "\x000"}
		for i, f := range fields {
			ld[labels[i]] = f
		}
		if ld["name"] != esc["name"] || hasSrc && ld["name"] == "none" && ld["fstype"] == "swap" && ld["src"] != a.s("src") {
			toWrite = append(toWrite, line)
			continue
		}
		exists = true
		for _, k := range []string{"src", "fstype", "opts", "dump", "passno"} {
			if ld[k] != esc[k] {
				ld[k] = esc[k]
				changed = true
			}
		}
		if changed {
			toWrite = append(toWrite, newLine(ld))
		} else {
			toWrite = append(toWrite, line)
		}
	}
	if !exists {
		toWrite = append(toWrite, newLine(esc))
		changed = true
	}
	if changed && !check {
		b, err := writeFstabLines(a.s("fstab"), toWrite, backup)
		if err != nil {
			return lines, changed, err
		}
		a["backup_file"] = b
	}
	return lines, changed, nil
}

// unsetMount removes the fstab entry.
func unsetMount(a mountArgs, check, backup bool) (bool, error) {
	lines, err := readFstabLines(a.s("fstab"))
	if err != nil {
		return false, err
	}
	name := escapeFstab(a.s("name"))
	_, hasSrc := a["src"]
	var toWrite []string
	changed := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		f := strings.Fields(line)
		if t == "" || strings.HasPrefix(t, "#") || len(f) != 6 {
			toWrite = append(toWrite, line)
			continue
		}
		if f[1] != name || hasSrc && f[1] == "none" && f[2] == "swap" && f[0] != a.s("src") {
			toWrite = append(toWrite, line)
			continue
		}
		changed = true
	}
	if changed && !check {
		if _, err := writeFstabLines(a.s("fstab"), toWrite, backup); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func fstabArgs(a mountArgs, state string) []string {
	var out []string
	if state != "ephemeral" && a.s("fstab") != "" && a.s("fstab") != "/etc/fstab" {
		out = append(out, "-T", a.s("fstab"))
	}
	if state == "ephemeral" {
		out = append(out, "-t", a.s("fstype"))
		if a.s("opts") != "defaults" {
			out = append(out, "-o", a.s("opts"))
		}
		out = append(out, a.s("src"))
	}
	return out
}

// mountPath is mount(): rc and out+err on failure (rc -1: missing binary).
func mountPath(env *RunEnv, a mountArgs, state string) (int, string) {
	bin, err := getBinPath("mount")
	if err != nil {
		return -1, err.Error()
	}
	argv := append(append([]string{bin}, fstabArgs(a, state)...), a.s("name"))
	rc, out, errOut := runCommand(env, argv, cmdOpts{})
	if rc == 0 {
		return 0, ""
	}
	return rc, out + errOut
}

func umountPath(env *RunEnv, path string) (int, string) {
	bin, err := getBinPath("umount")
	if err != nil {
		return -1, err.Error()
	}
	rc, out, errOut := runCommand(env, []string{bin, path}, cmdOpts{})
	if rc == 0 {
		return 0, ""
	}
	return rc, out + errOut
}

// remountPath is remount(): mount -o remount, falling back to umount and
// mount.
func remountPath(env *RunEnv, a mountArgs, state string) (int, string) {
	bin, err := getBinPath("mount")
	if err != nil {
		return -1, err.Error()
	}
	argv := []string{bin}
	if state == "remounted" && a.s("opts") != "defaults" {
		argv = append(argv, "-o", "remount,"+a.s("opts"))
	} else {
		argv = append(argv, "-o", "remount")
	}
	argv = append(append(argv, fstabArgs(a, state)...), a.s("name"))
	rc, _, _ := runCommand(env, argv, cmdOpts{})
	if rc == 0 {
		return 0, ""
	}
	if state == "remounted" && a.s("opts") != "defaults" {
		return -1, "Options were specified with remounted, but the remount " +
			"command failed. Failing in order to prevent an " +
			"unexpected mount result. Try replacing this command with " +
			"a \"state: unmounted\" followed by a \"state: mounted\" " +
			"using the full desired mount options instead."
	}
	rc, msg := umountPath(env, a.s("name"))
	if rc == 0 {
		rc, msg = mountPath(env, a, state)
	}
	return rc, msg
}

// pyIsMount is os.path.ismount.
func pyIsMount(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	parent, err := os.Lstat(filepath.Join(path, ".."))
	if err != nil {
		return false
	}
	s1, ok1 := st.Sys().(*syscall.Stat_t)
	s2, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return s1.Dev != s2.Dev || s1.Ino == s2.Ino
}

type linuxMount struct{ src, fs string }

// getLinuxMounts is get_linux_mounts(): mountinfo with bind-mount sources
// resolved; ok is false when the file cannot be read.
func getLinuxMounts(path string) (map[string]linuxMount, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	type rec struct {
		id, parent         int
		root, dst, fs, src string
	}
	info := map[int]*rec{}
	var order []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		id, _ := strconv.Atoi(f[0])
		parent, _ := strconv.Atoi(f[1])
		info[id] = &rec{id: id, parent: parent, root: f[3], dst: f[4], fs: f[len(f)-3], src: f[len(f)-2]}
		order = append(order, id)
	}
	mounts := map[string]linuxMount{}
	for _, id := range order {
		mnt := info[id]
		src := mnt.src
		if m, ok := info[mnt.parent]; mnt.parent != 1 && ok {
			root := mnt.root
			if len(m.root) > 1 && strings.HasPrefix(root, m.root+"/") {
				root = root[len(m.root):]
			}
			if m.dst != "/" {
				root = m.dst + root
			}
			src = root
		}
		mounts[mnt.dst] = linuxMount{src: src, fs: mnt.fs}
	}
	return mounts, true
}

// isBindMounted is is_bind_mounted(); withSrc selects the src check.
func isBindMounted(env *RunEnv, mounts map[string]linuxMount, dest, src, fstype string, withSrc bool) bool {
	if mounts != nil {
		m, ok := mounts[dest]
		if !withSrc {
			return ok
		}
		return ok && m.src == src
	}
	bin, err := getBinPath("mount")
	if err != nil {
		return false
	}
	_, out, _ := runCommand(env, []string{bin, "-l"}, cmdOpts{})
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && (!withSrc || f[0] == src) && f[2] == dest && (!withSrc || fstype == "" || f[4] == fstype) {
			return true
		}
	}
	return false
}

// isSameMountSrc is _is_same_mount_src().
func isSameMountSrc(env *RunEnv, src, mountpoint string, mounts map[string]linuxMount) bool {
	if !pyIsMount(mountpoint) && !isBindMounted(env, mounts, mountpoint, "", "", false) {
		return false
	}
	if mounts != nil && isBindMounted(env, mounts, mountpoint, src, "", true) {
		return true
	}
	bin, err := getBinPath("mount")
	if err != nil {
		return false
	}
	_, out, _ := runCommand(env, []string{bin, "-v"}, cmdOpts{})
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == src && f[2] == mountpoint {
			return true
		}
	}
	return false
}
