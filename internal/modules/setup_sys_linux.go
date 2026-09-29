//go:build linux

package modules

import (
	"syscall"
)

func utsString[T int8 | uint8](b [65]T) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func sysUname() unameInfo {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return unameInfo{sysname: "Linux"}
	}
	return unameInfo{
		sysname:  utsString(u.Sysname),
		nodename: utsString(u.Nodename),
		release:  utsString(u.Release),
		version:  utsString(u.Version),
		machine:  utsString(u.Machine),
	}
}

// sysStatvfs is ansible's get_mount_size (os.statvfs; glibc derives
// f_favail from f_ffree).
func sysStatvfs(path string) (map[string]any, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, false
	}
	frsize := int64(st.Frsize)
	if frsize == 0 {
		frsize = int64(st.Bsize)
	}
	blocks := int64(st.Blocks)
	bavail := int64(st.Bavail)
	files := int64(st.Files)
	favail := int64(st.Ffree)
	return map[string]any{
		"size_total":      frsize * blocks,
		"size_available":  frsize * bavail,
		"block_size":      int64(st.Bsize),
		"block_total":     blocks,
		"block_available": bavail,
		"block_used":      blocks - bavail,
		"inode_total":     files,
		"inode_available": favail,
		"inode_used":      files - favail,
	}, true
}

func sysStatID(path string) (uint64, uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

func sysFsType(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Type)
}

func sysLoadavg() ([3]float64, bool) {
	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err != nil {
		return [3]float64{}, false
	}
	const scale = 1 << 16
	return [3]float64{float64(si.Loads[0]) / scale, float64(si.Loads[1]) / scale, float64(si.Loads[2]) / scale}, true
}
