package modules

import "golang.org/x/sys/unix"

// childGroupLeaders lists the children of parent that lead their own
// process group, from the kernel's process table.
func childGroupLeaders(parent int) []int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	var out []int
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		if int(p.Eproc.Ppid) == parent && int(p.Eproc.Pgid) == pid {
			out = append(out, pid)
		}
	}
	return out
}

// outputGone is not detected here: an orphan is told by its parent.
func outputGone() bool { return false }
