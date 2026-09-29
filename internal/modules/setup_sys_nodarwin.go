//go:build !linux && !darwin

package modules

func sysctlUname(*unameInfo) {}

func sysStatID(string) (uint64, uint64, bool) { return 0, 0, false }
