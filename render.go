package understudy

import (
	"fmt"
	"os"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// YAML renders the playbook as standard Ansible playbook YAML, suitable
// for ansible-playbook or understudy's own loader.
func (pb Playbook) YAML() ([]byte, error) {
	docs := make([]any, 0, len(pb))
	for i := range pb {
		play, err := renderPlay(&pb[i])
		if err != nil {
			return nil, err
		}
		docs = append(docs, play)
	}
	out, err := yaml.Marshal(docs, 2)
	if err != nil {
		return nil, err
	}
	return append([]byte("---\n"), out...), nil
}

// WriteFile renders the playbook to a YAML file.
func (pb Playbook) WriteFile(path string) error {
	data, err := pb.YAML()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func renderPlay(p *Play) (yaml.OrderedMap, error) {
	if p.Hosts == "" {
		return nil, fmt.Errorf("play %q: Hosts is required", p.Name)
	}
	out := yaml.OrderedMap{}
	add := func(k string, v any) { out = append(out, yaml.KV{K: k, V: v}) }

	if p.Name != "" {
		add("name", p.Name)
	}
	add("hosts", p.Hosts)
	if p.GatherFacts != nil {
		add("gather_facts", *p.GatherFacts)
	}
	if p.Become {
		add("become", true)
	}
	if p.BecomeUser != "" {
		add("become_user", p.BecomeUser)
	}
	if p.BecomeMethod != "" {
		add("become_method", p.BecomeMethod)
	}
	if len(p.Vars) > 0 {
		add("vars", p.Vars)
	}
	if len(p.VarsFiles) > 0 {
		add("vars_files", strsToList(p.VarsFiles))
	}
	if len(p.Tags) > 0 {
		add("tags", strsToList(p.Tags))
	}
	if len(p.Environment) > 0 {
		add("environment", p.Environment)
	}
	for _, sec := range []struct {
		key   string
		tasks []Task
	}{
		{"pre_tasks", p.PreTasks},
		{"tasks", p.Tasks},
		{"post_tasks", p.PostTasks},
		{"handlers", p.Handlers},
	} {
		if len(sec.tasks) == 0 {
			continue
		}
		rendered, err := renderTasks(sec.tasks)
		if err != nil {
			return nil, fmt.Errorf("play %q %s: %w", p.Name, sec.key, err)
		}
		add(sec.key, rendered)
	}
	return out, nil
}

func renderTasks(tasks []Task) ([]any, error) {
	out := make([]any, 0, len(tasks))
	for i := range tasks {
		m, err := renderTask(&tasks[i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func renderTask(t *Task) (yaml.OrderedMap, error) {
	if err := validateAction(t); err != nil {
		return nil, err
	}
	out := yaml.OrderedMap{}
	add := func(k string, v any) { out = append(out, yaml.KV{K: k, V: v}) }

	if t.Name != "" {
		add("name", t.Name)
	}

	if len(t.Block) > 0 {
		blk, err := renderTasks(t.Block)
		if err != nil {
			return nil, err
		}
		add("block", blk)
		if len(t.Rescue) > 0 {
			resc, err := renderTasks(t.Rescue)
			if err != nil {
				return nil, err
			}
			add("rescue", resc)
		}
		if len(t.Always) > 0 {
			alw, err := renderTasks(t.Always)
			if err != nil {
				return nil, err
			}
			add("always", alw)
		}
	} else {
		name := t.Action.ModuleName()
		moduleArgs := t.Action.ModuleArgs()
		if ff, ok := t.Action.(freeFormer); ok && ff.freeForm() != "" {
			add(name, ff.freeForm())
			if len(moduleArgs) > 0 {
				add("args", moduleArgs)
			}
		} else if len(moduleArgs) > 0 {
			add(name, moduleArgs)
		} else {
			add(name, nil)
		}
	}

	if len(t.When) == 1 {
		add("when", t.When[0])
	} else if len(t.When) > 1 {
		add("when", strsToList(t.When))
	}
	if t.Loop != nil {
		add("loop", t.Loop)
	}
	if t.LoopVar != "" && t.LoopVar != "item" {
		add("loop_control", map[string]any{"loop_var": t.LoopVar})
	}
	if t.Register != "" {
		add("register", t.Register)
	}
	if t.IgnoreErrors {
		add("ignore_errors", true)
	}
	if len(t.FailedWhen) == 1 {
		add("failed_when", t.FailedWhen[0])
	} else if len(t.FailedWhen) > 1 {
		add("failed_when", strsToList(t.FailedWhen))
	}
	if len(t.ChangedWhen) == 1 {
		add("changed_when", t.ChangedWhen[0])
	} else if len(t.ChangedWhen) > 1 {
		add("changed_when", strsToList(t.ChangedWhen))
	}
	if t.Until != "" {
		add("until", t.Until)
		if t.Retries > 0 {
			add("retries", int64(t.Retries))
		}
		add("delay", int64(t.Delay))
	}
	if t.Become != nil {
		add("become", *t.Become)
	}
	if t.BecomeUser != "" {
		add("become_user", t.BecomeUser)
	}
	if t.BecomeMethod != "" {
		add("become_method", t.BecomeMethod)
	}
	if len(t.Vars) > 0 {
		add("vars", t.Vars)
	}
	if len(t.Environment) > 0 {
		add("environment", t.Environment)
	}
	if len(t.Notify) > 0 {
		add("notify", strsToList(t.Notify))
	}
	if len(t.Tags) > 0 {
		add("tags", strsToList(t.Tags))
	}
	if t.NoLog {
		add("no_log", true)
	}
	if t.DelegateTo != "" {
		add("delegate_to", t.DelegateTo)
	}
	return out, nil
}

func strsToList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
