//go:build e2e

// community.mysql differential e2e: an Ubuntu SSH target running MariaDB
// (with python3-pymysql for Ansible's modules) plus a MySQL 8.0 server on
// the same docker network. test/e2e/mysql/*.yml runs through real
// ansible-playbook (with community.mysql / ansible.mysql installed) and
// through understudy, and the -v output must match after normalizing the
// few inherently volatile values. Without a usable ansible-playbook the
// understudy run alone must succeed.
//
// Run with: go test -tags e2e -run TestMySQL ./test/e2e/
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	myNet    = "understudy-mysql-net"
	myTarget = "understudy-mysql-target"
	myMySQL8 = "understudy-mysql8"
	myUser   = "tester"
)

func myDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// startMySQLEnv boots the target and the MySQL 8.0 server, returning the
// target's SSH port and a private key for tester.
func startMySQLEnv(t *testing.T) (port, key string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	dir := t.TempDir()
	dockerfile := `FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server sudo python3 python3-pymysql python3-cryptography mariadb-server mariadb-client gzip && \
    mkdir /run/sshd && useradd -m -s /bin/bash ` + myUser + ` && \
    echo '` + myUser + ` ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/` + myUser + `
CMD ["sh", "-c", "service mariadb start && exec /usr/sbin/sshd -D"]`
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644)
	myDocker(t, "build", "-q", "-t", myTarget+"-img", dir)

	key = filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")

	for _, c := range []string{myTarget, myMySQL8} {
		exec.Command("docker", "rm", "-f", c).Run()
	}
	exec.Command("docker", "network", "rm", myNet).Run()
	myDocker(t, "network", "create", myNet)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", myTarget, myMySQL8).Run()
		exec.Command("docker", "network", "rm", myNet).Run()
	})
	myDocker(t, "run", "-d", "--name", myMySQL8, "--network", myNet, "--network-alias", "mysql8",
		"-e", "MYSQL_ROOT_PASSWORD=rootpw", "mysql:8.0")
	myDocker(t, "run", "-d", "--name", myTarget, "--network", myNet, "-p", "0:22", myTarget+"-img")

	install := fmt.Sprintf("install -d -m700 -o %[1]s -g %[1]s /home/%[1]s/.ssh && printf '%%s' %[2]q > /home/%[1]s/.ssh/authorized_keys && "+
		"chown %[1]s:%[1]s /home/%[1]s/.ssh/authorized_keys && chmod 600 /home/%[1]s/.ssh/authorized_keys",
		myUser, strings.TrimSpace(string(pub)))
	myDocker(t, "exec", myTarget, "sh", "-c", install)

	// Wait for both servers to accept queries.
	deadline := time.Now().Add(3 * time.Minute)
	for _, probe := range [][]string{
		{"exec", myTarget, "mariadb", "-e", "SELECT 1"},
		{"exec", myMySQL8, "mysql", "-uroot", "-prootpw", "-h127.0.0.1", "-e", "SELECT 1"},
	} {
		for exec.Command("docker", probe...).Run() != nil {
			if time.Now().After(deadline) {
				t.Fatalf("database never came up: %v", probe)
			}
			time.Sleep(2 * time.Second)
		}
	}
	// The MySQL server's auto-generated CA, for verified TLS connections.
	ca := filepath.Join(dir, "ca.pem")
	myDocker(t, "cp", myMySQL8+":/var/lib/mysql/ca.pem", ca)
	myDocker(t, "cp", ca, myTarget+":/etc/mysql8-ca.pem")
	myDocker(t, "exec", myTarget, "chmod", "644", "/etc/mysql8-ca.pem")
	// The published port can take a moment to show up.
	var out string
	for i := 0; ; i++ {
		b, err := exec.Command("docker", "port", myTarget, "22").CombinedOutput()
		if out = string(b); err == nil {
			break
		}
		if i == 20 {
			t.Fatalf("docker port: %v\n%s", err, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
	line := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	return line[strings.LastIndexByte(line, ':')+1:], key
}

// resetMySQLEnv drops everything the corpus creates.
func resetMySQLEnv(t *testing.T) {
	exec.Command("docker", "exec", myTarget, "sh", "-c",
		"rm -f /tmp/e2e_*.sql* /tmp/x.sql /home/tester/.my.cnf; mariadb -e \"DROP DATABASE IF EXISTS e2e_app; DROP DATABASE IF EXISTS e2e_other; DROP USER IF EXISTS 'e2e_bob'@'%'; DROP USER IF EXISTS 'e2e_dumper'@'localhost'\"").Run()
	exec.Command("docker", "exec", myMySQL8, "mysql", "-uroot", "-prootpw", "-h127.0.0.1", "-e",
		"DROP USER IF EXISTS 'e2e_carol'@'localhost'; DROP DATABASE IF EXISTS e2e_cfg; SET GLOBAL max_connections = 151").Run()
}

var (
	myVolatile = []*regexp.Regexp{
		regexp.MustCompile(`"execution_time_ms": \[[^\]]*\]`),
		regexp.MustCompile(`"src": "[^"]*ansible-tmp-[^"]*"`),
	}
	// Python set iteration order is randomized per process.
	myPrivLists = regexp.MustCompile(`(granted|revoked) \[([^\]]*)\]`)
	// Multi-line (-v debug) renderings of the execution_time_ms list.
	myTimingBlock = regexp.MustCompile(`(?s)"execution_time_ms": \[\n.*?\]`)
)

func normalizeMySQLRun(s string) string {
	s = myTimingBlock.ReplaceAllString(s, `"execution_time_ms": [...]`)
	for _, re := range myVolatile {
		s = re.ReplaceAllStringFunc(s, func(m string) string { return m[:strings.IndexByte(m, ':')] + ": X" })
	}
	s = myPrivLists.ReplaceAllStringFunc(s, func(m string) string {
		sm := myPrivLists.FindStringSubmatch(m)
		items := strings.Split(sm[2], ", ")
		sort.Strings(items)
		return sm[1] + " [" + strings.Join(items, ", ") + "]"
	})
	return s
}

func ansibleHasMySQL(ansible string) bool {
	galaxy := filepath.Join(filepath.Dir(ansible), "ansible-galaxy")
	out, err := exec.Command(galaxy, "collection", "list", "community.mysql").CombinedOutput()
	return err == nil && strings.Contains(string(out), "community.mysql")
}

func TestMySQLDifferential(t *testing.T) {
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("bin/understudy not built (run: make build)")
	}
	ansible := os.Getenv("UNDERSTUDY_ANSIBLE_PLAYBOOK")
	if ansible == "" {
		ansible, _ = exec.LookPath("ansible-playbook")
	}
	if ansible != "" && !ansibleHasMySQL(ansible) {
		t.Logf("ansible-playbook lacks community.mysql: running understudy only")
		ansible = ""
	}
	port, key := startMySQLEnv(t)

	corpus, _ := filepath.Glob("mysql/*.yml")
	if len(corpus) == 0 {
		t.Fatal("no mysql corpus")
	}
	env := append(os.Environ(), "NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_PYTHON_INTERPRETER=/usr/bin/python3")
	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			inv := filepath.Join(t.TempDir(), "hosts")
			os.WriteFile(inv, []byte(fmt.Sprintf(
				"target ansible_host=127.0.0.1 ansible_port=%s ansible_user=%s ansible_ssh_private_key_file=%s\n",
				port, myUser, key)), 0o644)
			run := func(name string, args ...string) (string, int) {
				resetMySQLEnv(t)
				cmd := exec.Command(name, args...)
				cmd.Env = env
				out, _ := cmd.Output() // stdout only: warnings go to stderr
				return string(out), cmd.ProcessState.ExitCode()
			}
			uOut, uCode := run(bin, "playbook", "-v", "-i", inv, pb)
			if uCode != 0 {
				t.Fatalf("understudy exited %d\n%s", uCode, uOut)
			}
			if ansible == "" {
				return
			}
			aOut, aCode := run(ansible, "-v", "-i", inv, pb)
			if aCode != 0 {
				t.Fatalf("ansible-playbook exited %d\n%s", aCode, aOut)
			}
			a, u := normalizeMySQLRun(aOut), normalizeMySQLRun(uOut)
			// The config-file banner names the file ansible found (or not).
			a = strings.SplitN(a, "\n", 2)[1]
			u = strings.SplitN(u, "\n", 2)[1]
			if a != u {
				t.Errorf("output differs\n%s", lineDiff(a, u))
			}
		})
	}
}

// lineDiff shows the differing lines of two outputs.
func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out strings.Builder
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			fmt.Fprintf(&out, "line %d:\n  ansible:    %s\n  understudy: %s\n", i+1, x, y)
		}
	}
	return out.String()
}
