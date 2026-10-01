package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestInterruptReachesModuleCommands: the commands a module starts lead
// their own process group (so a timeout kills their whole tree), which
// takes them out of the terminal's foreground group; a Ctrl-C (SIGINT to
// understudy's group) still reaches them.
func TestInterruptReachesModuleCommands(t *testing.T) {
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	pb := filepath.Join(dir, "play.yml")
	os.WriteFile(pb, []byte(`- hosts: localhost
  gather_facts: false
  tasks:
    - shell: echo $$ > `+pidFile+`.tmp && mv `+pidFile+`.tmp `+pidFile+` && exec sleep 30
`), 0o644)
	cmd := exec.Command(bin, "playbook", "-i", "localhost,", "-c", "local", pb)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // a terminal's job
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); ; {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) // Ctrl-C
	cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the module's command (pid %d) outlived the interrupt", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
