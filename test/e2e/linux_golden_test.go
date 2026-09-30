//go:build golden

// Linux module golden output: each playbook under golden/linux/ runs at -v
// through real ansible-playbook and through understudy, each against its
// own fresh container of the same image (so both start from identical
// state), and stdout must match byte for byte. This covers modules that
// need root on a real Linux target (user, hostname, alternatives, ...).
//
// A playbook's first line may restrict the images it runs on:
//
//	# distros: ubuntu alpine
//
// Requires docker and ansible-playbook; run with `make test-golden`.
package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const lgUser = "tester"

// lgImages are the target images: each runs sshd in the foreground with
// python3 (for ansible) and a passwordless-sudo user.
var lgImages = map[string]string{
	"ubuntu": `FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y openssh-server sudo python3 git && \
    mkdir -p /run/sshd && ssh-keygen -A && \
    useradd -m -s /bin/bash ` + lgUser + ` && \
    echo '` + lgUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + lgUser + `
CMD ["/usr/sbin/sshd", "-D", "-e"]`,
	"alpine": `FROM alpine:3.20
RUN apk add --no-cache openssh sudo python3 git && ssh-keygen -A && \
    adduser -D -s /bin/sh ` + lgUser + ` && \
    sed -i 's/^` + lgUser + `:!/` + lgUser + `:*/' /etc/shadow && \
    echo '` + lgUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + lgUser + `
CMD ["/usr/sbin/sshd", "-D", "-e"]`,
	"rocky": `FROM rockylinux/rockylinux:9
RUN printf 'keepcache=1\nmetadata_expire=-1\n' >> /etc/dnf/dnf.conf && \
    dnf -y install openssh-server openssh-clients sudo python3 chkconfig git-core && \
    dnf -y install --downloadonly tree zip && ssh-keygen -A && \
    useradd -m ` + lgUser + ` && \
    echo '` + lgUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + lgUser + `
CMD ["/usr/sbin/sshd", "-D", "-e"]`,
	// rocky-systemd boots systemd (sshd as a unit) for the service,
	// firewalld and selinux modules; only playbooks that ask for it run
	// on it.
	"rocky-systemd": `FROM rockylinux/rockylinux:9
RUN dnf -y install openssh-server sudo python3 systemd procps-ng firewalld \
      python3-libselinux selinux-policy-targeted policycoreutils && \
    ssh-keygen -A && systemctl enable sshd firewalld && \
    sed -i 's/^IPv6_rpfilter=.*/IPv6_rpfilter=no/' /etc/firewalld/firewalld.conf && \
    useradd -m ` + lgUser + ` && \
    echo '` + lgUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + lgUser + `
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/sbin/init"]`,
}

// lgRunArgs are extra docker run arguments per image.
var lgRunArgs = map[string][]string{
	"rocky-systemd": {"--cgroupns=host", "-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw"},
}

// lgReady is a command that must succeed in a booted container before a
// tool runs against it (services that start after sshd). (Docker
// Desktop's kernel lacks the fib expression firewalld's IPv6 rpfilter
// needs, so the image turns that off.)
var lgReady = map[string]string{
	"rocky-systemd": "systemctl is-system-running --wait >/dev/null; firewall-cmd --state",
}

var lgBuild sync.Map // distro -> *lgBuilt

type lgBuilt struct {
	once sync.Once
	tag  string
	err  error
}

