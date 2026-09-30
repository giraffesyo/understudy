//go:build golden

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenExitCodes runs golden/exitcodes playbooks, whose outcome is
// the exit code (the result of the last play run, carried-over failed and
// unreachable hosts, max_fail_percentage, several playbooks, --step), and
// compares the exit code, stdout and stderr byte for byte. Per case:
// "<name>.inventory" is the inventory (default: hosts.inventory, h1 in
// group a and h2 in group b),
// "<name>.args" holds extra arguments, one per line (a relative playbook
// path resolves against golden/exitcodes, whose books/ directory holds
// the playbooks that run after <name>.yml), and "<name>.stdin" is fed to
// standard input.
func TestGoldenExitCodes(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	corpus, err := filepath.Glob("golden/exitcodes/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no exit-code corpus found: %v", err)
	}
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_DEPRECATION_WARNINGS=False",
		"ANSIBLE_SYSTEM_WARNINGS=False", "ANSIBLE_RETRY_FILES_ENABLED=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	for _, pb := range corpus {
		base := strings.TrimSuffix(pb, ".yml")
		t.Run(filepath.Base(pb), func(t *testing.T) {
			abs, _ := filepath.Abs(pb)
			var extra []string
			if data, err := os.ReadFile(base + ".args"); err == nil {
				for _, arg := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					if strings.HasSuffix(arg, ".yml") && !filepath.IsAbs(arg) {
						arg, _ = filepath.Abs(filepath.Join(filepath.Dir(pb), arg))
					}
					extra = append(extra, arg)
				}
			}
			stdin, _ := os.ReadFile(base + ".stdin")
			run := func(bin string, pre ...string) (string, string, int) {
				work := t.TempDir()
				invSrc := base + ".inventory"
				if _, err := os.Stat(invSrc); err != nil {
					invSrc = filepath.Join(filepath.Dir(pb), "hosts.inventory")
				}
				inv := writeGoldenInventory(t, work, invSrc)
				args := append(pre, "-f", "1", "-i", inv, "-c", "local", "-e", "workdir="+work, abs)
				cmd := exec.Command(bin, append(args, extra...)...)
				cmd.Env = append(os.Environ(), env...)
				cmd.Stdin = bytes.NewReader(stdin)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				cmd.Run()
				return normalizeOutput(stdout.String(), work), normalizeOutput(stderr.String(), work), cmd.ProcessState.ExitCode()
			}
			aOut, aErr, aRC := run(ansible)
			uOut, uErr, uRC := run(understudy, "playbook")
			if uRC != aRC {
				t.Errorf("exit code %d, ansible-playbook %d", uRC, aRC)
			}
			if uOut != aOut {
				t.Errorf("stdout differs from ansible-playbook:\n%s", lineDiff(aOut, uOut))
			}
			if uErr != aErr {
				t.Errorf("stderr differs from ansible-playbook:\n%s", lineDiff(aErr, uErr))
			}
		})
	}
}
