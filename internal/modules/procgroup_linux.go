package modules

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// childGroupLeaders lists the children of parent that lead their own
// process group, from /proc/<pid>/stat ("pid (comm) state ppid pgrp ...").
func childGroupLeaders(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 3 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		pgrp, _ := strconv.Atoi(f[2])
		if ppid == parent && pgrp == pid {
			out = append(out, pid)
		}
	}
	return out
}

// outputGone reports whether stdout is a pipe whose reader has closed it
// (POLLERR/POLLHUP on the write end): the agent's session has ended.
func outputGone() bool {
	fds := []unix.PollFd{{Fd: 1}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0
}
