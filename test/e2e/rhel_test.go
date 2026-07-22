//go:build e2e

// RHEL-family end-to-end: runs representative real-world roles (chrony via
// yum + systemd service/handler; ip forwarding via sysctl + iptables)
// against a booted Rocky Linux container, then re-runs to prove
// idempotence. Requires docker with systemd-in-container support (privileged
// + host cgroups). Run with: go test -tags e2e -run TestRHEL ./test/e2e/
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

const rhelDockerfile = `FROM rockylinux:9
RUN dnf -y install openssh-server sudo systemd iptables && \
    dnf clean all && ssh-keygen -A && \
    useradd -m ` + testUser + ` && echo '` + testUser + `:` + testPass + `' | chpasswd && \
    echo '` + testUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + testUser + ` && \
    systemctl enable sshd
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/sbin/init"]`

// startRHELContainer boots a systemd Rocky container and returns its ssh port.
func startRHELContainer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(rhelDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", "understudy-rhel-test", dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	exec.Command("docker", "rm", "-f", "understudy-rhel-e2e").Run()
	runArgs := []string{"run", "-d", "--name", "understudy-rhel-e2e",
		"--privileged", "--cgroupns=host", "-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"-p", "0:22", "understudy-rhel-test", "/usr/sbin/init"}
	if out, err := exec.Command("docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "understudy-rhel-e2e").Run() })

	// Wait for systemd + sshd.
	for i := 0; i < 60; i++ {
		out, _ := exec.Command("docker", "exec", "understudy-rhel-e2e", "systemctl", "is-active", "sshd").Output()
		if strings.TrimSpace(string(out)) == "active" {
			break
		}
		time.Sleep(time.Second)
	}
	out, err := exec.Command("docker", "exec", "understudy-rhel-e2e", "systemctl", "is-active", "sshd").Output()
	if strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("sshd never became active: %v", err)
	}
	portOut, err := exec.Command("docker", "port", "understudy-rhel-e2e", "22").Output()
	if err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	return line[strings.LastIndexByte(line, ':')+1:]
}

// runRHEL executes a playbook (with roles alongside) against the container
// and returns combined output + exit code.
func runRHEL(t *testing.T, port, dir string) (string, int) {
	t.Helper()
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built (run: make build)")
	}
	inv := filepath.Join(dir, "hosts")
	os.WriteFile(inv, []byte(fmt.Sprintf(
		"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s ansible_password=%s\n",
		port, testUser, testPass)), 0o644)
	cmd := exec.Command(bin, "playbook", "-i", inv, filepath.Join(dir, "site.yml"))
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "NO_COLOR=1")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// TestRHELRolesIdempotent runs real chrony + ip-forwarding roles on a
// RHEL-family host and asserts a clean second run (changed=0).
func TestRHELRolesIdempotent(t *testing.T) {
	port := startRHELContainer(t)
	dir := t.TempDir()

	// ntp role: yum install chrony + systemd service, with a handler.
	writeFiles(t, filepath.Join(dir, "roles", "ntp"), map[string]string{
		"tasks/main.yml": `
- name: install chrony
  yum: {name: chrony}
  notify: restart chronyd
- name: start chronyd
  service: {name: chronyd, state: started, enabled: yes}
`,
		"handlers/main.yml": "- name: restart chronyd\n  service: {name: chronyd, state: restarted}\n",
	})
	// ip_forwarding role: sysctl + iptables.
	writeFiles(t, filepath.Join(dir, "roles", "ipfwd"), map[string]string{
		"tasks/main.yml": `
- name: enable ip forwarding
  sysctl: {name: net.ipv4.ip_forward, value: '1', sysctl_set: yes, reload: yes}
- name: NAT rule
  iptables: {table: nat, chain: POSTROUTING, out_interface: eth0, jump: MASQUERADE}
- name: forward rule
  iptables: {chain: FORWARD, in_interface: eth0, out_interface: eth0, jump: ACCEPT}
`,
	})
	os.WriteFile(filepath.Join(dir, "site.yml"), []byte(
		"- hosts: all\n  become: true\n  roles: [ntp, ipfwd]\n"), 0o644)

	// First run: should converge (some changes expected).
	out1, code1 := runRHEL(t, port, dir)
	if code1 != 0 {
		t.Fatalf("first run exit=%d\n%s", code1, out1)
	}
	if !strings.Contains(out1, "failed=0") {
		t.Errorf("first run had failures:\n%s", out1)
	}

	// Verify real effects landed.
	checks := map[string]string{
		"rpm -q chrony":                                        "chrony-",
		"systemctl is-enabled chronyd":                         "enabled",
		"cat /proc/sys/net/ipv4/ip_forward":                    "1",
		"iptables -t nat -C POSTROUTING -o eth0 -j MASQUERADE": "",
	}
	for cmd, want := range checks {
		out, err := exec.Command("docker", "exec", "understudy-rhel-e2e", "sh", "-c", cmd).CombinedOutput()
		if err != nil && want != "" {
			t.Errorf("check %q failed: %v\n%s", cmd, err, out)
		}
		if want != "" && !strings.Contains(string(out), want) {
			t.Errorf("check %q = %q, want substring %q", cmd, out, want)
		}
	}

	// Second run: must be fully idempotent.
	out2, code2 := runRHEL(t, port, dir)
	if code2 != 0 {
		t.Fatalf("second run exit=%d\n%s", code2, out2)
	}
	if !strings.Contains(out2, "changed=0") {
		t.Errorf("second run was not idempotent (expected changed=0):\n%s", out2)
	}
	if strings.Contains(out2, "RUNNING HANDLER") {
		t.Errorf("handler fired on the idempotent run:\n%s", out2)
	}
}

// writeFiles writes a set of role files under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
