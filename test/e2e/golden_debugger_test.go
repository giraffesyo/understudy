//go:build golden

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenDebugger drives the task debugger (golden/debug_strategy.yml)
// with scripted input through both tools and compares stdout byte for
// byte: printing expressions, help, redo, continue, quit and end of
// input, and the recap after a redo.
func TestGoldenDebugger(t *testing.T) {
	ansible := ansiblePlaybookBin(t)
	understudy := understudyBin(t)
	pb, _ := filepath.Abs("golden/debug_strategy.yml")
	env := []string{"NO_COLOR=1", "ANSIBLE_NOCOLOR=1", "ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_DEPRECATION_WARNINGS=False",
		"ANSIBLE_SYSTEM_WARNINGS=False"}
	if py, err := exec.LookPath("python3"); err == nil {
		env = append(env, "ANSIBLE_PYTHON_INTERPRETER="+py)
	}
	// A redo runs task.args edits; task_vars edits take effect through
	// update_task, which loads the task again (losing task.args edits).
	scripts := map[string]string{
		"edit_vars_redo":     "task_vars['word'] = 'good'\nr\nq\n",
		"edit_args_redo":     "task.args['_raw_params'] = 'true'\nr\nc\nc\nc\n",
		"update_task_redo":   "task_vars['word'] = 'good'\nu\np task.args\nr\nc\nc\nc\n",
		"update_drops_edits": "task.args['_raw_params'] = 'true'\nu\nr\nq\n",
		"inspect_continue":   "p result.host\np result.task\np result._result\np task_vars['word']\np task\n\np task.name\nhelp\nhelp p\nhelp nope\nnosuchname\nc\nc\nc\n",
		"redo_then_quit":     "r\nq\n",
		"redo_continue":      "r\nc\nc\nc\n",
		"continue_then_eof":  "c\n",
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			run := func(bin string, pre ...string) (string, int) {
				work := t.TempDir()
				inv := writeGoldenInventory(t, work, "")
				args := append(pre, "-f", "1", "-i", inv, "-c", "local", "-e", "workdir="+work, pb)
				cmd := exec.Command(bin, args...)
				cmd.Env = append(os.Environ(), env...)
				cmd.Stdin = strings.NewReader(script)
				out, _ := cmd.Output()
				return normalizeOutput(string(out), work), cmd.ProcessState.ExitCode()
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
