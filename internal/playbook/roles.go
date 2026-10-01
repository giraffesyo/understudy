package playbook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// LoadTaskFile parses a standalone task-list file (include_tasks target).
// srcDir carries the including task's role root so nested src resolution
// keeps working; blocks flatten as usual.
func LoadTaskFile(path, srcDir string) ([]*Task, error) {
	return loadTaskFile(path, srcDir, false)
}

// LoadHandlerFile parses a standalone handler-list file (a role's
// handlers/*.yml), where listen: is allowed.
func LoadHandlerFile(path string) ([]*Task, error) {
	return loadTaskFile(path, "", true)
}

func loadTaskFile(path, srcDir string, handler bool) ([]*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := yaml.ParseSingle(data, path)
	if err != nil || doc == nil {
		return nil, err
	}
	tasks, err := parseTaskListIn(doc, path, handler, &blockCounter{}, nil)
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

	defaultOrigins, varOrigins []template.KeyOrigin
}

// ResolveRoles materializes each play's roles: role tasks run before the
// play's own tasks (Ansible order: pre_tasks, roles, tasks, post_tasks),
// role handlers join the play's handlers, and defaults/vars are collected
// for the variable store's role layers. Dependencies from meta/main.yml
// load first, deduplicated by name.
func ResolveRoles(plays []*Play, baseDir string, rolesPath []string) error {
	for _, play := range plays {
		// Loading the play (after its roles) announced its tasks'
		// redirects and imports, import_role's role included.
		var taskNotes []string
		for _, list := range [][]*Task{play.Handlers, play.PreTasks, play.PostTasks, play.Tasks} {
			taskNotes = append(taskNotes, importRoleNotes(list, baseDir, rolesPath)...)
		}
		play.LoadNotes = taskNotes
		if len(play.Roles) == 0 {
			continue
		}
		seen := map[string]bool{}
		var roleTasks []*Task
		var notes []string
		for _, ref := range play.Roles {
			play.PlayRoleNames = append(play.PlayRoleNames, ref.Name)
		}
		for _, ref := range play.Roles {
			if err := resolveRoleRef(play, ref, baseDir, rolesPath, seen, &roleTasks, &notes, 0); err != nil {
				return err
			}
		}
		play.LoadNotes = append(notes, play.LoadNotes...)
		play.Tasks = append(roleTasks, play.Tasks...)
		play.Roles = nil // consumed
	}
	return nil
}

// importRoleNotes is TaskLoadNotes, with each import_role followed by
// the notes loading its role printed.
func importRoleNotes(tasks []*Task, baseDir string, rolesPath []string) []string {
	var out []string
	for _, t := range tasks {
		out = append(out, t.LoadNotes...)
		if t.Module != "import_role" {
			continue
		}
		name, _ := t.Args["name"].(string)
		from, _ := t.Args["tasks_from"].(string)
		if name == "" || strings.Contains(name, "{{") {
			continue
		}
		if ri, err := LoadRoleForInclude(name, baseDir, rolesPath, from); err == nil {
			out = append(out, TaskLoadNotes(ri.Tasks)...)
			out = append(out, TaskLoadNotes(ri.Handlers)...)
		}
	}
	return out
}

