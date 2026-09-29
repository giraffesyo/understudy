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

// TestGoldenOutput holds understudy to byte-for-byte parity with
// ansible-playbook's default stdout callback over the whole golden corpus
// (forks=1 so multi-host result order is deterministic). Only values that
// can never match are normalized: per-run work directories and command
// timings.
func TestGoldenOutput(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)

	corpus, err := filepath.Glob("golden/*.yml")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("no golden corpus found: %v", err)
	}
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_LOCALHOST_WARNING=False", "ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False", "ANSIBLE_SYSTEM_WARNINGS=False",
	}
	// An explicit interpreter skips discovery, whose discovered_interpreter
	// facts are a Python-only artifact.
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}

	for _, pb := range corpus {
		t.Run(filepath.Base(pb), func(t *testing.T) {
			abs, _ := filepath.Abs(pb)
			base := filepath.Base(pb)
			var extra []string
			if strings.HasPrefix(base, "check_") {
				extra = []string{"--check"}
			}
			if strings.HasPrefix(base, "tags_") {
				extra = []string{"--tags", "run_me,also"}
			}
			invSrc := strings.TrimSuffix(pb, ".yml") + ".inventory"

			run := func(bin string, pre ...string) string {
				work := t.TempDir()
				inv := writeGoldenInventory(t, work, invSrc)
				args := append(append(pre, "-f", "1", "-i", inv, "-c", "local", "-e", "workdir="+work), extra...)
				cmd := exec.Command(bin, append(args, abs)...)
				cmd.Env = append(os.Environ(), env...)
				cmd.Stdin = nil
				out, _ := cmd.Output() // stdout only; stderr carries warnings
				return normalizeOutput(string(out), work)
			}
			want := run(ansible)
			got := run(understudy, "playbook")
			if got != want {
				t.Errorf("stdout differs from ansible-playbook:\n%s", lineDiff(want, got))
			}
		})
	}
}

var timingRe = regexp.MustCompile(`"(delta|start|end)": "[^"]*"`)

func normalizeOutput(s, work string) string {
	s = strings.ReplaceAll(s, work, "WORK")
	return timingRe.ReplaceAllString(s, `"$1": "T"`)
}

// lineDiff shows the first differing lines with a little context.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		b.WriteString("line " + itoa(i+1) + ":\n  ansible:    " + wl + "\n  understudy: " + gl + "\n")
		if shown++; shown == 8 {
			b.WriteString("  ...\n")
			break
		}
	}
	return b.String()
}
