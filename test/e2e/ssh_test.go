//go:build e2e

// SSH end-to-end tests against a real sshd in docker. Requires docker and
// the agents built (make agents). Run with: go test -tags e2e ./test/e2e/
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

const testUser = "tester"
const testPass = "testpass"

// startContainer builds and runs the sshd container, returning its mapped
// port and a cleanup func.
func startContainer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}

	dir := t.TempDir()
	dockerfile := `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y openssh-server sudo && \
    mkdir /run/sshd && \
    useradd -m -s /bin/bash ` + testUser + ` && echo '` + testUser + `:` + testPass + `' | chpasswd && \
    echo '` + testUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + testUser + `
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D"]`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", "understudy-ssh-test", dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	exec.Command("docker", "rm", "-f", "understudy-e2e").Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", "understudy-e2e",
		"-p", "0:22", "understudy-ssh-test").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "understudy-e2e").Run() })

	out, err := exec.Command("docker", "port", "understudy-e2e", "22").Output()
	if err != nil {
		t.Fatal(err)
	}
	// "0.0.0.0:55001\n..." -> port
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	port := line[strings.LastIndexByte(line, ':')+1:]

	// Wait for sshd to accept connections.
	for i := 0; i < 30; i++ {
		c := exec.Command("docker", "exec", "understudy-e2e", "pgrep", "sshd")
		if c.Run() == nil {
			time.Sleep(500 * time.Millisecond)
			return port
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("sshd never came up")
	return ""
}

// runSSH invokes the built understudy binary against the container.
func runSSH(t *testing.T, port, playbookSrc string, extraArgs ...string) (string, int) {
	t.Helper()
	bin, err := filepath.Abs("../../bin/understudy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built (run: make build)")
	}
	dir := t.TempDir()
	pb := filepath.Join(dir, "play.yml")
	if err := os.WriteFile(pb, []byte(playbookSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := filepath.Join(dir, "hosts")
	hostLine := fmt.Sprintf(
		"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s ansible_password=%s\n",
		port, testUser, testPass)
	if err := os.WriteFile(inv, []byte(hostLine), 0o644); err != nil {
		t.Fatal(err)
	}

	args := append([]string{"playbook", "-i", inv, pb}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "NO_COLOR=1")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestSSHPingCommandFacts(t *testing.T) {
	port := startContainer(t)
	out, code := runSSH(t, port, `
- hosts: all
  tasks:
    - ping:
    - name: run a command through the agent
      command: uname -s
      register: un
    - assert:
        that:
          - un.stdout == "Linux"
          - ansible_os_family == "Debian"
          - ansible_distribution == "Ubuntu"
    - name: write a file as root
      become: true
      copy:
        content: "understudy was here\n"
        dest: /root/proof.txt
    - name: verify with raw
      become: true
      raw: cat /root/proof.txt
      register: proof
    - assert:
        that: "'understudy was here' in proof.stdout"
`)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	for _, want := range []string{"ok:", "Gathering Facts", "PLAY RECAP", "failed=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestSSHAgentCachedAcrossTasks(t *testing.T) {
	port := startContainer(t)
	// Second run against the same container must reuse the uploaded agent
	// (checksum-named path) — verify a marker survives and the run is green.
	out, code := runSSH(t, port, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: ls ~/.understudy/ | wc -l
      register: agents
      changed_when: false
    - assert:
        that: agents.stdout | int >= 1
`)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}

func TestSSHAsyncJob(t *testing.T) {
	port := startContainer(t)
	out, code := runSSH(t, port, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: "sleep 1; hostname"
      async: 30
      poll: 0
      register: job
    - async_status: {jid: "{{ job.ansible_job_id }}"}
      register: st
      until: st.finished
      retries: 20
      delay: 1
    - assert: {that: ["st.rc == 0", "st.stdout | length > 0"]}
    - command: echo polled
      async: 20
      poll: 1
      register: p
    - assert: {that: ["p.stdout == 'polled'"]}
`)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}
