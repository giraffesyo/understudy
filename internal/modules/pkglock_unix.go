//go:build unix

package modules

import (
	"os"
	"syscall"
)

// lockPackageManager takes an exclusive host-wide lock around package
// manager runs; it degrades to no locking if the lock file is unusable.
func lockPackageManager() func() {
	f, err := os.OpenFile(pkgLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
