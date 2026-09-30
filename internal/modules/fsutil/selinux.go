package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// SELinux file labeling as AnsibleModule does it through libselinux
// (selinux_enabled, selinux_default_context via matchpathcon,
// selinux_context via lgetfilecon_raw, set_context_if_different via
// lsetfilecon), without linking libselinux: the policy's file_contexts
// files are read and matched here, and labels are the security.selinux
// extended attribute.

// seRoot prefixes every system path read here (tests point it at a tree).
var seRoot = ""

// SpecialFS are the filesystems ansible-core labels like their mount
// point (DEFAULT_SELINUX_SPECIAL_FS).
var SpecialFS = []string{"fuse", "nfs", "vboxsf", "ramfs", "9p", "vfat"}

var (
	seOnce    sync.Once
	seEnabled bool
	seMLS     bool
)

func seProbe() {
	seOnce.Do(func() {
		// HAVE_SELINUX: ansible's bindings are a ctypes wrapper over
		// libselinux.so.1.
		if !haveLibselinux() {
			return
		}
		// is_selinux_enabled: selinuxfs is mounted and the config exists.
		mnt := selinuxMount()
		if mnt == "" {
			return
		}
		if _, err := os.Stat(seRoot + "/etc/selinux/config"); err != nil {
			return
		}
		seEnabled = true
		b, _ := os.ReadFile(seRoot + mnt + "/mls")
		seMLS = strings.TrimSpace(string(b)) == "1"
	})
}

func haveLibselinux() bool {
	if data, err := os.ReadFile(seRoot + "/etc/ld.so.cache"); err == nil && bytes.Contains(data, []byte("libselinux.so.1")) {
		return true
	}
	dirs := []string{"/lib64", "/usr/lib64", "/lib", "/usr/lib", "/usr/local/lib"}
	for _, base := range []string{"/lib", "/usr/lib"} {
		m, _ := filepath.Glob(seRoot + base + "/*-linux-gnu*")
		for _, d := range m {
			dirs = append(dirs, strings.TrimPrefix(d, seRoot))
		}
	}
	for _, d := range dirs {
		if _, err := os.Stat(seRoot + d + "/libselinux.so.1"); err == nil {
			return true
		}
	}
	return false
}

// selinuxMount is libselinux's init_selinuxmnt: /sys/fs/selinux or
// /selinux when selinuxfs is mounted there, else a selinuxfs mount.
func selinuxMount() string {
	for _, mnt := range []string{"/sys/fs/selinux", "/selinux"} {
		if isSelinuxfs(seRoot + mnt) {
			return mnt
		}
	}
	data, _ := os.ReadFile(seRoot + "/proc/mounts")
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[2] == "selinuxfs" {
			return f[1]
		}
	}
	return ""
}

// SELinuxEnabled is AnsibleModule.selinux_enabled.
func SELinuxEnabled() bool {
	seProbe()
	return seEnabled
}

// SEContext is a file context split like ansible-core does
// (user, role, type[, range]); "" stands for None.
type SEContext []string

// initialContext is selinux_initial_context: all None, with a range slot
// when MLS is on.
func initialContext() SEContext {
	seProbe()
	if seMLS {
		return SEContext{"", "", "", ""}
	}
	return SEContext{"", "", ""}
}

func splitContext(s string) SEContext {
	return SEContext(strings.SplitN(s, ":", 4))
}

// SELinuxError is a failure ansible-core reports with fail_json; Extra
// holds its other result keys.
type SELinuxError struct {
	Msg   string
	Extra map[string]any
}

func (e *SELinuxError) Error() string { return e.Msg }

// SELinuxDefaultContext is selinux_default_context(path): the policy's
// label for the path (matchpathcon), or all None.
func SELinuxDefaultContext(path string) SEContext {
	ctx := initialContext()
	if !SELinuxEnabled() {
		return ctx
	}
	con, ok := matchPathCon(path)
	if !ok {
		return ctx
	}
	return splitContext(con)
}

// SELinuxContext is selinux_context(path): the path's current label.
func SELinuxContext(path string) (SEContext, error) {
	ctx := initialContext()
	if !SELinuxEnabled() {
		return ctx, nil
	}
	con, err := lgetfilecon(path)
	if err != nil {
		return nil, &SELinuxError{Msg: "Failed to retrieve selinux context.", Extra: map[string]any{"path": path}}
	}
	return splitContext(con), nil
}

