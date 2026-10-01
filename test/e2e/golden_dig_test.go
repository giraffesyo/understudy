//go:build golden

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fixture's port in dnspython's nameserver text, and a resolution
// lifetime's measured length.
var (
	digPortRe     = regexp.MustCompile(`(Do53:127\.0\.0\.1)@[0-9]+`)
	digLifetimeRe = regexp.MustCompile(`expired after [0-9]+\.[0-9]{3} seconds`)
)

// TestGoldenDig runs the real community.general.dig lookup (dnspython)
// and understudy's against the DNS fixture (dnsfixture_test.go), each run
// against a fresh fixture, and compares stdout byte for byte at default
// verbosity and -v.
func TestGoldenDig(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	pb, _ := filepath.Abs("golden/dig/dig.yml")
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_DEPRECATION_WARNINGS=False",
		"ANSIBLE_SYSTEM_WARNINGS=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	for _, flags := range [][]string{nil, {"-v"}} {
		t.Run("verbosity"+strings.Join(flags, ""), func(t *testing.T) {
			run := func(bin string, pre ...string) (string, int) {
				dns := startDNSFixture(t)
				defer dns.close()
				work := t.TempDir()
				inv := writeGoldenInventory(t, work, "")
				args := append(append(pre, flags...), "-f", "1", "-i", inv, "-c", "local",
					"-e", "workdir="+work, "-e", "dns_port="+itoa(dns.port), pb)
				cmd := exec.Command(bin, args...)
				cmd.Env = append(os.Environ(), env...)
				out, _ := cmd.Output()
				s := normalizeOutput(string(out), work)
				s = digPortRe.ReplaceAllString(s, "$1@PORT")
				s = digLifetimeRe.ReplaceAllString(s, "expired after N seconds")
				if len(flags) > 0 {
					s = normalizeVerbose(s)
				}
				return s, cmd.ProcessState.ExitCode()
			}
			want, wantRC := run(ansible)
			got, gotRC := run(understudy, "playbook")
			if got != want {
				t.Errorf("stdout differs from ansible-playbook:\n%s", lineDiff(want, got))
			}
			if gotRC != wantRC {
				t.Errorf("exit code %d, ansible-playbook %d", gotRC, wantRC)
			}
		})
	}
}
