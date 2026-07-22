// Package e2e runs real playbooks through the full stack over the local
// connection: parse -> template -> execute -> stats.
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
	"github.com/giraffesyo/understudy/internal/playbook"
)

// run executes playbook YAML on localhost and returns exit code + output.
func run(t *testing.T, src string, opts executor.Options) (int, string, map[string]*executor.HostStats) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	plays, err := playbook.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var buf bytes.Buffer
	cb := &callback.Default{Out: &buf, Verbosity: opts.Verbosity, NoColor: true}
	if opts.BaseDir == "" {
		opts.BaseDir = dir
	}
	r := executor.NewRunner([]string{"localhost"}, cb, opts)
	code, err := r.Run(context.Background(), plays)
	if err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	return code, buf.String(), r.Stats()
}

func TestSmokePlaybook(t *testing.T) {
	src, err := os.ReadFile("../../examples/site.yml")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stats := run(t, string(src), executor.Options{BaseDir: "../../examples"})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	st := stats["localhost"]
	if st.OK != 9 || st.Changed != 1 || st.Skipped != 1 || st.Failed != 0 {
		t.Errorf("stats = %+v, want ok=9 changed=1 skipped=1", st)
	}
}

func TestFailurePropagation(t *testing.T) {
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: boom
      command: /bin/false
    - name: never reached
      debug:
        msg: unreachable
`, executor.Options{})
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(out, "fatal: [localhost]: FAILED!") {
		t.Errorf("missing fatal line:\n%s", out)
	}
	if strings.Contains(out, "never reached") &&
		strings.Contains(out, `"msg":"unreachable"`) {
		t.Errorf("task after failure ran:\n%s", out)
	}
	if st := stats["localhost"]; st.Failed != 1 {
		t.Errorf("failed = %d, want 1", st.Failed)
	}
}

func TestIgnoreErrors(t *testing.T) {
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - command: /bin/false
      ignore_errors: true
    - debug:
        msg: still here
`, executor.Options{})
	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "...ignoring") {
		t.Errorf("missing ...ignoring:\n%s", out)
	}
	st := stats["localhost"]
	if st.Ignored != 1 || st.Failed != 0 {
		t.Errorf("stats = %+v, want ignored=1 failed=0", st)
	}
}

func TestRegisterAndConditionals(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: echo hello
      register: greet
      changed_when: false
    - debug:
        msg: "got {{ greet.stdout }}"
      when: greet.rc == 0
    - debug:
        msg: skipped branch
      when: greet.rc != 0
    - assert:
        that:
          - greet.stdout == "hello"
          - greet.stdout_lines | length == 1
          - not greet.changed
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "got hello") {
		t.Errorf("register templating failed:\n%s", out)
	}
}

func TestLoopResults(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  vars:
    nums: [1, 2, 3]
  tasks:
    - shell: "echo {{ item * 2 }}"
      loop: "{{ nums }}"
      register: doubled
      changed_when: false
    - assert:
        that:
          - doubled.results | length == 3
          - doubled.results[2].stdout == "6"
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	for _, want := range []string{"(item=1)", "(item=2)", "(item=3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in output:\n%s", want, out)
		}
	}
}

func TestUntilRetries(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: succeeds on second try
      shell: "test -f `+marker+` && echo done || { touch `+marker+`; echo again; exit 1; }"
      register: r
      until: r.rc == 0
      retries: 3
      delay: 0
      ignore_errors: false
`, executor.Options{})
	if code != 0 {
		t.Fatalf("until/retries failed: exit=%d\n%s", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("marker file missing — first attempt never ran")
	}
}

func TestCheckModeSkipsCommands(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - command: echo should-not-run
`, executor.Options{CheckMode: true})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "skipping: [localhost]") {
		t.Errorf("command should skip in check mode:\n%s", out)
	}
}

func TestFailedWhenOverride(t *testing.T) {
	code, _, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: echo warning-condition
      register: r
      failed_when: "'warning-condition' in r.stdout"
      ignore_errors: true
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if st := stats["localhost"]; st.Ignored != 1 {
		t.Errorf("failed_when did not trip: %+v", st)
	}
}
