package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecutableCallbackPlugins: a callback plugin is any executable in a
// callback_plugins dir; it gets JSON events on stdin, as an aggregate
// callback (callbacks_enabled) or as the stdout callback.
func TestExecutableCallbackPlugins(t *testing.T) {
	bin, _ := filepath.Abs("../../bin/understudy")
	if _, err := os.Stat(bin); err != nil {
		missingPrereq(t, "bin/understudy not built")
	}
	dir := t.TempDir()
	plugins := filepath.Join(dir, "callback_plugins")
	os.MkdirAll(plugins, 0o755)
	events := filepath.Join(dir, "events.jsonl")
	os.WriteFile(filepath.Join(plugins, "recorder"), []byte("#!/bin/sh\ncat > "+events+"\n"), 0o755)
	os.WriteFile(filepath.Join(plugins, "names"), []byte("#!/bin/sh\nsed -n 's/.*\"event\":\"\\([a-z0-9_]*\\)\".*/EVENT \\1/p'\n"), 0o755)
	os.WriteFile(filepath.Join(plugins, "legacy.py"), []byte("# a Python-only plugin\n"), 0o644)
	pb := filepath.Join(dir, "play.yml")
	os.WriteFile(pb, []byte(`- hosts: localhost
  gather_facts: false
  tasks:
    - name: say hi
      debug: {msg: hi}
    - command: "false"
      ignore_errors: true
`), 0o644)

	runWith := func(env ...string) (string, string) {
		cmd := exec.Command(bin, "playbook", "-i", "localhost,", "-c", "local", pb)
		cmd.Env = append(os.Environ(), append(env, "NO_COLOR=1")...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, _ := cmd.Output()
		return string(out), stderr.String()
	}

	out, stderr := runWith("ANSIBLE_CALLBACKS_ENABLED=recorder,legacy")
	if !strings.Contains(out, "TASK [say hi]") {
		t.Fatalf("stdout callback output missing:\n%s", out)
	}
	if !strings.Contains(stderr, "Skipping callback plugin 'legacy', unable to load") {
		t.Errorf("python-only plugin not reported: %s", stderr)
	}
	data, _ := os.ReadFile(events)
	for _, want := range []string{`"event":"v2_playbook_on_start"`, `"event":"v2_playbook_on_task_start"`,
		`"name":"say hi"`, `"event":"v2_runner_on_ok"`, `"event":"v2_runner_on_failed"`, `"ignore_errors":true`,
		`"event":"v2_playbook_on_stats"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("events missing %s:\n%s", want, data)
		}
	}

	out, _ = runWith("ANSIBLE_STDOUT_CALLBACK=names")
	if !strings.Contains(out, "EVENT v2_runner_on_ok") || strings.Contains(out, "TASK [") {
		t.Errorf("executable stdout callback did not replace output:\n%s", out)
	}
}