func resolveRoleRef(play *Play, ref *RoleRef, baseDir string, rolesPath []string, seen map[string]bool, out *[]*Task, notes *[]string, depth int) error {
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
		if !containsStr(play.DependentRoleNames, dep.Name) {
			play.DependentRoleNames = append(play.DependentRoleNames, dep.Name)
		}
		if err := resolveRoleRef(play, dep, baseDir, rolesPath, seen, out, notes, depth+1); err != nil {
			return err
		}
	}
	// A role loads its dependencies, then its tasks and handlers.
	*notes = append(*notes, TaskLoadNotes(role.tasks)...)
	*notes = append(*notes, TaskLoadNotes(role.handlers)...)

	if role.defaults != nil {
		play.RoleDefaults = append(play.RoleDefaults, role.defaults)
		play.RoleDefaultOrigins = append(play.RoleDefaultOrigins, role.defaultOrigins...)
	}
	if role.vars != nil {
		play.RoleVars = append(play.RoleVars, role.vars)
		play.RoleVarOrigins = append(play.RoleVarOrigins, role.varOrigins...)
	}

	// The ref's when/tags/params inherit into every role task.
	inh := &Task{LoopVar: "item", When: ref.When, Tags: ref.Tags, CheckMode: ref.CheckMode, Diff: ref.Diff,
		Environment: ref.Environment, Timeout: ref.Timeout}
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

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// loadRole reads a role directory: tasks/main.yml, handlers/main.yml,
// defaults/main.yml, vars/main.yml, meta/main.yml.
func loadRole(ref *RoleRef, baseDir string, rolesPath []string) (*roleContent, error) {
	dir := findRoleDir(ref.Name, baseDir, rolesPath)
	if dir == "" {
		return nil, &parseError{file: ref.Src.File, line: ref.Src.Line, col: ref.Src.Col,
			msg: roleNotFound(ref.Name, baseDir, rolesPath), notParser: true}
	}
	role := &roleContent{name: ref.Name, dir: dir}

	if node, path, err := loadYAMLMain(filepath.Join(dir, "tasks")); err != nil {
		return nil, err
	} else if node != nil {
		tasks, err := parseTaskListIn(node, path, false, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		for _, t := range flattenRole(tasks, dir, ref.Name) {
			role.tasks = append(role.tasks, t)
		}
	}
	if vt, err := argSpecTask(dir, ref.Name, "main", ref.Params); err != nil {
		return nil, err
	} else if vt != nil {
		role.tasks = append(flattenRole([]*Task{vt}, dir, ref.Name), role.tasks...)
	}

	if node, path, err := loadYAMLMain(filepath.Join(dir, "handlers")); err != nil {
		return nil, err
	} else if node != nil {
		handlers, err := parseTaskListIn(node, path, true, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		role.handlers = flattenRole(handlers, dir, ref.Name)
	}

	var err error
	if role.defaults, role.defaultOrigins, err = loadVarsMainOrigins(filepath.Join(dir, "defaults")); err != nil {
		return nil, err
	}
	if role.vars, role.varOrigins, err = loadVarsMainOrigins(filepath.Join(dir, "vars")); err != nil {
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

// argSpecTask is Role._prepend_validation_task: when the role ships an
// argument spec for the entry point (meta/argument_specs.yml, else
// argument_specs in meta/main.yml), a validate_argument_spec task runs
// before the role's own tasks.
func argSpecTask(dir, roleName, entry string, params map[string]any) (*Task, error) {
	var specs map[string]any
	src := ""
	found := false
	for _, ext := range []string{".yml", ".yaml", ".json"} {
		p := filepath.Join(dir, "meta", "argument_specs"+ext)
		if _, err := os.Stat(p); err == nil {
			node, path, err := loadYAMLBase(filepath.Join(dir, "meta"), "argument_specs")
			if err != nil {
				return nil, err
			}
			found, src = true, path
			if node != nil {
				v, err := node.Decode()
				if err != nil {
					return nil, err
				}
				if m, ok := yaml.PlainMap(v); ok {
					specs, _ = yaml.PlainMap(m["argument_specs"])
				}
			}
			break
		}
	}
	if !found {
		node, path, err := loadYAMLMain(filepath.Join(dir, "meta"))
		if err != nil || node == nil {
			return nil, err
		}
		v, err := node.Decode()
		if err != nil {
			return nil, err
		}
		if m, ok := yaml.PlainMap(v); ok {
			specs, _ = yaml.PlainMap(m["argument_specs"])
		}
		src = path
	}
	if entry == "" {
		entry = "main"
	}
	spec, ok := yaml.PlainMap(specs[entry])
	if !ok || len(spec) == 0 {
		return nil, nil
	}
	name := fmt.Sprintf("Validating arguments against arg spec '%s'", entry)
	if sd, ok := spec["short_description"]; ok {
		name += " - " + fmt.Sprint(sd)
	}
	options, _ := spec["options"]
	if options == nil {
		options = map[string]any{}
	}
	provided := map[string]any{}
	for k, v := range params {
		provided[k] = v
	}
	return &Task{
		Name:    name,
		Module:  "validate_argument_spec",
		Action:  "ansible.builtin.validate_argument_spec",
		LoopVar: "item",
		Poll:    -1,
		Tags:    []string{"always"},
		Args: map[string]any{
			"argument_spec":      options,
			"provided_arguments": provided,
			"validate_args_context": map[string]any{
				"type": "role", "name": roleName, "argument_spec_name": entry, "path": dir,
			},
		},
		Src:         Pos{File: src, Line: 1, Col: 1},
		Synthesized: true,
	}, nil
}

// flattenRole stamps the role's source dir and name onto every task, so src
// resolution and "role : task" banners work uniformly.
func flattenRole(tasks []*Task, dir, roleName string) []*Task {
	for _, t := range tasks {
		t.SrcDir = dir
		if roleName != "" && t.RoleName == "" {
			t.RoleName = roleName
		}
	}
	return tasks
}

// findRoleDir searches baseDir/roles, the configured roles_path entries,
// and baseDir itself.
func findRoleDir(name, baseDir string, rolesPath []string) string {
	for _, dir := range roleSearchPaths(baseDir, rolesPath) {
		c := filepath.Join(dir, name)
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// roleSearchPaths is RoleDefinition's search order: the playbook's roles/
// directory, the configured roles_path, then the playbook directory.
func roleSearchPaths(baseDir string, rolesPath []string) []string {
	return append(append([]string{filepath.Join(baseDir, "roles")}, rolesPath...), baseDir)
}

// roleNotFound is ansible-core's error for a role no search path holds.
func roleNotFound(name, baseDir string, rolesPath []string) string {
	return fmt.Sprintf("The role '%s' was not found in: %s", name, strings.Join(roleSearchPaths(baseDir, rolesPath), ":"))
}

// RoleInclude is a role loaded at runtime for include_role/import_role.
type RoleInclude struct {
	Name     string
	Dir      string
	Tasks    []*Task
	Handlers []*Task
	Defaults map[string]any
	Vars     map[string]any
	// DefaultOrigins and VarOrigins are where they name reserved
	// variables.
	DefaultOrigins, VarOrigins []template.KeyOrigin
}

// LoadRoleForInclude loads a role for include_role: its task file (tasksFrom,
// default "main"), handlers, defaults, and vars. It does not process meta
// dependencies — include_role pulls only the named role's content.
func LoadRoleForInclude(name, baseDir string, rolesPath []string, tasksFrom string) (*RoleInclude, error) {
	dir := findRoleDir(name, baseDir, rolesPath)
	if dir == "" {
		return nil, errors.New(roleNotFound(name, baseDir, rolesPath))
	}
	if tasksFrom == "" {
		tasksFrom = "main"
	}
	ri := &RoleInclude{Name: name, Dir: dir}
	if node, path, err := loadYAMLBase(filepath.Join(dir, "tasks"), tasksFrom); err != nil {
		return nil, err
	} else if node != nil {
		tasks, err := parseTaskListIn(node, path, false, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		ri.Tasks = flattenRole(tasks, dir, name)
	}
	if vt, err := argSpecTask(dir, name, tasksFrom, nil); err != nil {
		return nil, err
	} else if vt != nil {
		ri.Tasks = append(flattenRole([]*Task{vt}, dir, name), ri.Tasks...)
	}
	if node, path, err := loadYAMLMain(filepath.Join(dir, "handlers")); err != nil {
		return nil, err
	} else if node != nil {
		handlers, err := parseTaskListIn(node, path, true, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		ri.Handlers = flattenRole(handlers, dir, name)
	}
	var err error
	if ri.Defaults, ri.DefaultOrigins, err = loadVarsMainOrigins(filepath.Join(dir, "defaults")); err != nil {
		return nil, err
	}
	if ri.Vars, ri.VarOrigins, err = loadVarsMainOrigins(filepath.Join(dir, "vars")); err != nil {
		return nil, err
	}
	return ri, nil
}

// loadYAMLMain loads <dir>/main.yml (or .yaml) as a parse node.
func loadYAMLMain(dir string) (*yaml.Node, string, error) {
	return loadYAMLBase(dir, "main")
}

// loadYAMLBase loads <dir>/<base>.yml (or .yaml, or bare) as a parse node.
func loadYAMLBase(dir, base string) (*yaml.Node, string, error) {
	for _, name := range []string{base + ".yml", base + ".yaml", base} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		doc, err := yaml.ParseSingle(data, path)
		if err != nil {
			return nil, "", err
		}
		return doc, path, nil
	}
	return nil, "", nil
}

// loadVarsMain loads <dir>/main.yml as a vars mapping.
func loadVarsMain(dir string) (map[string]any, error) {
	m, _, err := loadVarsMainOrigins(dir)
	return m, err
}

// loadVarsMainOrigins is loadVarsMain, with where the file names reserved
// variables.
func loadVarsMainOrigins(dir string) (map[string]any, []template.KeyOrigin, error) {
	node, path, err := loadYAMLMain(dir)
	if err != nil || node == nil {
		return nil, nil, err
	}
	m, err := varsOf(node, path)
	if err != nil || m == nil {
		return m, nil, err
	}
	return m, reservedKeyOrigins(node, path), nil
}

// varsOf decodes a vars file's document as a mapping.
func varsOf(node *yaml.Node, path string) (map[string]any, error) {
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return nil, fmt.Errorf("%s: must contain a mapping", path)
	}
	return m, nil
}
