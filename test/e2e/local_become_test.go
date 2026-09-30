//go:build e2e

// become on the local connection end-to-end: understudy runs inside an
// Ubuntu container as an unprivileged user (-c local) and escalates with
// the real sudo and su. Run with: go test -tags e2e ./test/e2e/
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startLocalBecomeContainer runs an image with sudo (NOPASSWD for nopw,
// password rule for pw), a root password for su, and a linux build of
// understudy at /usr/local/bin/understudy.
func startLocalBecomeContainer(t *testing.T) string {
	t.Helper()
	dockerAvailable(t)
	arch, err := exec.Command("docker", "version", "-f", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "understudy")
	build := exec.Command("go", "build", "-o", bin, "./cmd/understudy")
	build.Dir = "../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building understudy for linux: %v\n%s", err, out)
	}
	// The image carries no understudy build (each worktree has its own);
	// the binary is copied into this process's container instead.
	dockerfile := `FROM ubuntu:24.04
RUN apt-get update && apt-get install -y sudo && \
    useradd -m -s /bin/bash nopw && useradd -m -s /bin/bash pw && echo 'pw:pwpass' | chpasswd && \
    echo 'root:` + rootPass + `' | chpasswd && \
    echo 'nopw ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/nopw && echo 'pw ALL=(ALL) ALL' > /etc/sudoers.d/pw
CMD ["sleep", "infinity"]`
	image := dockerBuild(t, "understudy-e2e-local-become-img", dockerfile, nil)
	name := dockerRun(t, "understudy-e2e-local-become", image)
	if out, err := exec.Command("docker", "cp", bin, name+":/usr/local/bin/understudy").CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	exec.Command("docker", "exec", name, "chmod", "755", "/usr/local/bin/understudy").Run()
	for i := 0; i < 20; i++ {
		if exec.Command("docker", "exec", name, "true").Run() == nil {
			return name
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("container never came up")
	return ""
}

// runLocalBecome runs a playbook inside the container as user over the
// local connection.
func runLocalBecome(t *testing.T, name, user, extraVars, playbookSrc string) (string, int) {
	t.Helper()
	pb := filepath.Join(t.TempDir(), "play.yml")
	os.WriteFile(pb, []byte(playbookSrc), 0o644)
	if out, err := exec.Command("docker", "cp", pb, name+":/tmp/play.yml").CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	exec.Command("docker", "exec", name, "chmod", "644", "/tmp/play.yml").Run()
	args := []string{"exec", "-u", user, "-w", "/tmp", "-e", "NO_COLOR=1", name,
		"understudy", "playbook", "-i", "localhost,", "-c", "local", "/tmp/play.yml"}
	if extraVars != "" {
		args = append(args, "-e", extraVars)
	}
	cmd := exec.Command("docker", args...)
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestLocalBecome(t *testing.T) {
	name := startLocalBecomeContainer(t)

	play := `
- hosts: localhost
  gather_facts: false
  tasks:
    - command: id -un
      become: true
      register: who
    - assert: {that: who.stdout == "root"}
    - name: module payloads reach the escalated module
      copy: {content: "via local sudo\n", dest: /root/proof.txt, mode: "0600"}
      become: true
    - command: cat /root/proof.txt
      become: true
      register: proof
    - assert: {that: proof.stdout == "via local sudo"}
    - raw: id -un
      become: true
      register: rawwho
    - assert: {that: rawwho.stdout == "root\n"}
    - name: become_user
      command: id -un
      become: true
      become_user: nobody
      register: nobody
    - assert: {that: nobody.stdout == "nobody"}
    - command: id -un
      register: plain
    - assert: {that: plain.stdout != "root"}
    - name: facts gathered as root
      setup: {gather_subset: ['!all']}
      become: true
    - assert: {that: ansible_facts.user_id == "root"}
`
	out, code := runLocalBecome(t, name, "nopw", "", play)
	if code != 0 || !strings.Contains(out, "failed=0") {
		t.Fatalf("NOPASSWD sudo run failed (exit %d):\n%s", code, out)
	}

	out, code = runLocalBecome(t, name, "pw", "ansible_become_password=pwpass", play)
	if code != 0 || !strings.Contains(out, "failed=0") {
		t.Fatalf("password sudo run failed (exit %d):\n%s", code, out)
	}

	// Never silently unprivileged: without the password sudo refuses.
	out, code = runLocalBecome(t, name, "pw", "", `
- hosts: localhost
  gather_facts: false
  tasks:
    - command: id -un
      become: true
`)
	if code != 2 || !strings.Contains(out, "Task failed: Premature end of stream waiting for become success.") ||
		!strings.Contains(out, "sudo: a password is required") {
		t.Errorf("missing sudo password (exit %d):\n%s", code, out)
	}

	out, code = runLocalBecome(t, name, "nopw", "ansible_become_password="+rootPass, `
- hosts: localhost
  gather_facts: false
  become: true
  become_method: su
  tasks:
    - command: id -un
      register: who
    - assert: {that: who.stdout == "root"}
    - copy: {content: "via local su\n", dest: /root/su-proof.txt}
`)
	if code != 0 || !strings.Contains(out, "failed=0") {
		t.Errorf("su run failed (exit %d):\n%s", code, out)
	}
}
