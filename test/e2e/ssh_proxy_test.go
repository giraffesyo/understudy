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
	bastion, bastionPort := startSSHContainer(t)
	image := dockerBuild(t, "understudy-ssh-test", sshDockerfile, nil)
	network := dockerName("understudy-jump")
	if out, err := exec.Command("docker", "network", "create", "--label", "understudy-test="+dockerRunID, network).CombinedOutput(); err != nil {
		t.Fatalf("network: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "network", "disconnect", "-f", network, bastion).Run()
		exec.Command("docker", "network", "rm", network).Run()
	})
	if out, err := exec.Command("docker", "network", "connect", network, bastion).CombinedOutput(); err != nil {
		t.Fatalf("connect bastion: %v\n%s", err, out)
	}
	// Registered after the network's cleanup, so it runs first: the inner
	// container must be gone before the network can be removed.
	inner := dockerRun(t, "understudy-inner", "--hostname", "inner-host", "--network", network, image)
	for i := 0; i < 30 && exec.Command("docker", "exec", inner, "pgrep", "sshd").Run() != nil; i++ {
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(time.Second)

	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		missingPrereq(t, "bin/understudy not built")
	}
	if _, err := exec.LookPath("nc"); err != nil {
		missingPrereq(t, "nc not available for the ProxyCommand leg")
	}
	dir := t.TempDir()
	inv := fmt.Sprintf(`[all:vars]
ansible_user=%[1]s
ansible_password=%[2]s

[hosts]
inner ansible_host=%[4]s ansible_ssh_common_args='-o ProxyJump=%[1]s@127.0.0.1:%[3]s'
viacmd ansible_host=bastion ansible_ssh_common_args='-o "ProxyCommand=nc 127.0.0.1 %[3]s"'
`, testUser, testPass, bastionPort, inner)
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
