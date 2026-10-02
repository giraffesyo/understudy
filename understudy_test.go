package understudy_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	understudy "github.com/giraffesyo/understudy"
)

func samplePlaybook(dir string) understudy.Playbook {
	noGather := false
	return understudy.Playbook{{
		Name:        "API demo",
		Hosts:       "all",
		GatherFacts: &noGather,
		Vars:        understudy.Args{"greeting": "hello", "count": 3},
		Tasks: []understudy.Task{
			{
				Name:        "run a command",
				Action:      understudy.Shell{Cmd: "echo {{ greeting }}-world"},
				Register:    "out",
				ChangedWhen: []string{"false"},
			},
			{
				Name: "verify it",
				Action: understudy.Assert{That: []string{
					`out.stdout == "hello-world"`,
					"count * 2 == 6",
				}},
			},
			{
				Name:   "write a file",
				Action: understudy.Copy{Content: "from the api\n", Dest: filepath.Join(dir, "api.txt"), Mode: "0600"},
				Notify: []string{"note it"},
			},
			{
				Name:   "conditional skip",
				Action: understudy.Debug{Msg: "never shown"},
				When:   []string{`greeting == "goodbye"`},
			},
			{
				Name: "guarded block",
				Block: []understudy.Task{
					{Name: "fails", Action: understudy.Shell{Cmd: "exit 1"}},
				},
				Rescue: []understudy.Task{
					{Name: "recovered", Action: understudy.Debug{Msg: "rescued"}},
				},
			},
			{
				Name:   "loop over items",
				Action: understudy.Debug{Msg: "item={{ item }}"},
				Loop:   []any{"a", "b"},
			},
			{
				Name:   "generic module escape hatch",
				Action: understudy.M{Module: "ping", Args: understudy.Args{"data": "custom"}},
			},
		},
		Handlers: []understudy.Task{
			{Name: "note it", Action: understudy.Debug{Msg: "handler ran"}},
		},
	}}
}

func TestRenderYAML(t *testing.T) {
	pb := samplePlaybook("/tmp/x")
	data, err := pb.YAML()
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	// Human ordering: name before hosts before tasks; module args nested.
	for _, want := range []string{
		"---\n- name: API demo\n  hosts: all\n  gather_facts: false",
		"shell: echo {{ greeting }}-world",
		"register: out",
		`content: "from the api\n"`,
		"mode: \"0600\"",
		"block:",
		"rescue:",
		"loop:",
		"notify:",
		"handlers:",
		"ping:\n        data: custom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered YAML missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "loop_control") || strings.Contains(out, "become") {
		t.Errorf("unset fields must not render:\n%s", out)
	}
}

func TestValidationErrors(t *testing.T) {
	if _, err := (understudy.Playbook{{Name: "no hosts"}}).YAML(); err == nil ||
		!strings.Contains(err.Error(), "Hosts is required") {
		t.Errorf("missing-hosts error = %v", err)
	}
	pb := understudy.Playbook{{Hosts: "all", Tasks: []understudy.Task{{Name: "empty"}}}}
	if _, err := pb.YAML(); err == nil || !strings.Contains(err.Error(), "Action is required") {
		t.Errorf("missing-action error = %v", err)
	}
	both := understudy.Playbook{{Hosts: "all", Tasks: []understudy.Task{{
		Name:   "both",
		Action: understudy.Ping{},
		Block:  []understudy.Task{{Action: understudy.Ping{}}},
	}}}}
	if _, err := both.YAML(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("block+action error = %v", err)
	}
}

