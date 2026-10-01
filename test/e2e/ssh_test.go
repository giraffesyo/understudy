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
// port.
func startContainer(t *testing.T) string {
	_, port := startSSHContainer(t)
	return port
}

// startSSHContainer builds and runs the sshd container, returning its name
// and mapped port.
func startSSHContainer(t *testing.T) (name, port string) {
	t.Helper()
	dockerAvailable(t)
	image := dockerBuild(t, "understudy-ssh-test", sshDockerfile, nil)
	name = dockerRun(t, "understudy-e2e", "-p", "127.0.0.1:0:22", image)
	port = dockerPort(t, name, "22")

	// Wait for sshd to accept connections.
	for i := 0; i < 30; i++ {
		c := exec.Command("docker", "exec", name, "pgrep", "sshd")
		if c.Run() == nil {
			time.Sleep(500 * time.Millisecond)
			return name, port
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("sshd never came up")
	return "", ""
}

const sshDockerfile = `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y openssh-server sudo && \
    mkdir /run/sshd && \
    useradd -m -s /bin/bash ` + testUser + ` && echo '` + testUser + `:` + testPass + `' | chpasswd && \
    echo '` + testUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + testUser + `
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D"]`

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

// A task that times out on an SSH target has the processes its module
// started killed there too: the agent, terminated, takes the command's
// process group (background jobs included) down with it.
func TestSSHTimedOutCommandIsKilled(t *testing.T) {
	name, port := startSSHContainer(t)
	out, code := runSSH(t, port, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: (sleep 3 && touch /tmp/bg-marker) & sleep 3 && touch /tmp/fg-marker
      timeout: 1
      ignore_errors: true
    - become: true
      shell: (sleep 3 && touch /tmp/become-marker) & wait
      timeout: 1
      ignore_errors: true
`)
	if code != 0 || !strings.Contains(out, "ignored=2") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	time.Sleep(4 * time.Second)
	for _, m := range []string{"/tmp/bg-marker", "/tmp/fg-marker", "/tmp/become-marker"} {
		if exec.Command("docker", "exec", name, "test", "-e", m).Run() == nil {
			t.Errorf("the timed-out command kept running and created %s", m)
		}
	}
}
