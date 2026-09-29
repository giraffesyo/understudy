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
