//go:build linux

package modules

import (
	"os"
	"syscall"
)

// statTimes is (st_mtime, st_atime) as Python floats.
func statTimes(info os.FileInfo) (mtime, atime float64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return float64(info.ModTime().UnixNano()) / 1e9, 0
	}
	return float64(st.Mtim.Sec) + float64(st.Mtim.Nsec)/1e9, float64(st.Atim.Sec) + float64(st.Atim.Nsec)/1e9
}

// statCtime is st_ctime as a Python float.
func statCtime(info os.FileInfo) float64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return float64(st.Ctim.Sec) + float64(st.Ctim.Nsec)/1e9
	}
	return 0
}
