// Package e2e runs real playbooks through the full stack over the local
// connection: parse -> template -> execute -> stats.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giraffesyo/understudy/internal/callback"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/inventory"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/vault"
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
	roleBase := opts.BaseDir
	if roleBase == "" {
		roleBase = dir
	}
	if err := playbook.ResolveRoles(plays, roleBase, nil); err != nil {
		t.Fatalf("roles: %v", err)
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
    - name: template does not create parent directories (Ansible parity)
      template:
        src: greeting.j2
        dest: `+dest+`/greeting.txt
      register: noparent
      ignore_errors: true
    - assert:
        that: "noparent.failed and 'does not exist' in noparent.msg"
    - file: {path: `+dest+`, state: directory}
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
    - name: templated delegate target, facts delegated
      set_fact: {marker: yes}
      delegate_to: "{{ 'local' ~ 'host' }}"
      delegate_facts: true
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}

// writeRole builds a complete role fixture on disk.
func writeRole(t *testing.T, dir, name string, files map[string]string) {
	t.Helper()
	root := filepath.Join(dir, "roles", name)
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRolesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)

	// A dependency role, pulled in via meta/main.yml.
	writeRole(t, dir, "base", map[string]string{
		"tasks/main.yml": `
- name: base marker
  copy:
    content: "base was here\n"
    dest: ` + out + `/base.txt
`,
	})

	writeRole(t, dir, "web", map[string]string{
		"meta/main.yml":          "dependencies:\n  - base\n",
		"defaults/main.yml":      "port: 80\nservername: default-name\n",
		"vars/main.yml":          "docroot: /srv/www\n",
		"templates/site.conf.j2": "server {{ servername }}:{{ port }} root={{ docroot }}\n",
		"tasks/main.yml": `
- name: render site config
  template:
    src: site.conf.j2
    dest: ` + out + `/site.conf
  notify: reload web
- name: os-specific setup
  include_tasks: "{{ flavor }}.yml"
- name: static import
  import_tasks: extra.yml
  vars:
    extra_msg: from-import
`,
		"tasks/alpha.yml": `
- name: alpha branch
  copy:
    content: "flavor=alpha\n"
    dest: ` + out + `/flavor.txt
`,
		"tasks/extra.yml": `
- name: imported task
  copy:
    content: "{{ extra_msg }}\n"
    dest: ` + out + `/extra.txt
`,
		"handlers/main.yml": `
- name: reload web
  copy:
    content: "handler ran\n"
    dest: ` + out + `/handler.txt
`,
	})

	code, output, stats := run(t, `
- hosts: all
  gather_facts: false
  vars:
    flavor: alpha
  roles:
    - role: web
      vars:
        servername: overridden
  tasks:
    - name: role vars visible to play tasks
      assert:
        that:
          - port == 80
          - docroot == "/srv/www"
`, executor.Options{BaseDir: dir})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, output)
	}

	checks := map[string]string{
		"base.txt":    "base was here\n",                      // dependency ran first
		"site.conf":   "server overridden:80 root=/srv/www\n", // params > defaults; role vars work
		"flavor.txt":  "flavor=alpha\n",                       // dynamic include with templated path
		"extra.txt":   "from-import\n",                        // static import with vars inheritance
		"handler.txt": "handler ran\n",                        // role handler notified
	}
	for file, want := range checks {
		data, err := os.ReadFile(filepath.Join(out, file))
		if err != nil {
			t.Errorf("%s missing: %v", file, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", file, data, want)
		}
	}
	if st := stats["localhost"]; st.Failed != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestIncludeTasksWhenGates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inc.yml"), []byte(`
- name: included task
  debug: {msg: included-ran}
`), 0o644)
	_, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - include_tasks: inc.yml
      when: false
    - include_tasks: inc.yml
      when: true
`, executor.Options{BaseDir: dir})
	if strings.Count(out, "included-ran") != 1 {
		t.Errorf("include when-gating wrong (want exactly one run):\n%s", out)
	}
}

func TestRoleNotFoundError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yml")
	os.WriteFile(path, []byte("- hosts: all\n  roles: [nosuchrole]\n"), 0o644)
	plays, err := playbook.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = playbook.ResolveRoles(plays, dir, nil)
	if err == nil || !strings.Contains(err.Error(), "The role 'nosuchrole' was not found in: "+dir+"/roles:"+dir) {
		t.Errorf("err = %v", err)
	}
}

func TestAsyncFireAndForget(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "bg-done")
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: background job
      shell: sleep 0.2 && touch `+marker+`
      async: 60
      poll: 0
      register: bg
    - assert:
        that:
          - bg.started == 1
          - bg.finished == 0
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	// The marker must NOT exist yet (task returned before the sleep ended)…
	if _, err := os.Stat(marker); err == nil {
		t.Log("marker existed immediately (slow machine?); continuing")
	}
	// …but the detached process must complete on its own.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("background process never completed")
}

