package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// runDebugger runs a playbook on localhost with scripted debugger input,
// returning the exit code, the combined output and the stats.
func runDebugger(t *testing.T, src, input string) (int, string, map[string]*executor.HostStats) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yml")
	os.WriteFile(path, []byte(src), 0o644)
	plays, err := playbook.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cb := &callback.Default{Out: &buf, NoColor: true}
	inv, _ := inventory.Load([]string{"localhost,"}, nil)
	r := executor.NewRunner(inv, cb, executor.Options{BaseDir: dir, Forks: 1, Connection: "local"})
	r.DebugIn = strings.NewReader(input)
	r.DebugOut = &buf
	code, err := r.Run(context.Background(), plays)
	if err != nil {
		t.Fatal(err)
	}
	return code, buf.String(), r.Stats()
}

const debuggerPlay = `
- hosts: all
  gather_facts: false
  strategy: debug
  vars: {word: bad}
  tasks:
    - name: flaky
      command: "test {{ word }} = good"
    - debug: {msg: done}
`

// Beyond ansible-core 2.21 (whose update_task crashes and whose redo
// ignores task_vars edits), task_vars and task.args edits apply on redo.
func TestDebuggerRedoAfterFixes(t *testing.T) {
	code, out, stats := runDebugger(t, debuggerPlay, "task_vars['word'] = 'good'\nu\nr\n")
	if code != 0 || !strings.Contains(out, `"msg": "done"`) || stats["localhost"].Failed != 0 {
		t.Fatalf("task_vars redo: code %d, stats %+v\n%s", code, stats["localhost"], out)
	}
	code, out, _ = runDebugger(t, debuggerPlay, "task.args['_raw_params'] = 'true'\nr\n")
	if code != 0 || !strings.Contains(out, `"msg": "done"`) {
		t.Fatalf("task.args redo: code %d\n%s", code, out)
	}
	code, out, _ = runDebugger(t, debuggerPlay, "del task_vars['nope']\np task.args\nq\n")
	if code != 99 || !strings.Contains(out, "***KeyError:KeyError('nope')") ||
		!strings.Contains(out, "{'_raw_params': 'test {{ word }} = good'}") ||
		strings.Contains(out, "PLAY RECAP") || !strings.HasSuffix(out, "User interrupted execution\n") {
		t.Fatalf("quit: code %d\n%s", code, out)
	}
}

func TestDebuggerPprint(t *testing.T) {
	_, out, _ := runDebugger(t, `
- hosts: all
  gather_facts: false
  strategy: debug
  vars:
    big: {alpha: "aaaaaaaaaaaaaaaaaaaa", beta: ["bbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccc"], gamma: "it's"}
  tasks:
    - fail: {msg: boom}
`, "p task_vars['big']\nc\n")
	want := "{'alpha': 'aaaaaaaaaaaaaaaaaaaa',\n 'beta': ['bbbbbbbbbbbbbbbbbbbb', 'cccccccccccccccccccc'],\n 'gamma': \"it's\"}\n"
	if !strings.Contains(out, want) {
		t.Fatalf("pprint output:\n%s", out)
	}
}
