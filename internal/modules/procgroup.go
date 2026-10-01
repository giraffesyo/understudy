package modules

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// Every command a module starts leads its own process group, so that
// when its task times out (or the run is otherwise cancelled) the whole
// tree it spawned — a shell's background jobs, a pipeline's stages — is
// killed with it, not just the direct child. Being out of the
// terminal's foreground group, those commands no longer see a Ctrl-C
// themselves: ForwardSignals passes it (and SIGTERM/SIGHUP) on.

// ownGroup starts cmd in a new process group, which its cancellation
// kills as a whole.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return cmd.Process.Kill()
		}
		return nil
	}
}

// signalChildGroups sends sig to every process group a child of this
// process leads: the groups ownGroup started.
func signalChildGroups(sig syscall.Signal) {
	self := os.Getpid()
	for _, pid := range childGroupLeaders(self) {
		syscall.Kill(-pid, sig)
	}
}

// ForwardSignals makes an interrupt (Ctrl-C), SIGTERM or SIGHUP reach the
// commands modules are running in their own process groups, then lets
// the signal take this process down as it otherwise would.
func ForwardSignals() {
	var sigs []os.Signal
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) { // started under nohup, or in the background
			sigs = append(sigs, sig)
		}
	}
	if len(sigs) == 0 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	go func() {
		sig := (<-ch).(syscall.Signal)
		signalChildGroups(sig)
		signal.Reset(sigs...)
		syscall.Kill(os.Getpid(), sig)
	}()
}

// ExitWithParent ends this process, killing the process groups of the
// commands its module started, once its parent is gone: an agent whose
// shell was terminated (a timed-out task's, whose become wrapper does not
// pass the SIGTERM on) is reparented, and one whose connection dropped
// finds its output's reader gone — even when that happened before it
// started.
func ExitWithParent() {
	ppid := os.Getppid()
	orphaned := func() bool { return ppid == 1 || os.Getppid() != ppid || outputGone() }
	go func() {
		for ; ; time.Sleep(200 * time.Millisecond) {
			if orphaned() {
				signalChildGroups(syscall.SIGKILL)
				os.Exit(1)
			}
		}
	}()
}
