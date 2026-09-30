//go:build !linux

package fsutil

import "syscall"

// SELinux labels exist only on Linux; SELinuxEnabled is false elsewhere,
// so these are never reached.
var isSelinuxfs = func(string) bool { return false }

var lgetfilecon = func(string) (string, error) { return "", syscall.ENOTSUP }

var lsetfilecon = func(string, string) error { return syscall.ENOTSUP }
