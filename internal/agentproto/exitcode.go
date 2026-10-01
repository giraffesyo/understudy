package agentproto

import (
	"os/exec"
	"syscall"
)

// ExitCode is the rc a result reports for a command that exited
// unsuccessfully: Python's Popen.returncode, which is the exit status,
// or the negated signal number when a signal killed the process (-15
// for SIGTERM, -9 for SIGKILL).
func ExitCode(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return ee.ExitCode()
}
