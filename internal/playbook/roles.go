package playbook

import (
	"encoding/json"
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
	// allowDuplicates is meta/main.yml's allow_duplicates.
	allowDuplicates bool

	defaultOrigins, varOrigins []template.KeyOrigin
}

// RoleInstance is one load of a role, identified as ansible-core's play
// role cache identifies it (Role._get_hash_dict): the role, its params,
// when, tags, entry-point files and vars, and whether include_role or
// import_role loaded it. Loads with one Key share their "has run" state:
// a role that completed on a host (its tasks ran there to the end, at
// least one not skipped) does not run there again unless it allows
// duplicates.
type RoleInstance struct {
	Name, Path      string
	Key             string
	AllowDuplicates bool
}

// RoleCompleteAction is the implicit end-of-role meta task's action.
const RoleCompleteAction = "role_complete"

// compiledRole is a role compiled with its dependencies (Role.compile):
// each dependency's tasks, then the role's own, each followed by its
// role_complete marker.
type compiledRole struct {
	tasks, handlers            []*Task
	defaults, vars             []map[string]any
	defaultOrigins, varOrigins []template.KeyOrigin
	notes                      []string
	depNames                   []string
}

// roleCompiler is one compilation's settings and what it has collected
// once (a role's handlers, defaults and vars, and load notes, by path).
type roleCompiler struct {
	baseDir   string
	rolesPath []string
	seen      map[string]bool
}

// compile loads ref and, ahead of it, its meta dependencies (recursively,
// each inheriting the keywords and params of the roles that depend on
// it). top describes the role include_role/import_role loads (nil for a
// play role or a dependency).
func (rc *roleCompiler) compile(ref *RoleRef, top *roleIncludeOpts, depth int) (*compiledRole, error) {
	if depth > 20 {
		return nil, fmt.Errorf("%s: role dependency chain too deep at %q (cycle?)", ref.Src.File, ref.Name)
	}
	tasksFrom := ""
	if top != nil {
		tasksFrom = top.tasksFrom
	}
	role, err := loadRole(ref, rc.baseDir, rc.rolesPath, tasksFrom)
	if err != nil {
		return nil, err
	}
	inst := &RoleInstance{Name: ref.Name, Path: role.dir, AllowDuplicates: role.allowDuplicates}
	if top != nil {
		inst.AllowDuplicates = top.allowDuplicates
	}
	inst.Key = roleKey(ref, role.dir, top)

	out := &compiledRole{}
	for _, dep := range role.deps {
		c, err := rc.compile(dep, nil, depth+1)
		if err != nil {
			return nil, err
		}
		out.tasks = append(out.tasks, c.tasks...)
		out.handlers = append(out.handlers, c.handlers...)
		out.defaults = append(out.defaults, c.defaults...)
		out.vars = append(out.vars, c.vars...)
		out.defaultOrigins = append(out.defaultOrigins, c.defaultOrigins...)
		out.varOrigins = append(out.varOrigins, c.varOrigins...)
		out.notes = append(out.notes, c.notes...)
		out.depNames = append(out.depNames, dep.Name)
		out.depNames = append(out.depNames, c.depNames...)
	}
	first := !rc.seen[role.dir]
	rc.seen[role.dir] = true
	if first {
		// A role loads its dependencies, then its tasks and handlers.
		out.notes = append(out.notes, TaskLoadNotes(role.tasks)...)
		out.notes = append(out.notes, TaskLoadNotes(role.handlers)...)
		out.handlers = append(out.handlers, role.handlers...)
		if role.defaults != nil {
			out.defaults = append(out.defaults, role.defaults)
			out.defaultOrigins = append(out.defaultOrigins, role.defaultOrigins...)
		}
		if role.vars != nil {
			out.vars = append(out.vars, role.vars)
			out.varOrigins = append(out.varOrigins, role.varOrigins...)
		}
	}
	for _, t := range role.tasks {
		t.Role = inst
	}
	out.tasks = append(out.tasks, role.tasks...)

	// The ref's keywords and params reach every task it compiled, its
	// dependencies' included (their dep chain); a dependency's own
	// params win over its parents'.
	inh := &Task{LoopVar: "item", When: ref.When, Tags: ref.Tags, CheckMode: ref.CheckMode, Diff: ref.Diff,
		Environment: ref.Environment, Timeout: ref.Timeout}
	for _, t := range out.tasks {
		if t.Implicit {
			continue
		}
		applyBlockInheritance(t, inh)
		addRoleParams(t, ref.Params)
	}
	// The implicit role_complete marker: always runs (tags: always),
	// never displayed.
	out.tasks = append(out.tasks, &Task{
		Module: "meta", Action: "meta", FreeForm: RoleCompleteAction, Implicit: true, Synthesized: true,
		Role: inst, RoleName: ref.Name, SrcDir: role.dir, Tags: []string{"always"}, LoopVar: "item", Poll: -1,
		Src: ref.Src,
	})
	return out, nil
}

