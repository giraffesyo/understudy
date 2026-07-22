//go:build unix

package fsutil

import (
	"os"
	"syscall"
)

// statIDs extracts uid/gid from a FileInfo on unix systems.
func statIDs(info os.FileInfo) (uid, gid int, ok bool) {
	if st, isUnix := info.Sys().(*syscall.Stat_t); isUnix {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}
