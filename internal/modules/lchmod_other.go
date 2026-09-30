//go:build !darwin

package modules

import "syscall"

// Python has no os.lchmod on Linux: Ansible's chmod-through-the-link and
// restore leaves both the link and its target unchanged.
const haveLchmod = false

func lchmod(string, uint32) error { return syscall.ENOSYS }
