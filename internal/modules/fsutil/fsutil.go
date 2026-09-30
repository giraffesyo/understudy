// Package fsutil provides the file primitives modules share: atomic writes,
// checksums, mode parsing, and ownership changes.
package fsutil

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// AtomicWrite writes content to path via a temp file in the SAME directory
// (rename across filesystems fails) with fsync before rename.
func AtomicWrite(path string, content io.Reader, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".understudy-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := io.Copy(tmp, content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// AtomicRewrite overwrites an EXISTING file's content while preserving its
// mode, owner, and group — the temp+rename would otherwise re-create the
// file owned by the writing process (root under become) and reset its
// mode. Modules that edit a file in place (lineinfile, blockinfile, ...)
// use this so an edit never silently changes ownership. For a new file it
// falls back to defaultMode and the current owner.
func AtomicRewrite(path string, content io.Reader, defaultMode os.FileMode) error {
	mode := defaultMode
	uid, gid := -1, -1
	if info, err := os.Lstat(path); err == nil {
		mode = info.Mode().Perm() | specialBits(info.Mode())
		if u, g, ok := statIDs(info); ok {
			uid, gid = u, g
		}
	}
	if err := AtomicWrite(path, content, mode); err != nil {
		return err
	}
	if uid >= 0 || gid >= 0 {
		return os.Chown(path, uid, gid)
	}
	return nil
}

// Sha256File returns the hex sha256 of a file's content.
func Sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Sha1Bytes returns the hex SHA-1 of a byte slice (Ansible's checksum).
func Sha1Bytes(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// Sha256Bytes returns the hex sha256 of a byte slice.
func Sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ParseMode parses Ansible mode values: octal strings ("0644", "644",
// "01777"), integers (YAML 0644 already decodes to 420). Symbolic modes
// ("u=rw,g=r") are relative to the existing mode, so they go through
// ResolveMode; here they error.
func ParseMode(v any) (os.FileMode, error) {
	switch t := v.(type) {
	case int64:
		// YAML 0644 arrives as decimal 420 (already converted); a plain
		// decimal like 644 arrives as 644 which Ansible interprets as
		// octal-as-decimal-digits. Disambiguate the Ansible way: ints are
		// taken as literal mode bits.
		return permBits(uint32(t))
	case int:
		return permBits(uint32(t))
	case float64:
		// A JSON-decoded integer (args that crossed the agent wire
		// without number preservation).
		if t == float64(uint32(t)) {
			return permBits(uint32(t))
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("empty mode")
		}
		if IsSymbolicMode(s) {
			return 0, fmt.Errorf("symbolic mode %q needs the file's current mode (use ResolveMode)", s)
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid mode %q (expected octal like '0644')", s)
		}
		return permBits(uint32(n))
	}
	return 0, fmt.Errorf("invalid mode value of type %T", v)
}

func isOctalString(s string) bool {
	for _, c := range s {
		if c < '0' || c > '7' {
			return false
		}
	}
	return len(s) > 0
}

// permBits converts numeric mode bits including setuid/setgid/sticky.
func permBits(n uint32) (os.FileMode, error) {
	if n > 0o7777 {
		return 0, fmt.Errorf("mode %o out of range", n)
	}
	mode := os.FileMode(n & 0o777)
	if n&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if n&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if n&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode, nil
}

// ModeString renders a FileMode as Ansible's octal string ("0644").
func ModeString(mode os.FileMode) string {
	return "0" + strconv.FormatUint(uint64(unixBits(mode)), 8)
}

// LookupOwnerGroup resolves owner/group names (or numeric ids) to uid/gid.
// Either may be empty (-1 = unchanged).
func LookupOwnerGroup(owner, group string) (uid, gid int, err error) {
	uid, gid = -1, -1
	if owner != "" {
		if n, err2 := strconv.Atoi(owner); err2 == nil {
			uid = n
		} else if u, err2 := user.Lookup(owner); err2 == nil {
			uid, _ = strconv.Atoi(u.Uid)
		} else {
			return 0, 0, fmt.Errorf("user %q not found", owner)
		}
	}
	if group != "" {
		if n, err2 := strconv.Atoi(group); err2 == nil {
			gid = n
		} else if g, err2 := user.LookupGroup(group); err2 == nil {
			gid, _ = strconv.Atoi(g.Gid)
		} else {
			return 0, 0, fmt.Errorf("group %q not found", group)
		}
	}
	return uid, gid, nil
}

// ApplyFileAttrs sets mode/owner/group on path, reporting whether anything
// changed. Empty/nil attributes are left alone.
func ApplyFileAttrs(path string, mode any, owner, group string, followSymlink bool) (changed bool, err error) {
	stat := os.Stat
	if !followSymlink {
		stat = os.Lstat
	}
	info, err := stat(path)
	if err != nil {
		return false, err
	}

	if mode != nil {
		want, err := ResolveMode(mode, info.Mode())
		if err != nil {
			return false, err
		}
		if info.Mode().Perm() != want.Perm() || specialBits(info.Mode()) != specialBits(want) {
			if err := os.Chmod(path, want); err != nil {
				return false, err
			}
			changed = true
		}
	}

	if owner != "" || group != "" {
		uid, gid, err := LookupOwnerGroup(owner, group)
		if err != nil {
			return changed, err
		}
		curUID, curGID, ok := statIDs(info)
		if !ok || (uid >= 0 && uid != curUID) || (gid >= 0 && gid != curGID) {
			if err := os.Chown(path, uid, gid); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

func specialBits(m os.FileMode) os.FileMode {
	return m & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}
