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
	"github.com/giraffesyo/understudy/internal/inventory"
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
	inv, err := inventory.Load([]string{"localhost,"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := executor.NewRunner(inv, cb, opts)
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
      shell: exit 1
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
    - shell: exit 1
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

func TestHandlersNotifyAndFlush(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "handler-ran")
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: change something
      command: touch `+filepath.Join(dir, "x")+`
      notify: record handler
    - name: change again (handler must still run once)
      command: touch `+filepath.Join(dir, "y")+`
      notify: record handler
  handlers:
    - name: record handler
      command: touch `+marker+`
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "RUNNING HANDLER [record handler]") {
		t.Errorf("missing handler banner:\n%s", out)
	}
	if strings.Count(out, "RUNNING HANDLER") != 1 {
		t.Errorf("handler ran more than once:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("handler did not run")
	}
}

func TestHandlerNotRunWithoutChange(t *testing.T) {
	_, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - command: echo hi
      changed_when: false
      notify: never runs
  handlers:
    - name: never runs
      debug:
        msg: should not appear
`, executor.Options{})
	if strings.Contains(out, "RUNNING HANDLER") {
		t.Errorf("handler ran without a change:\n%s", out)
	}
}

func TestTagsFiltering(t *testing.T) {
	src := `
- hosts: all
  gather_facts: false
  tasks:
    - debug: {msg: tagged-a}
      tags: [a]
    - debug: {msg: tagged-b}
      tags: [b]
    - debug: {msg: always-on}
      tags: [always]
    - debug: {msg: never-on}
      tags: [never]
`
	_, out, _ := run(t, src, executor.Options{Tags: []string{"a"}})
	for _, want := range []string{"tagged-a", "always-on"} {
		if !strings.Contains(out, want) {
			t.Errorf("--tags a: missing %s:\n%s", want, out)
		}
	}
	for _, not := range []string{"tagged-b", "never-on"} {
		if strings.Contains(out, not) {
			t.Errorf("--tags a: should not run %s:\n%s", not, out)
		}
	}
	_, out2, _ := run(t, src, executor.Options{SkipTags: []string{"a"}})
	if strings.Contains(out2, "tagged-a") || !strings.Contains(out2, "tagged-b") {
		t.Errorf("--skip-tags a wrong:\n%s", out2)
	}
}

func TestCopyTemplateFileStat(t *testing.T) {
	dir := t.TempDir()
	tpl := filepath.Join(dir, "templates")
	os.MkdirAll(tpl, 0o755)
	os.WriteFile(filepath.Join(tpl, "greeting.j2"),
		[]byte("Hello {{ target_name }} from {{ inventory_hostname }}\n"), 0o644)
	dest := filepath.Join(dir, "out")

	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  vars:
    target_name: world
  tasks:
    - name: render template
      template:
        src: greeting.j2
        dest: `+dest+`/greeting.txt
        mode: "0600"
      register: t1
    - name: render again (idempotent)
      template:
        src: greeting.j2
        dest: `+dest+`/greeting.txt
      register: t2
    - assert:
        that:
          - t1.changed
          - not t2.changed
    - name: copy inline content
      copy:
        content: "static\n"
        dest: `+dest+`/static.txt
    - name: stat the rendered file
      stat:
        path: `+dest+`/greeting.txt
      register: st
    - assert:
        that:
          - st.stat.exists
          - st.stat.mode == "0600"
          - st.stat.isreg
    - name: manage a directory
      file:
        path: `+dest+`/subdir
        state: directory
        mode: "0755"
    - name: symlink
      file:
        src: `+dest+`/greeting.txt
        dest: `+dest+`/link.txt
        state: link
    - name: remove it
      file:
        path: `+dest+`/static.txt
        state: absent
      register: rm
    - assert:
        that: rm.changed
`, executor.Options{BaseDir: dir})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	data, err := os.ReadFile(filepath.Join(dest, "greeting.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "Hello world from localhost\n" {
		t.Errorf("rendered content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dest, "static.txt")); !os.IsNotExist(err) {
		t.Error("static.txt should have been removed")
	}
	if target, err := os.Readlink(filepath.Join(dest, "link.txt")); err != nil || target != filepath.Join(dest, "greeting.txt") {
		t.Errorf("symlink target = %q, %v", target, err)
	}
}

func TestBlockRescueAlways(t *testing.T) {
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - block:
        - name: doomed
          shell: exit 1
        - name: unreachable in block
          debug: {msg: never-block}
      rescue:
        - name: recovery
          debug:
            msg: "rescued after {{ ansible_failed_task.name }} (rc={{ ansible_failed_result.rc }})"
      always:
        - name: cleanup
          debug: {msg: always-runs}
    - name: after block
      debug: {msg: play-continues}
`, executor.Options{})
	if code != 0 {
		t.Fatalf("rescue should keep the play green: exit=%d\n%s", code, out)
	}
	for _, want := range []string{"rescued after doomed (rc=1)", "always-runs", "play-continues", "FAILED!"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "never-block") {
		t.Errorf("block continued after failure:\n%s", out)
	}
	st := stats["localhost"]
	if st.Failed != 0 || st.Rescued != 1 {
		t.Errorf("stats = %+v, want failed=0 rescued=1", st)
	}
}

func TestBlockWithoutRescueFails(t *testing.T) {
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - block:
        - shell: exit 1
      always:
        - debug: {msg: always-still-runs}
    - debug: {msg: not-reached}
`, executor.Options{})
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if !strings.Contains(out, "always-still-runs") {
		t.Errorf("always must run even on unrescued failure:\n%s", out)
	}
	if strings.Contains(out, "not-reached") {
		t.Errorf("play continued after unrescued failure:\n%s", out)
	}
	if st := stats["localhost"]; st.Failed != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestBlockInheritance(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  vars: {run_it: false}
  tasks:
    - block:
        - name: inherited when skips me
          debug: {msg: should-skip}
        - name: also skipped
          debug: {msg: also-skip}
      when: run_it
    - block:
        - name: sees block var
          debug: {msg: "{{ blockvar }}"}
      vars: {blockvar: from-block}
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if strings.Contains(out, "should-skip") && !strings.Contains(out, "skipping") {
		t.Errorf("block when not inherited:\n%s", out)
	}
	if !strings.Contains(out, "from-block") {
		t.Errorf("block vars not inherited:\n%s", out)
	}
}

func TestRescueFailurePropagates(t *testing.T) {
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - block:
        - shell: exit 1
      rescue:
        - name: rescue that also fails
          shell: exit 1
    - debug: {msg: not-reached}
`, executor.Options{})
	if code != 2 {
		t.Errorf("failing rescue must fail the host: exit=%d\n%s", code, out)
	}
	if strings.Contains(out, "not-reached") {
		t.Errorf("play continued after failing rescue:\n%s", out)
	}
	if st := stats["localhost"]; st.Failed != 1 || st.Rescued != 1 {
		t.Errorf("stats = %+v, want failed=1 rescued=1", st)
	}
}

func TestDelegateToLocalhost(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: delegated command
      command: echo on-control-node
      delegate_to: localhost
      register: d
    - assert:
        that:
          - d.stdout == "on-control-node"
          - inventory_hostname == "localhost"
    - name: unsupported delegate target errors clearly
      command: echo nope
      delegate_to: otherhost
      ignore_errors: true
      register: bad
    - assert:
        that: "'not supported yet' in bad.msg"
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}