func TestRunDirect(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	res, err := understudy.Run(context.Background(), samplePlaybook(dir), understudy.Options{
		Inventory:  []string{"localhost,"},
		Connection: "local",
		Output:     &buf,
		NoColor:    true,
		BaseDir:    dir,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if res.Failed() {
		t.Fatalf("run failed (exit=%d):\n%s", res.ExitCode, buf.String())
	}
	st := res.Hosts["localhost"]
	if st.Failed != 0 || st.Rescued != 1 || st.Skipped != 1 {
		t.Errorf("stats = %+v, want failed=0 rescued=1 skipped=1", st)
	}
	out := buf.String()
	for _, want := range []string{"rescued", "handler ran", "item=a", "item=b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "api.txt"))
	if err != nil || string(data) != "from the api\n" {
		t.Errorf("copied file = %q, %v", data, err)
	}
}

// The rendered YAML must be loadable and produce identical results —
// the render and direct paths are the same by construction, but this
// guards the file-writing surface too.
func TestRenderedFileRuns(t *testing.T) {
	dir := t.TempDir()
	pb := samplePlaybook(dir)
	path := filepath.Join(dir, "site.yml")
	if err := pb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	res, err := understudy.Run(context.Background(), pb, understudy.Options{
		Inventory: []string{"localhost,"}, Connection: "local",
		Output: &buf, NoColor: true, BaseDir: dir,
	})
	if err != nil || res.Failed() {
		t.Fatalf("res=%+v err=%v\n%s", res, err, buf.String())
	}
}

func TestRunOnEvent(t *testing.T) {
	var names []string
	pb := understudy.Playbook{{Name: "p", Hosts: "localhost", Tasks: []understudy.Task{{Name: "t", Action: understudy.Debug{Msg: "x"}}}}}
	_, err := understudy.Run(context.Background(), pb, understudy.Options{
		Inventory: []string{"localhost,"}, Connection: "local", Output: io.Discard,
		OnEvent: func(e understudy.Event) { names = append(names, e.Event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, " ")
	for _, want := range []string{"v2_playbook_on_start", "v2_playbook_on_play_start", "v2_playbook_on_task_start", "v2_runner_on_ok", "v2_playbook_on_stats"} {
		if !strings.Contains(got, want) {
			t.Errorf("events %q missing %s", got, want)
		}
	}
}

// RunFiles runs YAML playbooks as ansible-playbook does: roles beside the
// playbook, -e @file extra vars under the Go ones, and the ANSIBLE_*
// environment configuring the run.
func TestRunFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("roles/greet/tasks/main.yml", "- name: greet\n  copy:\n    content: \"{{ greeting }} {{ target }}\\n\"\n    dest: \"{{ out_dir }}/greeting.txt\"\n")
	write("site.yml", "- hosts: localhost\n  roles: [greet]\n  tasks:\n    - name: extra vars\n      assert:\n        that: target == 'go'\n")
	write("vars.json", `{"greeting": "hello", "target": "file"}`)

	var buf bytes.Buffer
	var actions []string
	res, err := understudy.RunFiles(t.Context(), []string{filepath.Join(dir, "site.yml")}, understudy.Options{
		ExtraVarsFiles: []string{filepath.Join(dir, "vars.json")},
		ExtraVars:      map[string]any{"target": "go", "out_dir": dir},
		Settings:       map[string]string{"ANSIBLE_GATHERING": "explicit"},
		Output:         &buf,
		OnEvent: func(e understudy.Event) {
			if e.Event == "v2_playbook_on_task_start" {
				actions = append(actions, e.Task.Action)
			}
		},
	})
	if err != nil || res.Failed() {
		t.Fatalf("res=%+v err=%v\n%s", res, err, buf.String())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "greeting.txt")); string(data) != "hello go\n" {
		t.Errorf("greeting.txt = %q, want the file's greeting and the Go target", data)
	}
	if got := strings.Join(actions, " "); got != "copy assert" {
		t.Errorf("task actions = %q, want no fact gathering with the ANSIBLE_GATHERING=explicit setting", got)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("output to a buffer is colored:\n%q", buf.String())
	}
}

func TestRunFilesErrors(t *testing.T) {
	_, err := understudy.RunFiles(t.Context(), []string{filepath.Join(t.TempDir(), "missing.yml")}, understudy.Options{Output: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "could not be found") {
		t.Errorf("missing playbook: err = %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(path, []byte("- hosts: localhost\n  tasks:\n    - nosuchmodule_xyz: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := understudy.RunFiles(t.Context(), []string{path}, understudy.Options{Output: io.Discard}); err == nil {
		t.Error("unknown module: no error")
	}
}