// addRoleParams sets role params on a task (they outrank its vars),
// leaving those a dependency's own params already set.
func addRoleParams(t *Task, params map[string]any) {
	if len(params) == 0 {
		return
	}
	merged := make(map[string]any, len(t.Vars)+len(params))
	for k, v := range t.Vars {
		merged[k] = v
	}
	if t.roleParams == nil {
		t.roleParams = map[string]bool{}
	}
	for k, v := range params {
		if t.roleParams[k] {
			continue
		}
		merged[k] = v
		t.roleParams[k] = true
	}
	t.Vars = merged
}

// roleIncludeOpts is how include_role/import_role loads its role.
type roleIncludeOpts struct {
	tasksFrom       string
	vars            map[string]any
	allowDuplicates bool
}

// roleKey is the role cache's identity of a role load.
func roleKey(ref *RoleRef, path string, top *roleIncludeOpts) string {
	params, vars, from := ref.InlineParams, ref.Vars, ""
	fromInclude := top != nil
	if top != nil {
		params, vars, from = nil, top.vars, top.tasksFrom
	}
	key := struct {
		Name, Path  string
		Params      map[string]any
		When, Tags  []string
		From        string
		Vars        map[string]any
		FromInclude bool
	}{ref.Name, path, params, ref.When, ref.Tags, from, vars, fromInclude}
	data, err := json.Marshal(key)
	if err != nil {
		return fmt.Sprintf("%#v", key)
	}
	return string(data)
}

// ResolveRoles materializes each play's roles: role tasks (each role's
// dependencies first) run before the play's own tasks (Ansible order:
// pre_tasks, roles, tasks, post_tasks), role handlers come before the
// play's, and defaults/vars are collected for the variable store's role
// layers. A role listed (or depended on) again is compiled again: the
// run skips what already completed (RoleInstance).
func ResolveRoles(plays []*Play, baseDir string, rolesPath []string) error {
	for _, play := range plays {
		// Loading the play (after its roles) announced its tasks'
		// redirects and imports, import_role's role included.
		var taskNotes []string
		imported := &compiledRole{}
		for _, list := range [][]*Task{play.Handlers, play.PreTasks, play.PostTasks, play.Tasks} {
			taskNotes = append(taskNotes, importRoleNotes(list, baseDir, rolesPath, imported)...)
		}
		play.LoadNotes = taskNotes
		// A statically imported role is one of the play's roles from the
		// start: a public one's defaults and vars are the play's.
		defer func(play *Play) {
			play.RoleDefaults = append(play.RoleDefaults, imported.defaults...)
			play.RoleVars = append(play.RoleVars, imported.vars...)
			play.RoleDefaultOrigins = append(play.RoleDefaultOrigins, imported.defaultOrigins...)
			play.RoleVarOrigins = append(play.RoleVarOrigins, imported.varOrigins...)
		}(play)
		if len(play.Roles) == 0 {
			continue
		}
		rc := &roleCompiler{baseDir: baseDir, rolesPath: rolesPath, seen: map[string]bool{}}
		var roleTasks, roleHandlers []*Task
		var notes []string
		for _, ref := range play.Roles {
			play.PlayRoleNames = append(play.PlayRoleNames, ref.Name)
		}
		for _, ref := range play.Roles {
			c, err := rc.compile(ref, nil, 0)
			if err != nil {
				return err
			}
			for _, d := range c.depNames {
				if !containsStr(play.DependentRoleNames, d) {
					play.DependentRoleNames = append(play.DependentRoleNames, d)
				}
			}
			roleTasks = append(roleTasks, c.tasks...)
			roleHandlers = append(roleHandlers, c.handlers...)
			notes = append(notes, c.notes...)
			play.RoleDefaults = append(play.RoleDefaults, c.defaults...)
			play.RoleVars = append(play.RoleVars, c.vars...)
			play.RoleDefaultOrigins = append(play.RoleDefaultOrigins, c.defaultOrigins...)
			play.RoleVarOrigins = append(play.RoleVarOrigins, c.varOrigins...)
		}
		play.inheritIgnoreErrors(roleTasks)
		play.inheritIgnoreErrors(roleHandlers)
		// The roles' handlers come before the play's own
		// (compile_roles_handlers() + handlers).
		play.Handlers = append(roleHandlers, play.Handlers...)
		play.LoadNotes = append(notes, play.LoadNotes...)
		play.Tasks = append(roleTasks, play.Tasks...)
		play.Roles = nil // consumed
	}
	return nil
}

