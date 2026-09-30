//go:build golden

// RHEL golden differential: runs the same playbook through real
// ansible-playbook AND understudy against a live Rocky Linux container over
// SSH, then asserts identical per-task status and PLAY RECAP. This catches
// module-behavior divergences on a real target (e.g. file-ownership
// preservation) automatically, the way the local golden test catches
// template/filter divergences. Reuses parseRun/recap/diff helpers from
// golden_test.go (same build tag).
//
// Requires docker (systemd-in-container) and ansible-playbook. Run with:
//
//	make test-golden   (or: go test -tags golden ./test/e2e/)
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const rgUser = "tester"
const rgImage = "understudy-rhelgolden-img" // distinct from rhel_test.go's image

// bootRockyKeyAuth boots the systemd Rocky container with SSH key auth for
// `tester` (passwordless sudo) and returns the ssh port and private-key path.
func bootRockyKeyAuth(t *testing.T) (port, keyFile string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}

	// Build the image if it isn't already present (Dockerfile lives in
	// rhel_test.go; rebuild here so this test is independent of test order).
	dir := t.TempDir()
	// procps-ng provides the `sysctl` binary that the sysctl module (in
	// both tools) shells out to. A distinct image tag avoids clashing with
	// rhel_test.go's image. The corpus's packages (zip, chrony) and the repo
	// metadata are cached in the image and never expire, so the runs don't
	// depend on mirrors: a mirror hiccup during one tool's run (and not the
	// other's) would fail the comparison.
	dockerfile := `FROM rockylinux:9
RUN printf 'keepcache=1\nmetadata_expire=-1\n' >> /etc/dnf/dnf.conf && \
    dnf -y install openssh-server sudo systemd procps-ng && \
    dnf -y install --downloadonly zip chrony && ssh-keygen -A && \
    useradd -m ` + rgUser + ` && \
    echo '` + rgUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + rgUser + ` && \
    systemctl enable sshd
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/sbin/init"]`
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644)
	if out, err := exec.Command("docker", "build", "-q", "-t", rgImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}

	// Generate a keypair.
	keyFile = filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(keyFile + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	name := "understudy-rhelgolden"
	exec.Command("docker", "rm", "-f", name).Run()
	runArgs := []string{"run", "-d", "--name", name, "--privileged", "--cgroupns=host",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw", "-p", "0:22", rgImage, "/usr/sbin/init"}
	if out, err := exec.Command("docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })

	// Wait for sshd, then install the authorized key.
	for i := 0; ; i++ {
		out, _ := exec.Command("docker", "exec", name, "systemctl", "is-active", "sshd").Output()
		if strings.TrimSpace(string(out)) == "active" {
			break
		}
		if i == 60 {
			t.Fatalf("sshd not active in the container after 60s: %s", out)
		}
		time.Sleep(time.Second)
	}
	install := fmt.Sprintf(
		"install -d -m700 -o %s -g %s /home/%s/.ssh && "+
			"printf '%%s' %q > /home/%s/.ssh/authorized_keys && "+
			"chown %s:%s /home/%s/.ssh/authorized_keys && chmod 600 /home/%s/.ssh/authorized_keys",
		rgUser, rgUser, rgUser, strings.TrimSpace(string(pub)), rgUser, rgUser, rgUser, rgUser, rgUser)
	if out, err := exec.Command("docker", "exec", name, "sh", "-c", install).CombinedOutput(); err != nil {
		t.Fatalf("install key: %v\n%s", err, out)
	}

	portOut, err := exec.Command("docker", "port", name, "22").Output()
	if err != nil {
		t.Fatal(err)
	}
	line := strings.SplitN(strings.TrimSpace(string(portOut)), "\n", 2)[0]
	return line[strings.LastIndexByte(line, ':')+1:], keyFile
}

func TestRHELGoldenDifferential(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	port, keyFile := bootRockyKeyAuth(t)

	corpus, err := filepath.Glob("golden/rhel/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no RHEL golden corpus: %v", err)
	}

	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_SYSTEM_WARNINGS=False",
		"ANSIBLE_COMMAND_WARNINGS=False", "ANSIBLE_ACTION_WARNINGS=False"}

	writeInv := func(dir string) string {
		inv := filepath.Join(dir, "hosts")
		os.WriteFile(inv, []byte(fmt.Sprintf(
			"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s ansible_ssh_private_key_file=%s\n",
			port, rgUser, keyFile)), 0o644)
		return inv
	}

	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			// A fresh workdir var per tool keeps their file paths distinct.
			invA := writeInv(t.TempDir())
			invB := writeInv(t.TempDir())
			// Reset all state the corpus mutates so both tools start from an
			// identical clean slate (ansible runs first, then understudy).
			// Covers every mutation across the RHEL corpus files.
			resetCmd := "rm -rf /etc/understudy-golden /tmp/understudy-golden*; " +
				"userdel -r uduser 2>/dev/null; groupdel udgrp 2>/dev/null; " +
				"userdel -r deploy 2>/dev/null; groupdel deploy 2>/dev/null; groupdel wheel2 2>/dev/null; " +
				"systemctl disable --now chronyd 2>/dev/null; " +
				"echo 0 > /proc/sys/net/ipv4/ip_forward 2>/dev/null; " +
				"sed -i '/net.ipv4.ip_forward/d' /etc/sysctl.conf 2>/dev/null; " +
				"dnf -y remove zip chrony 2>/dev/null; " +
				"umount /etc/hostname 2>/dev/null; hostnamectl set-hostname rhelgolden.example 2>/dev/null; true"
			exec.Command("docker", "exec", "understudy-rhelgolden", "sh", "-c", resetCmd).Run()
			aOut := runTool(t, ansible, []string{"-i", invA, pb}, env, 1)

			exec.Command("docker", "exec", "understudy-rhelgolden", "sh", "-c", resetCmd).Run()
			uOut := runTool(t, understudy, []string{"playbook", "-i", invB, pb}, env, 1)

			if os.Getenv("UNDERSTUDY_GOLDEN_LOG") != "" {
				t.Logf("--- ansible ---\n%s\n--- understudy ---\n%s", aOut, uOut)
			}
			aStatus, aRecap := parseRun(aOut)
			uStatus, uRecap := parseRun(uOut)
			if !reflect.DeepEqual(aRecap, uRecap) {
				t.Errorf("PLAY RECAP differs\n ansible:    %v\n understudy: %v\n\n--- ansible ---\n%s\n--- understudy ---\n%s",
					sortRecap(aRecap), sortRecap(uRecap), aOut, uOut)
			}
			if !reflect.DeepEqual(aStatus, uStatus) {
				t.Errorf("per-task status differs\n%s\n\n--- ansible ---\n%s\n--- understudy ---\n%s",
					diffStatus(aStatus, uStatus), aOut, uOut)
			}
		})
	}
}
