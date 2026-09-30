package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Umask returns the process umask.
func Umask() uint32 { return processUmask() }

// AtomicMove is AnsibleModule.atomic_move: rename src over dest. With
// keepDestAttrs an existing dest's owner, mode and flags carry over to the
// new content; a newly created dest gets 0666 &^ umask and the caller's
// euid (and the directory's group when it is setgid). Cross-device and
// permission-denied renames fall back to a copy beside dest. With SELinux
// on, dest keeps its label (a new dest gets the policy's default), since
// the rename carries src's; a labeling failure is a *SELinuxError.
func AtomicMove(src, dest string, keepDestAttrs bool) error {
	var destStat os.FileInfo
	var context SEContext
	if st, err := os.Stat(dest); err == nil && keepDestAttrs {
		destStat = st
		if uid, gid, ok := statIDs(st); ok {
			if err := os.Chown(src, uid, gid); err != nil && !errors.Is(err, syscall.EPERM) {
				return err
			}
		}
		if err := copyStat(dest, src); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		now := time.Now()
		os.Chtimes(src, now, now)
		if SELinuxEnabled() {
			if context, err = SELinuxContext(dest); err != nil {
				return err
			}
		}
	} else if SELinuxEnabled() {
		context = SELinuxDefaultContext(dest)
	}
	_, statErr := os.Stat(dest)
	creating := statErr != nil

	if err := os.Rename(src, dest); err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) || (errno != syscall.EPERM && errno != syscall.EXDEV &&
			errno != syscall.EACCES && errno != syscall.ETXTBSY && errno != syscall.EBUSY) {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(dest), ".ansible_tmp*"+filepath.Base(dest))
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		tmp.Close()
		defer os.Remove(tmpName)
		if err := copyFileContents(src, tmpName, !keepDestAttrs); err != nil {
			return err
		}
		os.Remove(src)
		if keepDestAttrs {
			if tst, err := os.Stat(tmpName); err == nil && destStat != nil {
				tu, tg, _ := statIDs(tst)
				du, dg, _ := statIDs(destStat)
				if tu != du || tg != dg {
					os.Chown(tmpName, du, dg)
				}
			}
			now := time.Now()
			os.Chtimes(tmpName, now, now)
		}
		if SELinuxEnabled() {
			if _, err := SetSELinuxContextIfDifferent(tmpName, context, false); err != nil {
				return err
			}
		}
		if err := os.Rename(tmpName, dest); err != nil {
			return err
		}
	}
	if creating {
		if err := syscall.Chmod(dest, 0o666&^processUmask()); err != nil {
			return err
		}
		gid := os.Getegid()
		if dst, err := os.Stat(filepath.Dir(dest)); err == nil && dst.Mode()&os.ModeSetgid != 0 {
			if _, g, ok := statIDs(dst); ok {
				gid = g
			}
		}
		os.Chown(dest, os.Geteuid(), gid) // best effort, as in Ansible
	}
	if SELinuxEnabled() {
		if _, err := SetSELinuxContextIfDifferent(dest, context, false); err != nil {
			return err
		}
	}
	return nil
}

// copyStat is shutil.copystat's permission-bit and timestamp copy.
func copyStat(from, to string) error {
	st, err := os.Stat(from)
	if err != nil {
		return err
	}
	if err := os.Chtimes(to, st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	return syscall.Chmod(to, unixBits(st.Mode()))
}

// copyFileContents is shutil.copy (content + mode) or, with meta,
// shutil.copy2 (content + mode + times).
func copyFileContents(src, dst string, meta bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := syscall.Chmod(dst, unixBits(st.Mode())); err != nil {
		return err
	}
	if meta {
		return os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return nil
}

// CopyFile is shutil.copyfile followed, with stat, by shutil.copystat.
func CopyFile(src, dst string, stat bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if stat {
		return copyStat(src, dst)
	}
	return nil
}
