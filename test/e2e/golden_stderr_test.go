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

// TestGoldenStderr runs golden/stderr playbooks with deprecation warnings
// on (the other golden tests turn them off) and compares stdout, stderr
// and the exit code byte for byte: the deprecation warnings' wording,
// origins, de-duplication and order. With deprecation_warnings off, both
// tools must print nothing on stderr. A playbook's <name>.env file adds
// environment variables (KEY=VALUE lines: configuration) to both runs.
func TestGoldenStderr(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	corpus, err := filepath.Glob("golden/stderr/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no stderr corpus found: %v", err)
	}
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_SYSTEM_WARNINGS=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	for _, pb := range corpus {
		for _, deprecations := range []string{"True", "False"} {
			t.Run(filepath.Base(pb)+"/deprecation_warnings="+deprecations, func(t *testing.T) {
				abs, _ := filepath.Abs(pb)
				env := env
				if data, err := os.ReadFile(strings.TrimSuffix(pb, ".yml") + ".env"); err == nil {
					for _, line := range strings.Split(string(data), "\n") {
						if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
							env = append(append([]string{}, env...), line)
						}
					}
				}
				run := func(bin string, pre ...string) (string, string, int) {
					work := t.TempDir()
					inv := writeGoldenInventory(t, work, "")
					cmd := exec.Command(bin, append(pre, "-f", "1", "-i", inv, "-c", "local", "-e", "workdir="+work, abs)...)
					cmd.Env = append(append(os.Environ(), env...), "ANSIBLE_DEPRECATION_WARNINGS="+deprecations)
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
				if deprecations == "True" && aErr == "" {
					t.Errorf("ansible-playbook printed no deprecation warnings")
				}
			})
		}
	}
}
