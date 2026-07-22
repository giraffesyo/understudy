//go:build unix

package modules

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in its own session so it survives the
// agent process exiting (fire-and-forget async).
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
