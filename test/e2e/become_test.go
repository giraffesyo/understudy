//go:build e2e

// become_method su and doas end-to-end: both prompt for a password on a
// terminal, so understudy drives them through an SSH pseudo-terminal.
// Ubuntu (util-linux su, root password set, no sudo) and Alpine (doas,
// password rule for the login user). Run with: go test -tags e2e ./test/e2e/
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

const rootPass = "r00tpass"

// startBecomeContainer builds and runs an sshd image, returning the port.
func startBecomeContainer(t *testing.T, name, dockerfile string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", name+"-img", dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "-p", "0:22", name+"-img").CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	out, err := exec.Command("docker", "port", name, "22").Output()
	if err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	port := line[strings.LastIndexByte(line, ':')+1:]
	for i := 0; i < 30; i++ {
		if exec.Command("docker", "exec", name, "pgrep", "sshd").Run() == nil {
			time.Sleep(500 * time.Millisecond)
			return port
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("sshd never came up")
	return ""
}

// runBecome runs a playbook against the container with extra host vars.
func runBecome(t *testing.T, port, hostVars, playbookSrc string) (string, int) {
	t.Helper()
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built (run: make build)")
	}
	dir := t.TempDir()
	pb := filepath.Join(dir, "play.yml")
	os.WriteFile(pb, []byte(playbookSrc), 0o644)
	inv := filepath.Join(dir, "hosts")
	os.WriteFile(inv, []byte(fmt.Sprintf(
		"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s ansible_password=%s %s\n",
		port, testUser, testPass, hostVars)), 0o644)
	cmd := exec.Command(bin, "playbook", "-i", inv, pb)
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "NO_COLOR=1")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestBecomeSu(t *testing.T) {
	port := startBecomeContainer(t, "understudy-e2e-su", `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y openssh-server && mkdir /run/sshd && \
    useradd -m -s /bin/bash `+testUser+` && echo '`+testUser+`:`+testPass+`' | chpasswd && \
    echo 'root:`+rootPass+`' | chpasswd
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D"]`)

	out, code := runBecome(t, port, "ansible_become_password="+rootPass, `
- hosts: all
  gather_facts: false
  become: true
  become_method: su
  tasks:
    - command: id -un
      register: who
    - assert: {that: who.stdout == "root"}
    - name: module payloads travel through the staged stdin
      copy: {content: "via su\n", dest: /root/su-proof.txt, mode: "0600"}
    - command: cat /root/su-proof.txt
      register: proof
    - assert: {that: proof.stdout == "via su"}
    - name: without flags su keeps the login user's directory
      shell: pwd
      register: nolog
    - name: become_exe and become_flags take effect (-l starts in root's home)
      shell: pwd
      become_exe: /usr/bin/su
      become_flags: -l
      register: login
    - assert: {that: ["nolog.stdout != '/root'", "login.stdout == '/root'"]}
    - name: a bogus become_exe fails
      command: "true"
      become_exe: /nonexistent/su
      register: bogus
      ignore_errors: true
    - assert: {that: bogus is failed}
    - name: a failing command keeps its rc through su
      command: sh -c 'exit 4'
      register: rc4
      failed_when: rc4.rc != 4
    - name: unescalated task in the same play
      command: id -un
      become: false
      register: plain
    - assert: {that: plain.stdout == "`+testUser+`"}
`)
	if code != 0 || !strings.Contains(out, "failed=0") {
		t.Fatalf("su run failed (exit %d):\n%s", code, out)
	}

	out, _ = runBecome(t, port, "ansible_become_password=wrong", `
- hosts: all
  gather_facts: false
  tasks:
    - command: id -un
      become: true
      become_method: su
`)
	if !strings.Contains(out, "Incorrect su password") {
		t.Errorf("wrong password: want 'Incorrect su password' in:\n%s", out)
	}

	out, _ = runBecome(t, port, "", `
- hosts: all
  gather_facts: false
  tasks:
    - command: id -un
      become: true
      become_method: su
`)
	if !strings.Contains(out, "Missing su password") {
		t.Errorf("no password: want 'Missing su password' in:\n%s", out)
	}

	// Host variables select the method and outrank keywords.
	out, code = runBecome(t, port, "ansible_become=true ansible_become_method=su ansible_su_pass="+rootPass, `
- hosts: all
  gather_facts: false
  become_method: sudo
  tasks:
    - command: id -un
      register: who
    - assert: {that: who.stdout == "root"}
`)
	if code != 0 {
		t.Errorf("host-var su failed (exit %d):\n%s", code, out)
	}
}

func TestBecomeDoas(t *testing.T) {
	port := startBecomeContainer(t, "understudy-e2e-doas", `FROM alpine:3.20
RUN apk add --no-cache openssh doas && ssh-keygen -A && \
    adduser -D -s /bin/sh `+testUser+` && echo '`+testUser+`:`+testPass+`' | chpasswd && \
    mkdir -p /etc/doas.d && echo 'permit `+testUser+` as root' > /etc/doas.d/doas.conf && \
    sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]`)

	out, code := runBecome(t, port, "ansible_become_password="+testPass, `
- hosts: all
  gather_facts: false
  become: true
  become_method: community.general.doas
  tasks:
    - command: id -un
      register: who
    - assert: {that: who.stdout == "root"}
    - copy: {content: "via doas\n", dest: /root/doas-proof.txt}
    - command: cat /root/doas-proof.txt
      register: proof
    - assert: {that: proof.stdout == "via doas"}
`)
	if code != 0 || !strings.Contains(out, "failed=0") {
		t.Fatalf("doas run failed (exit %d):\n%s", code, out)
	}

	out, _ = runBecome(t, port, "", `
- hosts: all
  gather_facts: false
  tasks:
    - command: id -un
      become: true
      become_method: doas
`)
	if !strings.Contains(out, "Missing doas password") {
		t.Errorf("no password: want 'Missing doas password' in:\n%s", out)
	}
	out, _ = runBecome(t, port, "ansible_become_password=nope", `
- hosts: all
  gather_facts: false
  tasks:
    - command: id -un
      become: true
      become_method: doas
`)
	if !strings.Contains(out, "Incorrect doas password") {
		t.Errorf("wrong password: want 'Incorrect doas password' in:\n%s", out)
	}
}
