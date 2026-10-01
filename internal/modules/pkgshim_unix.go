//go:build unix

package modules

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Package managers run by commands in a parallel block take the same
// host-wide lock as the package modules (see lockPackageManager): the
// command's PATH leads with a directory of shims, links to this binary
// named after each package manager, and a shim takes the lock and
// becomes the real program (found on the original PATH), holding the
// lock until it exits.

// pkgShimEnv carries the lock file and the original PATH to a shim.
const pkgShimEnv = "UNDERSTUDY_PKG_SHIM"

// pkgShimNames are the package managers shimmed.
var pkgShimNames = []string{"apk", "apt", "apt-get", "aptitude", "dnf", "dnf5", "dpkg", "microdnf", "rpm", "yum", "zypper"}

func pkgLockPath() string { return os.TempDir() + "/understudy-pkg.lock" }

func init() {
	spec, ok := os.LookupEnv(pkgShimEnv)
	if !ok || !slices.Contains(pkgShimNames, filepath.Base(os.Args[0])) {
		return
	}
	lock, path, _ := strings.Cut(spec, "\n")
	os.Unsetenv(pkgShimEnv)
	os.Setenv("PATH", path)
	name := filepath.Base(os.Args[0])
	real, err := exec.LookPath(name)
	if err != nil {
		os.Stderr.WriteString(name + ": command not found\n")
		os.Exit(127)
	}
	// The lock's descriptor stays open across exec (no close-on-exec):
	// the package manager holds it until it exits.
	if fd, err := syscall.Open(lock, syscall.O_CREAT|syscall.O_RDWR, 0o600); err == nil {
		syscall.Flock(fd, syscall.LOCK_EX)
	}
	argv := append([]string{name}, os.Args[1:]...)
	err = syscall.Exec(real, argv, os.Environ())
	os.Stderr.WriteString(name + ": " + err.Error() + "\n")
	os.Exit(126)
}

// pkgShimDir makes the shim directory for a command's PATH ("" when it
// cannot).
func pkgShimDir() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	dir, err := os.MkdirTemp("", "understudy-pkgshim-")
	if err != nil {
		return ""
	}
	for _, n := range pkgShimNames {
		if err := os.Symlink(self, filepath.Join(dir, n)); err != nil {
			os.RemoveAll(dir)
			return ""
		}
	}
	return dir
}

// withPkgShims puts the shims ahead of an environment's PATH.
func (env *RunEnv) withPkgShims(environ []string) []string {
	if !env.PkgShim {
		return environ
	}
	if env.shimDir == "" {
		if env.shimDir = pkgShimDir(); env.shimDir == "" {
			return environ
		}
	}
	path, at := "", -1
	for i, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path, at = v, i
		}
	}
	out := slices.Clone(environ)
	shimmed := "PATH=" + env.shimDir + string(os.PathListSeparator) + path
	if at < 0 {
		out = append(out, shimmed)
	} else {
		out[at] = shimmed
	}
	return append(out, pkgShimEnv+"="+pkgLockPath()+"\n"+path)
}

// shimmedPath is the program a command runs by name: its shim, for a
// package manager named without a directory.
func (env *RunEnv) shimmedPath(name, path string) string {
	if !env.PkgShim || strings.Contains(name, "/") || !slices.Contains(pkgShimNames, name) {
		return path
	}
	if env.shimDir == "" {
		if env.shimDir = pkgShimDir(); env.shimDir == "" {
			return path
		}
	}
	return filepath.Join(env.shimDir, name)
}