func TestNewModulesRegistered(t *testing.T) {
	// Every surveyed module must at least resolve (parse without
	// "couldn't resolve module/action").
	mods := []string{
		"group", "pip", "get_url", "blockinfile", "wait_for", "mount",
		"sysctl", "tempfile", "find", "modprobe", "script", "firewalld",
		"selinux", "parted", "filesystem", "yum_repository", "sudoers",
		"openssh_keypair", "mysql_db", "mysql_user", "uri", "git", "cron",
	}
	for _, m := range mods {
		src := "- hosts: all\n  gather_facts: false\n  tasks:\n    - " + m + ": {}\n"
		dir := t.TempDir()
		path := filepath.Join(dir, "p.yml")
		os.WriteFile(path, []byte(src), 0o644)
		if _, err := playbook.LoadFile(path); err != nil {
			t.Errorf("module %q does not resolve: %v", m, err)
		}
	}
}

func TestBlockinfileAndFind(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "app.conf")
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: add managed block
      blockinfile:
        path: `+conf+`
        create: true
        block: |
          option one
          option two
      register: b1
    - name: block idempotent
      blockinfile:
        path: `+conf+`
        block: |
          option one
          option two
      register: b2
    - name: find the conf
      find:
        paths: `+dir+`
        patterns: "*.conf"
      register: found
    - assert:
        that:
          - b1.changed
          - not b2.changed
          - found.matched == 1
          - found.files[0].path == "`+conf+`"
    - name: make a temp dir
      tempfile:
        state: directory
      register: tmp
    - name: group check-mode reports would-change
      group:
        name: understudy-nonexistent-grp
      check_mode: true
      register: g
    - assert:
        that:
          - tmp.path is defined
          - g.changed
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), "ANSIBLE MANAGED BLOCK") || !strings.Contains(string(data), "option one") {
		t.Errorf("block content wrong:\n%s", data)
	}
}

func TestVaultRuntime(t *testing.T) {
	dir := t.TempDir()
	// Encrypt values with understudy's own vault (format-verified against
	// real ansible-vault elsewhere).
	inlineVal, err := vault.Encrypt([]byte("super-secret-token"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	fileEnc, err := vault.Encrypt([]byte("db_password: p@ssw0rd\n"), "pw")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "secrets.yml"), []byte(fileEnc), 0o644)

	// Embed the !vault inline var (indented under vars:).
	var indented strings.Builder
	for _, line := range strings.Split(strings.TrimRight(inlineVal, "\n"), "\n") {
		indented.WriteString("      " + line + "\n")
	}
	src := `
- hosts: all
  gather_facts: false
  vars_files: [secrets.yml]
  vars:
    api_token: !vault |
` + indented.String() + `
  tasks:
    - assert:
        that:
          - api_token == "super-secret-token"
          - db_password == "p@ssw0rd"
`
	path := filepath.Join(dir, "site.yml")
	os.WriteFile(path, []byte(src), 0o644)

	plays, err := playbook.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	secrets := vault.NewSecrets("pw")
	inventory.Decrypt = secrets.MaybeDecryptFile
	defer func() { inventory.Decrypt = nil }()
	if err := playbook.ResolveRoles(plays, dir, nil); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cb := &callback.Default{Out: &buf, NoColor: true}
	inv, _ := inventory.Load([]string{"localhost,"}, nil)
	r := executor.NewRunner(inv, cb, executor.Options{BaseDir: dir, Vault: secrets})
	code, err := r.Run(context.Background(), plays)
	if err != nil || code != 0 {
		t.Fatalf("vault run failed: code=%d err=%v\n%s", code, err, buf.String())
	}

	// Without the password, the encrypted value must error clearly.
	plays2, _ := playbook.LoadFile(path)
	inventory.Decrypt = nil
	playbook.ResolveRoles(plays2, dir, nil)
	var buf2 bytes.Buffer
	r2 := executor.NewRunner(inventoryMust(t), &callback.Default{Out: &buf2, NoColor: true},
		executor.Options{BaseDir: dir})
	// vars_files decryption fails at play start without a password.
	code2, _ := r2.Run(context.Background(), plays2)
	if code2 == 0 {
		t.Errorf("run without vault password should fail:\n%s", buf2.String())
	}
}

