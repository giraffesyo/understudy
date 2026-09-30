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

// TestGoldenLoadErrors runs playbooks that fail to load (golden/errors)
// through both tools: each must fail before running anything, with the
// same exit code and byte-identical stdout and stderr (the "[ERROR]: ..."
// message, its Origin and the source excerpt).
func TestGoldenLoadErrors(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	corpus, err := filepath.Glob("golden/errors/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no load-error corpus found: %v", err)
	}
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_DEPRECATION_WARNINGS=False",
		"ANSIBLE_SYSTEM_WARNINGS=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			abs, _ := filepath.Abs(pb)
			run := func(bin string, pre ...string) (string, string, int) {
				work := t.TempDir()
				inv := writeGoldenInventory(t, work, "")
				cmd := exec.Command(bin, append(pre, "-i", inv, "-c", "local", abs)...)
				cmd.Env = append(os.Environ(), env...)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				cmd.Run()
				return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
			}
			aOut, aErr, aRC := run(ansible)
			uOut, uErr, uRC := run(understudy, "playbook")
			if aRC == 0 || !strings.HasPrefix(aErr, "[ERROR]: ") {
				t.Fatalf("ansible-playbook loaded the playbook (exit %d):\n%s%s", aRC, aOut, aErr)
			}
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
