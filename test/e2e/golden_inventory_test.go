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

// TestGoldenInventory loads inventory sources through both tools and
// compares stdout, stderr and the exit code byte for byte: which plugin
// parses each source (host_list, script, auto, yaml, ini), the warnings
// for sources none can parse (each plugin's failure, "Unable to parse",
// "No inventory was parsed"), the implicit localhost left when nothing
// parsed, and the resulting groups and variables.
//
// Every entry of golden/inventory/sources (a file or a directory) runs
// as the only -i source of golden/inventory/playbook.yml. Each
// golden/inventory/cases/<name>.args holds a full argument list, one per
// line, with @INV@ standing for golden/inventory; <name>.env adds
// environment variables (KEY=VALUE lines). A case run with -v/-vvv also
// compares which plugins each source was offered to and why they declined
// it (the version banner masked, as in TestGoldenOutputVV).
func TestGoldenInventory(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	dir, _ := filepath.Abs("golden/inventory")
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_DEPRECATION_WARNINGS=False",
		"ANSIBLE_SYSTEM_WARNINGS=False", "ANSIBLE_RETRY_FILES_ENABLED=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}

	type invCase struct {
		name string
		args []string
		env  []string
	}
	var cases []invCase
	sources, _ := filepath.Glob(filepath.Join(dir, "sources", "*"))
	for _, src := range sources {
		cases = append(cases, invCase{name: "sources/" + filepath.Base(src),
			args: []string{"-i", src, filepath.Join(dir, "playbook.yml")}})
	}
	argFiles, _ := filepath.Glob(filepath.Join(dir, "cases", "*.args"))
	for _, f := range argFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c := invCase{name: strings.TrimSuffix(filepath.Base(f), ".args")}
		for _, arg := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			c.args = append(c.args, strings.ReplaceAll(arg, "@INV@", dir))
		}
		if data, err := os.ReadFile(strings.TrimSuffix(f, ".args") + ".env"); err == nil {
			c.env = strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		t.Fatal("no inventory corpus found")
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Relative sources resolve against a fresh directory (no
			// ansible.cfg), the same for both tools.
			work := t.TempDir()
			run := func(bin string, pre ...string) (string, string, int) {
				cmd := exec.Command(bin, append(append(pre, "-f", "1", "-c", "local"), c.args...)...)
				cmd.Dir = work
				cmd.Env = append(append(os.Environ(), env...), c.env...)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				cmd.Run()
				return versionBannerRe.ReplaceAllString(stdout.String(), "VERSION\n"), stderr.String(), cmd.ProcessState.ExitCode()
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
