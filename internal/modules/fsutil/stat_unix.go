//go:build unix

package fsutil

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// statIDs extracts uid/gid from a FileInfo on unix systems.
func statIDs(info os.FileInfo) (uid, gid int, ok bool) {
	if st, isUnix := info.Sys().(*syscall.Stat_t); isUnix {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}

var (
	umaskOnce sync.Once
	umaskVal  uint32
)

// processUmask returns the process umask. Linux exposes it read-only in
// /proc/self/status; elsewhere it is read once by setting and restoring it,
// the only portable way (racy against concurrent file creation, hence once).
func processUmask() uint32 {
	umaskOnce.Do(func() {
		if data, err := os.ReadFile("/proc/self/status"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "Umask:"); ok {
					if n, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err == nil {
						umaskVal = uint32(n)
						return
					}
				}
			}
		}
		old := syscall.Umask(0o022)
		syscall.Umask(old)
		umaskVal = uint32(old)
	})
	return umaskVal
}
