package playbook

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// LoadTaskFile parses a standalone task-list file (include_tasks target).
// srcDir carries the including task's role root so nested src resolution
// keeps working; blocks flatten as usual.
func LoadTaskFile(path, srcDir string) ([]*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := yaml.Parse(data, path)
	if err != nil {
		return nil, err
	}
	if len(f.Docs) == 0 {
		return nil, nil
	}
	tasks, err := parseTaskListIn(f.Docs[0], path, false, &blockCounter{}, nil)
	if err != nil {
		return nil, err
	}
	if srcDir != "" {
		for _, t := range tasks {
			if t.SrcDir == "" {
				t.SrcDir = srcDir
			}
		}
	}
	return tasks, nil
}

// roleContent is a loaded role's parts.
type roleContent struct {
	name     string
	dir      string
	tasks    []*Task
	handlers []*Task
	defaults map[string]any
	vars     map[string]any
	deps     []*RoleRef
}

// ResolveRoles materializes each play's roles: role tasks run before the
// play's own tasks (Ansible order: pre_tasks, roles, tasks, post_tasks),
// role handlers join the play's handlers, and defaults/vars are collected
// for the variable store's role layers. Dependencies from meta/main.yml
// load first, deduplicated by name.
func ResolveRoles(plays []*Play, baseDir string, rolesPath []string) error {
	for _, play := range plays {
		if len(play.Roles) == 0 {
			continue
		}
		seen := map[string]bool{}
		var roleTasks []*Task
		for _, ref := range play.Roles {
			if err := resolveRoleRef(play, ref, baseDir, rolesPath, seen, &roleTasks, 0); err != nil {
				return err
			}
		}
		play.Tasks = append(roleTasks, play.Tasks...)
		play.Roles = nil // consumed
	}
	return nil
}

func resolveRoleRef(play *Play, ref *RoleRef, baseDir string, rolesPath []string, seen map[string]bool, out *[]*Task, depth int) error {
	if depth > 20 {
		return fmt.Errorf("%s: role dependency chain too deep at %q (cycle?)", ref.Src.File, ref.Name)
	}
	// Ansible dedups a role that appears twice without distinct params.
	if seen[ref.Name] && len(ref.Params) == 0 {
		return nil
	}
	seen[ref.Name] = true

	role, err := loadRole(ref, baseDir, rolesPath)
	if err != nil {
		return err
	}

	for _, dep := range role.deps {
		if err := resolveRoleRef(play, dep, baseDir, rolesPath, seen, out, depth+1); err != nil {
			return err
		}
	}

	if role.defaults != nil {
		play.RoleDefaults = append(play.RoleDefaults, role.defaults)
	}
	if role.vars != nil {
		play.RoleVars = append(play.RoleVars, role.vars)
	}

	// The ref's when/tags/params inherit into every role task.
	inh := &Task{LoopVar: "item", When: ref.When, Tags: ref.Tags}
	for _, t := range role.tasks {
		applyBlockInheritance(t, inh)
		if len(ref.Params) > 0 {
			// Role params outrank task vars.
			merged := make(map[string]any, len(t.Vars)+len(ref.Params))
			for k, v := range t.Vars {
				merged[k] = v
			}
			for k, v := range ref.Params {
				merged[k] = v
			}
			t.Vars = merged
		}
	}
	*out = append(*out, role.tasks...)
	play.Handlers = append(play.Handlers, role.handlers...)
	return nil
}

// loadRole reads a role directory: tasks/main.yml, handlers/main.yml,
// defaults/main.yml, vars/main.yml, meta/main.yml.
func loadRole(ref *RoleRef, baseDir string, rolesPath []string) (*roleContent, error) {
	dir := findRoleDir(ref.Name, baseDir, rolesPath)
	if dir == "" {
		return nil, fmt.Errorf("%s: the role %q was not found in %s/roles or the configured roles_path",
			ref.Src.File, ref.Name, baseDir)
	}
	role := &roleContent{name: ref.Name, dir: dir}

	if node, path, err := loadYAMLMain(filepath.Join(dir, "tasks")); err != nil {
		return nil, err
	} else if node != nil {
		tasks, err := parseTaskListIn(node, path, false, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		for _, t := range flattenWithSrcDir(tasks, dir) {
			role.tasks = append(role.tasks, t)
		}
	}

	if node, path, err := loadYAMLMain(filepath.Join(dir, "handlers")); err != nil {
		return nil, err
	} else if node != nil {
		handlers, err := parseTaskListIn(node, path, true, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		role.handlers = flattenWithSrcDir(handlers, dir)
	}

	var err error
	if role.defaults, err = loadVarsMain(filepath.Join(dir, "defaults")); err != nil {
		return nil, err
	}
	if role.vars, err = loadVarsMain(filepath.Join(dir, "vars")); err != nil {
		return nil, err
	}

	// meta/main.yml dependencies.
	if meta, err := loadVarsMain(filepath.Join(dir, "meta")); err != nil {
		return nil, err
	} else if meta != nil {
		if deps, ok := meta["dependencies"].([]any); ok {
			for _, d := range deps {
				dep := &RoleRef{Src: ref.Src}
				switch t := d.(type) {
				case string:
					dep.Name = t
				case map[string]any:
					if name, ok := t["role"].(string); ok {
						dep.Name = name
					} else if name, ok := t["name"].(string); ok {
						dep.Name = name
					}
					for k, v := range t {
						if k == "role" || k == "name" {
							continue
						}
						if dep.Params == nil {
							dep.Params = map[string]any{}
						}
						dep.Params[k] = v
					}
				}
				if dep.Name != "" {
					role.deps = append(role.deps, dep)
				}
			}
		}
	}
	return role, nil
}

func flattenWithSrcDir(tasks []*Task, dir string) []*Task {
	for _, t := range tasks {
		t.SrcDir = dir
	}
	return tasks
}

// findRoleDir searches baseDir/roles, the configured roles_path entries,
// and baseDir itself.
func findRoleDir(name, baseDir string, rolesPath []string) string {
	candidates := []string{filepath.Join(baseDir, "roles", name)}
	for _, rp := range rolesPath {
		candidates = append(candidates, filepath.Join(rp, name))
	}
	candidates = append(candidates, filepath.Join(baseDir, name))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// loadYAMLMain loads <dir>/main.yml (or .yaml) as a parse node.
func loadYAMLMain(dir string) (*yaml.Node, string, error) {
	for _, name := range []string{"main.yml", "main.yaml", "main"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, err := yaml.Parse(data, path)
		if err != nil {
			return nil, "", err
		}
		if len(f.Docs) == 0 {
			return nil, path, nil
		}
		return f.Docs[0], path, nil
	}
	return nil, "", nil
}

// loadVarsMain loads <dir>/main.yml as a vars mapping.
func loadVarsMain(dir string) (map[string]any, error) {
	node, path, err := loadYAMLMain(dir)
	if err != nil || node == nil {
		return nil, err
	}
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must contain a mapping", path)
	}
	return m, nil
}
