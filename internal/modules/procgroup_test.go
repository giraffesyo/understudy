package modules

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitGone waits for pid to exit (or be reaped as a zombie of init).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("pid %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.HasSuffix(string(data), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A cancelled command's whole process group is killed, background jobs
// included.
func TestCommandCancelKillsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	env := &RunEnv{Ctx: ctx}
	cmd := env.Command("/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	bg := readPid(t, pidFile)
	cancel()
	cmd.Wait()
	waitGone(t, bg)
}

// Signals reach the process groups module commands lead.
func TestSignalChildGroups(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	env := &RunEnv{}
	cmd := env.Command("/bin/sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	bg := readPid(t, pidFile)
	if leaders := childGroupLeaders(os.Getpid()); !slices.Contains(leaders, cmd.Process.Pid) {
		cmd.Process.Kill()
		t.Fatalf("child group leaders %v lack %d", leaders, cmd.Process.Pid)
	}
	signalChildGroups(syscall.SIGTERM)
	err := cmd.Wait()
	ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("shell ended with %v (%v), want SIGTERM", cmd.ProcessState, err)
	}
	waitGone(t, bg)
}