// SetSELinuxContextIfDifferent is set_context_if_different: apply the
// given parts of ctx (None keeps the current part; a special filesystem
// takes its mount point's label). It reports whether the label changes;
// in check mode nothing is written.
func SetSELinuxContextIfDifferent(path string, ctx SEContext, checkMode bool) (bool, error) {
	if !SELinuxEnabled() {
		return false, nil
	}
	if checkMode {
		if _, err := os.Lstat(path); err != nil {
			return true, nil
		}
	}
	cur, err := SELinuxContext(path)
	if err != nil {
		return false, err
	}
	next := append(SEContext(nil), cur...)
	if special, spCtx, err := specialSELinuxPath(path); err != nil {
		return false, err
	} else if special {
		next = spCtx
	} else {
		for i := range cur {
			if i < len(ctx) && ctx[i] != "" && ctx[i] != cur[i] {
				next[i] = ctx[i]
			}
		}
	}
	if strings.Join(cur, "\x00") == strings.Join(next, "\x00") {
		return false, nil
	}
	if checkMode {
		return true, nil
	}
	if err := lsetfilecon(path, strings.Join(next, ":")); err != nil {
		return false, &SELinuxError{
			Msg:   "invalid selinux context: " + pyOSErrorText(err),
			Extra: map[string]any{"path": path, "new_context": contextList(next), "cur_context": contextList(cur), "input_was": contextList(ctx)},
		}
	}
	return true, nil
}

// SetDefaultSELinuxContext is set_default_selinux_context(path, False).
func SetDefaultSELinuxContext(path string, checkMode bool) error {
	if !SELinuxEnabled() {
		return nil
	}
	_, err := SetSELinuxContextIfDifferent(path, SELinuxDefaultContext(path), checkMode)
	return err
}

func contextList(c SEContext) []any {
	out := make([]any, len(c))
	for i, s := range c {
		if s != "" {
			out[i] = s
		}
	}
	return out
}

// pyOSErrorText is str(OSError(errno, strerror)).
func pyOSErrorText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s", int(errno), strings.ToUpper(errno.Error()[:1])+errno.Error()[1:])
	}
	return err.Error()
}

// specialSELinuxPath is is_special_selinux_path: a path on one of the
// SpecialFS filesystems takes its mount point's label.
func specialSELinuxPath(path string) (bool, SEContext, error) {
	data, err := os.ReadFile(seRoot + "/proc/mounts")
	if err != nil {
		return false, nil, nil
	}
	mp := findMountPoint(path)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.SplitN(line, " ", 5)
		if len(f) < 5 || f[1] != mp {
			continue
		}
		for _, fs := range SpecialFS {
			if strings.Contains(f[2], fs) {
				ctx, err := SELinuxContext(mp)
				return true, ctx, err
			}
		}
	}
	return false, nil, nil
}

// findMountPoint is find_mount_point: the realpath's nearest ancestor
// that is a mount point.
func findMountPoint(path string) string {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		p = filepath.Clean(path)
	}
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	for !isMount(p) {
		p = filepath.Dir(p)
	}
	return p
}

// isMount is os.path.ismount.
func isMount(p string) bool {
	if p == "/" {
		return true
	}
	st, err := os.Lstat(p)
	if err != nil || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	pst, err := os.Lstat(filepath.Dir(p))
	if err != nil {
		return false
	}
	a, aok := st.Sys().(*syscall.Stat_t)
	b, bok := pst.Sys().(*syscall.Stat_t)
	if !aok || !bok {
		return false
	}
	return a.Dev != b.Dev || a.Ino == b.Ino
}

// --- matchpathcon ----------------------------------------------------------

// fcSpec is one file_contexts line.
type fcSpec struct {
	re    *regexp.Regexp
	mode  uint32 // S_IFMT bits; 0 = any file type
	con   string
	exact bool // no regex metacharacters
}

type fcDB struct {
	specs     []fcSpec
	subs      [][2]string // file_contexts.subs, most recent first
	distSubs  [][2]string // file_contexts.subs_dist, most recent first
	available bool
}

var (
	fcOnce sync.Once
	fc     fcDB
)

