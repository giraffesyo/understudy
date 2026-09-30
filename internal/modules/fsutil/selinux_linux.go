package fsutil

import (
	"strings"
	"syscall"
	"unsafe"
)

const xattrSELinux = "security.selinux"

// isSelinuxfs is verify_selinuxmnt: the path is a mounted selinuxfs.
var isSelinuxfs = func(path string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(path, &st) == nil && uint32(st.Type) == 0xf97cff8c // SELINUX_MAGIC
}

// lgetfilecon is lgetfilecon_raw: the security.selinux attribute of the
// path itself (not a symlink's target).
var lgetfilecon = func(path string) (string, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return "", err
	}
	name, _ := syscall.BytePtrFromString(xattrSELinux)
	buf := make([]byte, 256)
	for {
		n, _, e := syscall.Syscall6(syscall.SYS_LGETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(name)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e == syscall.ERANGE {
			buf = make([]byte, len(buf)*4)
			continue
		}
		if e != 0 {
			return "", e
		}
		return strings.TrimRight(string(buf[:n]), "\x00"), nil
	}
}

// lsetfilecon is lsetfilecon_raw: the label with its terminating NUL, as
// libselinux writes it.
var lsetfilecon = func(path, con string) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	name, _ := syscall.BytePtrFromString(xattrSELinux)
	val := append([]byte(con), 0)
	_, _, e := syscall.Syscall6(syscall.SYS_LSETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