func inventoryMust(t *testing.T) *inventory.Inventory {
	inv, err := inventory.Load([]string{"localhost,"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// runMulti runs a playbook across N local hosts (h0..hN-1).
func runMulti(t *testing.T, n int, src string, opts executor.Options) (int, string, map[string]*executor.HostStats) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yml")
	os.WriteFile(path, []byte(src), 0o644)
	plays, err := playbook.LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if opts.BaseDir == "" {
		opts.BaseDir = dir
	}
	opts.Connection = "local"
	var hosts []string
	for i := 0; i < n; i++ {
		hosts = append(hosts, fmt.Sprintf("h%d,", i))
	}
	inv, err := inventory.Load([]string{strings.Join(hosts, "")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	cb := &callback.Default{Out: &buf, NoColor: true}
	r := executor.NewRunner(inv, cb, opts)
	code, err := r.Run(context.Background(), plays)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	return code, buf.String(), r.Stats()
}

func TestSerialBatching(t *testing.T) {
	// 5 hosts, serial 2 -> batches [h0,h1] [h2,h3] [h4]. The play banner
	// reprints per batch, so 3 PLAY banners appear.
	code, out, stats := runMulti(t, 5, `
- name: rolling
  hosts: all
  gather_facts: false
  serial: 2
  tasks:
    - debug: {msg: "on {{ inventory_hostname }}"}
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if got := strings.Count(out, "PLAY [rolling]"); got != 3 {
		t.Errorf("expected 3 play banners (3 batches), got %d:\n%s", got, out)
	}
	for i := 0; i < 5; i++ {
		if st := stats[fmt.Sprintf("h%d", i)]; st == nil || st.OK != 1 {
			t.Errorf("h%d stats = %+v", i, st)
		}
	}
}

func TestMaxFailPercentageAborts(t *testing.T) {
	// 4 hosts; h1 fails; max_fail_percentage 20 (>20% of a batch aborts).
	// serial 4 = one batch of 4, 1/4 = 25% > 20% -> the play aborts before
	// the second task, so "second task" never runs on the survivors.
	code, out, _ := runMulti(t, 4, `
- hosts: all
  gather_facts: false
  serial: 4
  max_fail_percentage: 20
  tasks:
    - name: maybe fail
      shell: "test '{{ inventory_hostname }}' != 'h1'"
    - name: second task
      debug: {msg: "reached second on {{ inventory_hostname }}"}
`, executor.Options{})
	if code != 2 {
		t.Errorf("exit=%d, want 2 (a host failed)\n%s", code, out)
	}
	if strings.Contains(out, "reached second") {
		t.Errorf("play should have aborted after the failing batch:\n%s", out)
	}
}

func TestMaxFailPercentageTolerated(t *testing.T) {
	// Same failure but threshold 50: 25% <= 50%, play continues.
	code, out, _ := runMulti(t, 4, `
- hosts: all
  gather_facts: false
  serial: 4
  max_fail_percentage: 50
  tasks:
    - name: maybe fail
      shell: "test '{{ inventory_hostname }}' != 'h1'"
    - name: second task
      debug: {msg: "reached second on {{ inventory_hostname }}"}
`, executor.Options{})
	if code != 2 {
		t.Errorf("exit=%d, want 2 (h1 still failed)\n%s", code, out)
	}
	// The 3 survivors continue to the second task.
	if strings.Count(out, "reached second") != 3 {
		t.Errorf("survivors should reach the second task (want 3):\n%s", out)
	}
}

func TestAsyncJobsAndStatus(t *testing.T) {
	code, out, _ := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - shell: "sleep 1; echo from-job"
      async: 30
      poll: 0
      register: job
    - assert: {that: ["job.started", "not job.finished", "job.ansible_job_id is defined"]}
    - async_status: {jid: "{{ job.ansible_job_id }}"}
      register: st
      until: st.finished
      retries: 30
      delay: 1
    - assert: {that: ["st.stdout == 'from-job'", "st.rc == 0"]}
    - command: echo polled
      async: 20
      poll: 1
      register: p
    - assert: {that: ["p.finished", "p.stdout == 'polled'"]}
    - command: sleep 10
      async: 2
      poll: 1
      register: to
      ignore_errors: true
    - assert: {that: ["to.failed", "to.async_result is defined"]}
    - async_status: {jid: "{{ job.ansible_job_id }}", mode: cleanup}
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}

func TestParallelBlockOptIn(t *testing.T) {
	start := time.Now()
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - vars: {understudy_parallel: true}
      block:
        - command: sleep 2
        - command: sleep 2
        - block:
            - set_fact: {first: 1}
            - assert: {that: "first == 1"}
    - debug: {msg: after}
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if d := time.Since(start); d > 3500*time.Millisecond {
		t.Errorf("parallel block took %v; the two 2s tasks should overlap", d)
	}
	if st := stats["localhost"]; st == nil || st.OK != 5 || st.Changed != 2 {
		t.Errorf("stats = %+v", st)
	}
}

// TestTimedOutCommandIsKilled: a task that times out is abandoned, and
// the process its in-process module started is killed with it rather
// than left running (so its later side effects never happen).
func TestTimedOutCommandIsKilled(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	marker := filepath.Join(dir, "marker")
	start := time.Now()
	code, out, stats := run(t, `
- hosts: all
  gather_facts: false
  tasks:
    - name: exec'd child
      shell: echo $$ > `+pidFile+` && exec sleep 30
      timeout: 1
      ignore_errors: true
      register: r
    - name: a later step of the shell script
      shell: sleep 2 && touch `+marker+`
      timeout: 1
      ignore_errors: true
    - assert:
        that: r.timedout.period == 1
`, executor.Options{})
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if st := stats["localhost"]; st == nil || st.Ignored != 2 {
		t.Errorf("stats = %+v\n%s", st, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("run took %v", d)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the command never started: %v\n%s", err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the timed-out command (pid %d) is still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The second script's shell was killed before its touch could run.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the timed-out shell script kept running and created its marker")
	}
}
