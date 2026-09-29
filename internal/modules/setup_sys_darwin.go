//go:build darwin

package modules

import (
	"os"
	"strings"
	"syscall"
)

func sysctlUname(u *unameInfo) {
	if v, err := syscall.Sysctl("kern.osrelease"); err == nil {
		u.release = v
	}
	if v, err := syscall.Sysctl("kern.version"); err == nil {
		u.version = strings.TrimSpace(v)
	}
	if v, err := syscall.Sysctl("hw.machine"); err == nil && v != "" {
		u.machine = v
	}
}

func sysStatID(path string) (uint64, uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino), true
	}
	return 0, 0, false
}