func lgImage(t *testing.T, distro string) string {
	t.Helper()
	v, _ := lgBuild.LoadOrStore(distro, &lgBuilt{})
	b := v.(*lgBuilt)
	b.once.Do(func() {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(lgImages[distro]), 0o644)
		// Content-addressed, like dockerBuild's tags.
		sum := sha256.Sum256([]byte(lgImages[distro]))
		b.tag = fmt.Sprintf("understudy-lg-%s:%x", distro, sum[:8])
		if out, err := exec.Command("docker", "build", "-q", "-t", b.tag, dir).CombinedOutput(); err != nil {
			b.err = fmt.Errorf("docker build %s: %v\n%s", distro, err, out)
		}
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	return b.tag
}

// lgBoot starts a fresh container (extra: more docker run arguments) and
// returns its name and ssh port.
func lgBoot(t *testing.T, image, base, pub string, extra ...string) (name, port string) {
	t.Helper()
	args := append(append([]string{"--privileged", "--hostname", "golden.example.com", "-p", "127.0.0.1:0:22"}, extra...), image)
	name = dockerRun(t, base, args...)
	install := fmt.Sprintf("set -e; h=$(getent passwd %[1]s | cut -d: -f6); mkdir -p $h/.ssh; "+
		"printf '%%s\\n' %[2]q > $h/.ssh/authorized_keys; chown -R %[1]s $h/.ssh; "+
		"chmod 700 $h/.ssh; chmod 600 $h/.ssh/authorized_keys", lgUser, pub)
	if out, err := exec.Command("docker", "exec", name, "sh", "-c", install).CombinedOutput(); err != nil {
		t.Fatalf("install key: %v\n%s", err, out)
	}
	port = dockerPort(t, name, "22")
	for i := 0; i < 60; i++ {
		if out, _ := exec.Command("ssh-keyscan", "-p", port, "127.0.0.1").Output(); len(out) > 0 {
			return name, port
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("sshd in %s never came up", name)
	return "", ""
}

var lgDistros = regexp.MustCompile(`^#\s*distros:\s*(.*)`)

func TestLinuxGoldenOutput(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	corpus, err := filepath.Glob("golden/linux/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no linux golden corpus: %v", err)
	}
	keyDir := t.TempDir()
	keyFile := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(keyFile + ".pub")
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_SYSTEM_WARNINGS=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_LOCALHOST_WARNING=False"}

	for _, pb := range corpus {
		data, _ := os.ReadFile(pb)
		distros := []string{"ubuntu", "alpine", "rocky"}
		if m := lgDistros.FindStringSubmatch(strings.SplitN(string(data), "\n", 2)[0]); m != nil {
			distros = strings.Fields(m[1])
		}
		for _, distro := range distros {
			t.Run(filepath.Base(pb)+"/"+distro, func(t *testing.T) {
				image := lgImage(t, distro)
				abs, _ := filepath.Abs(pb)
				var lastStderr string
				run := func(tool string, bin string, pre ...string) string {
					name, port := lgBoot(t, image, "understudy-lg-"+distro+"-"+tool, strings.TrimSpace(string(pub)), lgRunArgs[distro]...)
					if ready := lgReady[distro]; ready != "" {
						ok := false
						for i := 0; i < 60 && !ok; i++ {
							ok = exec.Command("docker", "exec", name, "sh", "-c", ready).Run() == nil
							if !ok {
								time.Sleep(time.Second)
							}
						}
						if !ok {
							t.Fatalf("%s never became ready (%s)", name, ready)
						}
					}
					dir := t.TempDir()
					inv := filepath.Join(dir, "hosts")
					os.WriteFile(inv, []byte(fmt.Sprintf(
						"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s "+
							"ansible_ssh_private_key_file=%s ansible_python_interpreter=/usr/bin/python3\n",
						port, lgUser, keyFile)), 0o644)
					args := append(append(pre, "-v", "-f", "1", "-i", inv, "-e", "distro="+distro), abs)
					cmd := exec.Command(bin, args...)
					cmd.Env = append(os.Environ(), env...)
					var stderr strings.Builder
					cmd.Stderr = &stderr
					out, _ := cmd.Output()
					lastStderr = stderr.String()
					return normalizeVerbose(normalizeOutput(string(out), dir))
				}
				want := run("ansible", ansible)
				if !strings.Contains(want, "PLAY RECAP") || strings.Contains(want, "UNREACHABLE!") {
					t.Fatalf("ansible-playbook did not run the play:\n%s\n%s", want, lastStderr)
				}
				got := run("understudy", understudy, "playbook")
				if os.Getenv("UNDERSTUDY_GOLDEN_LOG") != "" {
					t.Logf("--- ansible ---\n%s\n--- understudy ---\n%s", want, got)
				}
				if got != want {
					t.Errorf("stdout differs from ansible-playbook:\n%s", lineDiff(want, got))
				}
			})
		}
	}
}
