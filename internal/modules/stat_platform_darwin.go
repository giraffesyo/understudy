//go:build darwin

package modules

import (
	"os"
	"syscall"
)

func statInode(info os.FileInfo) any {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ino)
	}
	return int64(0)
}

func statDev(info os.FileInfo) any {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Dev)
	}
	return int64(0)
}

// statPlatform is the platform-dependent part of the stat module's output:
// the os.stat_result attributes CPython exposes on macOS.
func statPlatform(info os.FileInfo) map[string]any {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return map[string]any{
		"blocks":           st.Blocks,
		"disk_usage_bytes": st.Blocks * 512,
		"block_size":       int64(st.Blksize),
		"device_type":      int64(st.Rdev),
		"flags":            int64(st.Flags),
		"generation":       int64(st.Gen),
		"birthtime":        float64(st.Birthtimespec.Sec) + float64(st.Birthtimespec.Nsec)/1e9,
	}
}