// importRoleNotes is TaskLoadNotes, with each import_role followed by
// the notes loading its role printed.
func importRoleNotes(tasks []*Task, baseDir string, rolesPath []string, public *compiledRole) []string {
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
		if ri, err := LoadRoleForInclude(name, baseDir, rolesPath, RoleIncludeOptions{TasksFrom: from}); err == nil {
			out = append(out, TaskLoadNotes(ri.Tasks)...)
			out = append(out, TaskLoadNotes(ri.Handlers)...)
			if pub, ok := t.Args["public"]; public != nil && (!ok || isTrue(pub)) {
				public.defaults = append(public.defaults, ri.Defaults...)
				public.vars = append(public.vars, ri.Vars...)
				public.defaultOrigins = append(public.defaultOrigins, ri.DefaultOrigins...)
				public.varOrigins = append(public.varOrigins, ri.VarOrigins...)
			}
		}
	}
	return out
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
func loadRole(ref *RoleRef, baseDir string, rolesPath []string, tasksFrom string) (*roleContent, error) {
	dir := findRoleDir(ref.Name, baseDir, rolesPath)
	if dir == "" {
		return nil, &parseError{file: ref.Src.File, line: ref.Src.Line, col: ref.Src.Col,
			msg: roleNotFound(ref.Name, baseDir, rolesPath), notParser: true}
	}
	role := &roleContent{name: ref.Name, dir: dir}

	if tasksFrom == "" {
		tasksFrom = "main"
	}
	if node, path, err := loadYAMLBase(filepath.Join(dir, "tasks"), tasksFrom); err != nil {
		return nil, err
	} else if node != nil {
		tasks, err := parseTaskListIn(node, path, false, &blockCounter{}, nil)
		if err != nil {
			return nil, err
		}
		role.tasks = append(role.tasks, flattenRole(tasks, dir, ref.Name)...)
	}
	if vt, err := argSpecTask(dir, ref.Name, tasksFrom, ref.Params); err != nil {
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

	// meta/main.yml: allow_duplicates and dependencies (role entries as
	// in a play's roles:).
	if node, path, err := loadYAMLMain(filepath.Join(dir, "meta")); err != nil {
		return nil, err
	} else if node != nil {
		if v := node.MapGet("allow_duplicates"); v != nil {
			if dv, err := v.Decode(); err == nil {
				role.allowDuplicates, _ = ParseBool(dv)
			}
		}
		if deps := node.MapGet("dependencies"); deps != nil && !deps.IsNull() {
			refs, err := parseRoleRefs(deps, path)
			if err != nil {
				return nil, err
			}
			role.deps = refs
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

// RoleInclude is a role loaded at runtime for include_role/import_role,
// compiled with its dependencies.
type RoleInclude struct {
	Name     string
	Dir      string
	Tasks    []*Task // dependencies' tasks, then the role's, with their role_complete markers
	Handlers []*Task
	Defaults []map[string]any
	Vars     []map[string]any
	// DefaultOrigins and VarOrigins are where they name reserved
	// variables.
	DefaultOrigins, VarOrigins []template.KeyOrigin
	// DependentRoleNames are the dependencies' names.
	DependentRoleNames []string
}

// RoleIncludeOptions are include_role/import_role's options for loading
// the role.
type RoleIncludeOptions struct {
	TasksFrom string
	// Vars are the include task's vars (part of the role's identity).
	Vars map[string]any
	// AllowDuplicates is the include's allow_duplicates (default true).
	AllowDuplicates bool
	// Src is the include task's position.
	Src Pos
}

// LoadRoleForInclude loads a role for include_role/import_role: its
// meta dependencies (compiled first), its task file (TasksFrom, default
// "main"), handlers, defaults, and vars.
func LoadRoleForInclude(name, baseDir string, rolesPath []string, o RoleIncludeOptions) (*RoleInclude, error) {
	dir := findRoleDir(name, baseDir, rolesPath)
	if dir == "" {
		return nil, errors.New(roleNotFound(name, baseDir, rolesPath))
	}
	rc := &roleCompiler{baseDir: baseDir, rolesPath: rolesPath, seen: map[string]bool{}}
	ref := &RoleRef{Name: name, Src: o.Src}
	c, err := rc.compile(ref, &roleIncludeOpts{tasksFrom: o.TasksFrom, vars: o.Vars, allowDuplicates: o.AllowDuplicates}, 0)
	if err != nil {
		return nil, err
	}
	return &RoleInclude{Name: name, Dir: dir, Tasks: c.tasks, Handlers: c.handlers,
		Defaults: c.defaults, Vars: c.vars, DefaultOrigins: c.defaultOrigins, VarOrigins: c.varOrigins,
		DependentRoleNames: c.depNames}, nil
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

// isTrue is a boolean option's value as written.
func isTrue(v any) bool {
	b, ok := ParseBool(v)
	return ok && b
}