func loadFileContexts() {
	fcOnce.Do(func() { fc = readFileContexts() })
}

func readFileContexts() fcDB {
	var db fcDB
	policy := "targeted"
	cfg, _ := os.ReadFile(seRoot + "/etc/selinux/config")
	for _, line := range strings.Split(string(cfg), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "SELINUXTYPE="); ok && strings.TrimSpace(v) != "" {
			policy = strings.TrimSpace(v)
		}
	}
	base := seRoot + "/etc/selinux/" + policy + "/contexts/files/file_contexts"
	var specs []fcSpec
	for i, suffix := range []string{"", ".homedirs", ".local"} {
		data, err := os.ReadFile(base + suffix)
		if err != nil {
			if i == 0 {
				return db
			}
			continue
		}
		specs = append(specs, parseFileContexts(string(data))...)
	}
	db.available = true
	// label_file's sort_specs: exact paths go last (in order), so the
	// backwards search tries them first.
	for _, s := range specs {
		if !s.exact {
			db.specs = append(db.specs, s)
		}
	}
	for _, s := range specs {
		if s.exact {
			db.specs = append(db.specs, s)
		}
	}
	db.distSubs = readSubs(base + ".subs_dist")
	db.subs = readSubs(base + ".subs")
	return db
}

var fcModes = map[string]uint32{
	"--": syscall.S_IFREG, "-d": syscall.S_IFDIR, "-c": syscall.S_IFCHR, "-b": syscall.S_IFBLK,
	"-s": syscall.S_IFSOCK, "-l": syscall.S_IFLNK, "-p": syscall.S_IFIFO,
}

func parseFileContexts(data string) []fcSpec {
	var specs []fcSpec
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		var s fcSpec
		switch len(f) {
		case 2:
			s.con = f[1]
		case 3:
			m, ok := fcModes[f[1]]
			if !ok {
				continue
			}
			s.mode, s.con = m, f[2]
		default:
			continue
		}
		re, err := regexp.Compile("^(?:" + f[0] + ")$")
		if err != nil {
			continue
		}
		s.re, s.exact = re, !hasMetaChars(f[0])
		specs = append(specs, s)
	}
	return specs
}

// hasMetaChars is label_file's spec_hasMetaChars.
func hasMetaChars(re string) bool {
	for i := 0; i < len(re); i++ {
		switch re[i] {
		case '.', '^', '$', '?', '*', '+', '|', '[', '(', '{':
			return true
		case '\\':
			i++
		}
	}
	return false
}

func readSubs(path string) [][2]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var subs [][2]string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		subs = append([][2]string{{f[0], f[1]}}, subs...)
	}
	return subs
}

// selabelSub is selabel_sub: the first alias whose source is a path
// prefix of key.
func selabelSub(subs [][2]string, key string) (string, bool) {
	for _, s := range subs {
		src, dst := s[0], s[1]
		if strings.HasPrefix(key, src) && (len(key) == len(src) || key[len(src)] == '/') {
			if dst == "/" && len(key) > len(src) {
				return key[len(src):], true
			}
			return dst + key[len(src):], true
		}
	}
	return "", false
}

// matchPathCon is matchpathcon(path, 0): the realpath's label from the
// policy's file_contexts (with the substitution files applied), or false
// when there is none (or it is <<none>>).
func matchPathCon(path string) (string, bool) {
	loadFileContexts()
	if !fc.available {
		return "", false
	}
	if rp, err := filepath.EvalSymlinks(path); err == nil {
		if abs, err := filepath.Abs(rp); err == nil {
			path = abs
		}
	}
	return fc.lookup(path)
}

func (db *fcDB) lookup(key string) (string, bool) {
	// label_file removes repeated slashes.
	for strings.Contains(key, "//") {
		key = strings.ReplaceAll(key, "//", "/")
	}
	if s, ok := selabelSub(db.subs, key); ok {
		key = s
		if d, ok := selabelSub(db.distSubs, key); ok {
			key = d
		}
	} else if d, ok := selabelSub(db.distSubs, key); ok {
		key = d
	}
	for i := len(db.specs) - 1; i >= 0; i-- {
		s := db.specs[i]
		if s.re.MatchString(key) {
			if s.con == "<<none>>" {
				return "", false
			}
			return s.con, true
		}
	}
	return "", false
}
