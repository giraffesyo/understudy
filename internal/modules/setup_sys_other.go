//go:build !linux

package modules

import (
	"os"
	"runtime"
)

func sysUname() unameInfo {
	u := unameInfo{sysname: platformSystem(), machine: normalizeArch(runtime.GOARCH)}
	u.nodename, _ = os.Hostname()
	sysctlUname(&u)
	return u
}

func sysStatvfs(string) (map[string]any, bool) { return nil, false }

func sysFsType(string) int64 { return 0 }

func sysLoadavg() ([3]float64, bool) { return [3]float64{}, false }

// normalizeArch maps GOARCH to the uname -m spelling (platform.machine()).
func normalizeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i386"
	}
	return goarch
}
