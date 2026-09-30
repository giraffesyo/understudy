package modules

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/giraffesyo/understudy/internal/agentproto"
)

// This file ports the copy module's remote_src directory handling
// (copy_directory and its helpers in ansible/modules/copy.py) with the
// shutil and filecmp behavior it relies on.

// treeOpts carries the module parameters the directory helpers read.
type treeOpts struct {
	env          *RunEnv
	owner, group *string
	localFollow  *bool // nil when unset: neither True nor False
}

func (o *treeOpts) followIs(v bool) bool { return o.localFollow != nil && *o.localFollow == v }

// copyDirectory is copy_directory: a missing dest is created with
// shutil.copytree (modes and times preserved, symlinks kept unless
// local_follow is true); an existing one gets the differing and missing
// entries, recursively, and then owner/group throughout.
func copyDirectory(o *treeOpts, src, dest string) (bool, error) {
	if !pathExists(dest) {
		if !o.env.CheckMode {
			if err := pyCopytree(src, dest, !o.followIs(true)); err != nil {
				return false, err
			}
			if _, err := chownRecursive(o, dest); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	diffChanged, err := copyDiffFiles(o, src, dest)
	if err != nil {
		return false, err
	}
	leftChanged, err := copyLeftOnly(o, src, dest)
	if err != nil {
		return false, err
	}
	commonChanged, err := copyCommonDirs(o, src, dest)
	if err != nil {
		return false, err
	}
	ownerChanged, err := chownRecursive(o, dest)
	if err != nil {
		return false, err
	}
	return diffChanged || leftChanged || commonChanged || ownerChanged, nil
}

// copyDiffFiles is copy_diff_files: common regular files whose contents
// differ (filecmp's shallow comparison) are copied over with their mode.
func copyDiffFiles(o *treeOpts, src, dest string) (bool, error) {
	dc, err := newDircmp(src, dest)
	if err != nil {
		return false, err
	}
	diff := dc.diffFiles()
	changed := len(diff) > 0
	if o.env.CheckMode {
		return changed, nil
	}
	for _, item := range diff {
		s, d := pyJoin(src, item), pyJoin(dest, item)
		if isLink(s) && o.followIs(false) {
			linkto, err := os.Readlink(s)
			if err != nil {
				return changed, err
			}
			if err := os.Symlink(linkto, d); err != nil {
				return changed, err
			}
		} else {
			if err := pyCopyfile(s, d); err != nil {
				return changed, err
			}
			if err := pyCopymode(s, d); err != nil {
				return changed, err
			}
		}
		if err := chownPath(o, d); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// copyLeftOnly is copy_left_only: entries only in src are copied. With
// local_follow unset, symlinks match none of its cases and are skipped
// (still counting as a change), exactly as in Ansible.
func copyLeftOnly(o *treeOpts, src, dest string) (bool, error) {
	dc, err := newDircmp(src, dest)
	if err != nil {
		return false, err
	}
	changed := len(dc.leftOnly) > 0
	if o.env.CheckMode {
		return changed, nil
	}
	for _, item := range dc.leftOnly {
		s, d := pyJoin(src, item), pyJoin(dest, item)
		link, dir, file := isLink(s), isDir(s), isFile(s)
		switch {
		case link && dir && o.followIs(true):
			if err := pyCopytree(s, d, false); err != nil {
				return changed, err
			}
			if _, err := chownRecursive(o, d); err != nil {
				return changed, err
			}
		case link && (dir || file) && o.followIs(false):
			linkto, err := os.Readlink(s)
			if err != nil {
				return changed, err
			}
			if err := os.Symlink(linkto, d); err != nil {
				return changed, err
			}
		case link && file && o.followIs(true):
			if err := pyCopyfile(s, d); err != nil {
				return changed, err
			}
			if err := chownPath(o, d); err != nil {
				return changed, err
			}
		case !link && file:
			if err := pyCopyfile(s, d); err != nil {
				return changed, err
			}
			if err := pyCopymode(s, d); err != nil {
				return changed, err
			}
			if err := chownPath(o, d); err != nil {
				return changed, err
			}
		case !link && dir:
			if err := pyCopytree(s, d, !o.followIs(true)); err != nil {
				return changed, err
			}
			if _, err := chownRecursive(o, d); err != nil {
				return changed, err
			}
		}
		changed = true
	}
	return changed, nil
}

// copyCommonDirs is copy_common_dirs: recurse into directories present on
// both sides.
func copyCommonDirs(o *treeOpts, src, dest string) (bool, error) {
	dc, err := newDircmp(src, dest)
	if err != nil {
		return false, err
	}
	changed := false
	for _, item := range dc.commonDirs {
		s, d := pyJoin(src, item), pyJoin(dest, item)
		a, err := copyDiffFiles(o, s, d)
		if err != nil {
			return changed, err
		}
		b, err := copyLeftOnly(o, s, d)
		if err != nil {
			return changed, err
		}
		if a || b {
			changed = true
		}
		c, err := copyCommonDirs(o, s, d)
		if err != nil {
			return changed, err
		}
		changed = c || changed
	}
	return changed, nil
}

// chownPath is chown_path: owner then group, if given and different.
func chownPath(o *treeOpts, path string) error {
	changed, fail := setOwner(o.env, path, o.owner, false, nil)
	if fail != nil {
		return &failErr{fail}
	}
	if _, fail := setGroup(o.env, path, o.group, changed, nil); fail != nil {
		return &failErr{fail}
	}
	return nil
}

// chownRecursive is chown_recursive over os.walk(path): every directory,
// subdirectory entry (symlinks to directories included, not descended) and
// file.
func chownRecursive(o *treeOpts, path string) (bool, error) {
	if o.owner == nil && o.group == nil {
		return false, nil
	}
	changed := false
	one := func(p string) error {
		c, fail := setOwner(o.env, p, o.owner, false, nil)
		if fail != nil {
			return &failErr{fail}
		}
		c, fail = setGroup(o.env, p, o.group, c, nil)
		if fail != nil {
			return &failErr{fail}
		}
		changed = changed || c
		return nil
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := one(dir); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil // os.walk ignores unreadable directories
		}
		var dirs, files []string
		for _, e := range entries {
			p := pyJoin(dir, e.Name())
			if isDir(p) {
				dirs = append(dirs, p)
			} else {
				files = append(files, p)
			}
		}
		for _, p := range dirs {
			if err := one(p); err != nil {
				return err
			}
		}
		for _, p := range files {
			if err := one(p); err != nil {
				return err
			}
		}
		for _, p := range dirs {
			if !isLink(p) {
				if err := walk(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return changed, walk(path)
}

// dircmp is the part of filecmp.dircmp the copy module uses.
type dircmp struct {
	left, right string
	leftOnly    []string
	commonDirs  []string
	commonFiles []string
}

// filecmpIgnores is filecmp.DEFAULT_IGNORES: names dircmp never compares.
var filecmpIgnores = []string{"RCS", "CVS", "tags", ".git", ".hg", ".bzr", "_darcs", "__pycache__"}

func listFiltered(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !slices.Contains(filecmpIgnores, e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

func newDircmp(a, b string) (*dircmp, error) {
	left, err := listFiltered(a)
	if err != nil {
		return nil, err
	}
	right, err := listFiltered(b)
	if err != nil {
		return nil, err
	}
	dc := &dircmp{left: a, right: b}
	for _, x := range left {
		if !slices.Contains(right, x) {
			dc.leftOnly = append(dc.leftOnly, x)
			continue
		}
		as, aerr := os.Stat(pyJoin(a, x))
		bs, berr := os.Stat(pyJoin(b, x))
		if aerr != nil || berr != nil {
			continue // common_funny
		}
		at, bt := as.Mode().Type(), bs.Mode().Type()
		switch {
		case at != bt:
		case as.IsDir():
			dc.commonDirs = append(dc.commonDirs, x)
		case as.Mode().IsRegular():
			dc.commonFiles = append(dc.commonFiles, x)
		}
	}
	return dc, nil
}

// diffFiles is dircmp.diff_files: common files filecmp.cmp (shallow)
// reports as different; files that cannot be compared are "funny".
func (dc *dircmp) diffFiles() []string {
	var out []string
	for _, x := range dc.commonFiles {
		same, err := filecmpShallow(pyJoin(dc.left, x), pyJoin(dc.right, x))
		if err == nil && !same {
			out = append(out, x)
		}
	}
	return out
}

// filecmpShallow is filecmp.cmp(f1, f2, shallow=True): equal (type, size,
// mtime) signatures count as the same file; otherwise sizes, then bytes,
// are compared.
func filecmpShallow(f1, f2 string) (bool, error) {
	s1, err := os.Stat(f1)
	if err != nil {
		return false, err
	}
	s2, err := os.Stat(f2)
	if err != nil {
		return false, err
	}
	if !s1.Mode().IsRegular() || !s2.Mode().IsRegular() {
		return false, nil
	}
	if s1.Size() == s2.Size() && s1.ModTime().Equal(s2.ModTime()) {
		return true, nil
	}
	if s1.Size() != s2.Size() {
		return false, nil
	}
	a, err := os.ReadFile(f1)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(f2)
	if err != nil {
		return false, err
	}
	return bytes.Equal(a, b), nil
}

// pyCopyfile is shutil.copyfile: contents only, dst opened for writing
// (following a symlink at dst).
func pyCopyfile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pyCopymode is shutil.copymode: the source's permission bits (following
// symlinks on both sides).
func pyCopymode(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return syscall.Chmod(dst, sIMode(st.Mode()))
}

// pyCopystat is shutil.copystat: permission bits and atime/mtime. With
// follow false on a symlink, the link's own mode is set where lchmod
// exists and its times are left alone.
func pyCopystat(src, dst string, follow bool) error {
	if !follow && isLink(src) {
		if haveLchmod {
			st, err := os.Lstat(src)
			if err != nil {
				return err
			}
			if err := lchmod(dst, sIMode(st.Mode())); err != nil && !errors.Is(err, syscall.ENOTSUP) {
				return err
			}
		}
		return nil
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.Chtimes(dst, st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	return syscall.Chmod(dst, sIMode(st.Mode()))
}

// pyCopy2 is shutil.copy2: copyfile then copystat.
func pyCopy2(src, dst string) error {
	if isDir(dst) {
		dst = pyJoin(dst, filepath.Base(src))
	}
	if err := pyCopyfile(src, dst); err != nil {
		return err
	}
	return pyCopystat(src, dst, true)
}

// pyCopytree is shutil.copytree(src, dst, symlinks=symlinks) with copy2:
// dst (and missing parents) is created, entries are copied with their
// modes and times, symlinks are recreated when symlinks is true and
// followed otherwise, and finally the directory's own stat is copied.
func pyCopytree(src, dst string, symlinks bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	var errs []string
	for _, e := range entries {
		s, d := pyJoin(src, e.Name()), pyJoin(dst, e.Name())
		var err error
		switch {
		case isLink(s):
			linkto, rerr := os.Readlink(s)
			switch {
			case rerr != nil:
				err = rerr
			case symlinks:
				if err = os.Symlink(linkto, d); err == nil {
					err = pyCopystat(s, d, false)
				}
			case !pathExists(s):
				err = &os.PathError{Op: "copytree", Path: s, Err: syscall.ENOENT}
			case isDir(s):
				err = pyCopytree(s, d, symlinks)
			default:
				err = pyCopy2(s, d)
			}
		case e.IsDir():
			err = pyCopytree(s, d, symlinks)
		default:
			err = pyCopy2(s, d)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := pyCopystat(src, dst, true); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// treeFailure turns a directory-copy error into the module's result.
func treeFailure(err error) *agentproto.Result {
	var fe *failErr
	if errors.As(err, &fe) {
		return fe.res
	}
	return moduleCrash(err)
}
