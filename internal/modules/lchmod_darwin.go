package modules

import (
	"syscall"
	"unsafe"
)

// Python has os.lchmod on macOS, so Ansible changes a symlink's own mode
// there (set_mode_if_different).
const haveLchmod = true

const (
	sysFchmodat       = 467 // fchmodat(2) in xnu's syscalls.master
	atFdcwd           = -2
	atSymlinkNofollow = 0x20
)

// lchmod is fchmodat(AT_FDCWD, path, mode, AT_SYMLINK_NOFOLLOW).
func lchmod(path string, mode uint32) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	fd := atFdcwd
	_, _, errno := syscall.Syscall6(sysFchmodat, uintptr(fd), uintptr(unsafe.Pointer(p)), uintptr(mode), atSymlinkNofollow, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
