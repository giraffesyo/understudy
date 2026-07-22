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
