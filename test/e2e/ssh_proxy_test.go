//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSSHProxyJumpAndCommand reaches a host that is only on a private
// docker network through a bastion (ProxyJump from ansible_ssh_common_args),
// and reaches the bastion itself through a ProxyCommand.
func TestSSHProxyJumpAndCommand(t *testing.T) {
	bastionPort := startContainer(t) // builds understudy-ssh-test, runs understudy-e2e
	for _, c := range [][]string{
		{"network", "rm", "understudy-jump"},
		{"rm", "-f", "understudy-inner"},
	} {
		exec.Command("docker", c...).Run()
	}
	if out, err := exec.Command("docker", "network", "create", "understudy-jump").CombinedOutput(); err != nil {
		t.Fatalf("network: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "understudy-inner").Run()
		exec.Command("docker", "network", "disconnect", "understudy-jump", "understudy-e2e").Run()
		exec.Command("docker", "network", "rm", "understudy-jump").Run()
	})
	if out, err := exec.Command("docker", "network", "connect", "understudy-jump", "understudy-e2e").CombinedOutput(); err != nil {
		t.Fatalf("connect bastion: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "run", "-d", "--name", "understudy-inner", "--hostname", "inner-host",
		"--network", "understudy-jump", "understudy-ssh-test").CombinedOutput(); err != nil {
		t.Fatalf("run inner: %v\n%s", err, out)
	}
	for i := 0; i < 30 && exec.Command("docker", "exec", "understudy-inner", "pgrep", "sshd").Run() != nil; i++ {
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(time.Second)

	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built")
	}
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc not available for the ProxyCommand leg")
	}
	dir := t.TempDir()
	inv := fmt.Sprintf(`[all:vars]
ansible_user=%[1]s
ansible_password=%[2]s

[hosts]
inner ansible_host=understudy-inner ansible_ssh_common_args='-o ProxyJump=%[1]s@127.0.0.1:%[3]s'
viacmd ansible_host=bastion ansible_ssh_common_args='-o "ProxyCommand=nc 127.0.0.1 %[3]s"'
`, testUser, testPass, bastionPort)
	os.WriteFile(filepath.Join(dir, "hosts"), []byte(inv), 0o644)
	os.WriteFile(filepath.Join(dir, "play.yml"), []byte(`- hosts: all
  gather_facts: false
  tasks:
    - command: hostname
      register: h
    - debug: {msg: "{{ inventory_hostname }} is {{ h.stdout }}"}
`), 0o644)
	cmd := exec.Command(bin, "playbook", "-i", filepath.Join(dir, "hosts"), filepath.Join(dir, "play.yml"))
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "NO_COLOR=1")
	out, _ := cmd.CombinedOutput()
	s := string(out)
	if cmd.ProcessState.ExitCode() != 0 || !strings.Contains(s, "inner is inner-host") || !strings.Contains(s, "viacmd is ") {
		t.Fatalf("proxy run failed:\n%s", s)
	}
}
