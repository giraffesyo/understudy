//go:build unix

package modules

import (
	"os"
	"syscall"
)

func statIDsOf(info os.FileInfo) (uid, gid int, ok bool) {
	if st, isUnix := info.Sys().(*syscall.Stat_t); isUnix {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}

// nlinkOf is st_nlink (1 when unavailable).
func nlinkOf(info os.FileInfo) uint64 {
	if st, isUnix := info.Sys().(*syscall.Stat_t); isUnix {
		return uint64(st.Nlink)
	}
	return 1
}
