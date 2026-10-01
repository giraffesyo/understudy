package playbook

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// ModuleKnown reports whether a name is a runnable module or action; wired
// by the executor at startup (it knows both registries).
var ModuleKnown = func(name string) bool { return true }

// taskKeywords are task keys that are NOT the module name.
var taskKeywords = map[string]bool{
	"name": true, "when": true, "loop": true, "with_items": true,
	"loop_control": true, "register": true, "ignore_errors": true,
	"failed_when": true, "changed_when": true, "until": true, "retries": true,
	"delay": true, "become": true, "become_user": true, "become_method": true,
	"vars": true, "environment": true, "notify": true, "tags": true,
	"no_log": true, "delegate_to": true, "args": true, "listen": true,
	"check_mode": true, "diff": true, "run_once": true, "any_errors_fatal": true,
	"async": true, "poll": true, "delegate_facts": true, "throttle": true,
	"timeout": true, "ignore_unreachable": true, "remote_user": true,
	"connection": true, "collections": true, "module_defaults": true,
	"debugger": true, "become_flags": true, "become_exe": true, "port": true,
	"action": true, "local_action": true,
}

// playKeywords are recognized play-level keys.
var playKeywords = map[string]bool{
	"name": true, "hosts": true, "vars": true, "vars_files": true,
	"gather_facts": true, "become": true, "become_user": true,
	"become_method": true, "tasks": true, "handlers": true,
	"pre_tasks": true, "post_tasks": true, "tags": true, "environment": true,
	"remote_user": true, "connection": true, "serial": true, "strategy": true,
	"max_fail_percentage": true, "any_errors_fatal": true, "roles": true,
	"force_handlers": true, "vars_prompt": true,
	"gather_subset": true, "gather_timeout": true, "fact_path": true,
	"check_mode": true, "diff": true, "become_flags": true, "become_exe": true,
	"debugger": true, "timeout": true, "ignore_errors": true,
}

// Deferred play keys that must fail loudly rather than be ignored.
var unsupportedPlayKeys = map[string]string{}

type parseError struct {
	file      string
	line, col int
	msg       string
	// notParser marks an AnsibleError that is not an AnsibleParserError
	// (ansible-playbook exits 1 for it instead of 4).
	notParser bool
	// help is the error's help text, shown after its source excerpt.
	help string
	// cause, when set, is the error this one was raised from: shown
	// after "<<< caused by >>>" with its own origin and the help text.
	cause *parseError
}

// HelpText is the error's help text.
func (e *parseError) HelpText() string { return e.help }

// Formatted is the error as Display.error shows a chained error, ""
// when it has no cause.
func (e *parseError) Formatted() string {
	if e.cause == nil {
		return ""
	}
	c := e.cause
	var b strings.Builder
	fmt.Fprintf(&b, "[ERROR]: %s: %s\n\n%s\n", strings.TrimRight(e.msg, ". "), c.msg, e.msg)
	fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n%s\n", e.file, e.line, e.col, SourceContext(e.file, e.line, e.col))
	fmt.Fprintf(&b, "<<< caused by >>>\n\n%s\n", c.msg)
	fmt.Fprintf(&b, "Origin: %s:%d:%d\n\n%s\n", c.file, c.line, c.col, SourceContext(c.file, c.line, c.col))
	if c.help != "" {
		b.WriteString(c.help + "\n\n")
	}
	return b.String()
}

// ExitCode is ansible-playbook's exit status for the error.
func (e *parseError) ExitCode() int {
	if e.notParser {
		return 1
	}
	return 4
}

func (e *parseError) Error() string {
	return fmt.Sprintf("%s:%d: %s", e.file, e.line, e.msg)
}

// Message is the error without its position.
func (e *parseError) Message() string { return e.msg }

// Origin is where the error points (line 0: nowhere in particular).
func (e *parseError) Origin() (file string, line, col int) { return e.file, e.line, e.col }

// OriginError is a load error that points into a playbook file; the CLI
// shows it as ansible-core does, with an "Origin:" line and the source.
type OriginError interface {
	error
	Message() string
	Origin() (file string, line, col int)
}

func errAt(file string, node *yaml.Node, format string, args ...any) error {
	e := &parseError{file: file, msg: fmt.Sprintf(format, args...)}
	if node != nil {
		e.line, e.col = node.Line, node.Column
	}
	return e
}

// pyTaggedType is the Python type ansible-core reports for a loaded YAML
// value (its origin-tagged containers and scalars).
func pyTaggedType(v any) string {
	tagged := func(name string) string {
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTagged" + name + "'>"
	}
	switch v.(type) {
	case nil:
		return "<class 'NoneType'>"
	case bool:
		return "<class 'bool'>"
	case string:
		return tagged("Str")
	case int, int64:
		return tagged("Int")
	case float64:
		return tagged("Float")
	case []any:
		return tagged("List")
	}
	return tagged("Dict")
}

// keyPos is the source position of a mapping key.
func keyPos(node *yaml.Node, key, file string) Pos {
	if k := node.MapKeyNode(key); k != nil {
		return Pos{File: file, Line: k.Line, Col: k.Column}
	}
	return Pos{File: file, Line: node.Line, Col: node.Column}
}

// errAtKey points at a mapping key (ansible-core's origin for an invalid
// attribute).
func errAtKey(file string, node *yaml.Node, key string, format string, args ...any) error {
	if k := node.MapKeyNode(key); k != nil {
		node = k
	}
	return errAt(file, node, format, args...)
}

// playAttributes are ansible-core's Play attributes. A key outside them is
// "not a valid attribute"; one inside them that understudy doesn't
// implement fails with its own message.
var playAttributes = map[string]bool{
	"any_errors_fatal": true, "become": true, "become_exe": true, "become_flags": true,
	"become_method": true, "become_user": true, "check_mode": true, "collections": true,
	"connection": true, "debugger": true, "diff": true, "environment": true,
	"fact_path": true, "force_handlers": true, "gather_facts": true, "gather_subset": true,
	"gather_timeout": true, "handlers": true, "hosts": true, "ignore_errors": true,
	"ignore_unreachable": true, "max_fail_percentage": true, "module_defaults": true,
	"name": true, "no_log": true, "order": true, "port": true, "post_tasks": true,
	"pre_tasks": true, "remote_user": true, "roles": true, "run_once": true, "serial": true,
	"strategy": true, "tags": true, "tasks": true, "throttle": true, "timeout": true,
	"validate_argspec": true, "vars": true, "vars_files": true, "vars_prompt": true,
}

// blockAttributes are ansible-core's Block attributes.
var blockAttributes = map[string]bool{
	"always": true, "any_errors_fatal": true, "become": true, "become_exe": true,
	"become_flags": true, "become_method": true, "become_user": true, "block": true,
	"check_mode": true, "collections": true, "connection": true, "debugger": true,
	"delegate_facts": true, "delegate_to": true, "diff": true, "environment": true,
	"ignore_errors": true, "ignore_unreachable": true, "module_defaults": true,
	"name": true, "no_log": true, "notify": true, "port": true, "remote_user": true,
	"rescue": true, "run_once": true, "tags": true, "throttle": true, "timeout": true,
	"vars": true, "when": true,
}

// loadState records the tasks a playbook load has begun, in order.
type loadState struct{ tasks []*Task }

// loading is the playbook load in progress (LoadFileTasks), if any.
var (
	loading   atomic.Pointer[loadState]
	loadingMu sync.Mutex
)

// LoadFileTasks is LoadFile that also returns the tasks it loaded (all
// that it began, when it fails): ansible-core reports each task's load
// deprecations as it loads it, before a later load error.
func LoadFileTasks(path string) ([]*Play, []*Task, error) {
	loadingMu.Lock()
	defer loadingMu.Unlock()
	st := &loadState{}
	loading.Store(st)
	defer loading.Store(nil)
	plays, err := LoadFile(path)
	return plays, st.tasks, err
}

// LoadFile parses a playbook file into plays.
func LoadFile(path string) ([]*Play, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data, path)
}

// Load parses playbook YAML into plays.
func Load(data []byte, filename string) ([]*Play, error) {
	doc, err := yaml.ParseSingle(data, filename)
	if err != nil {
		return nil, err
	}
	if doc == nil || doc.IsNull() {
		return nil, &parseError{file: filename, msg: "Empty playbook, nothing to do: " + filename}
	}
	items, ok := doc.Seq()
	if !ok {
		v, _ := doc.Decode()
		return nil, errAt(filename, doc, "A playbook must be a list of plays, got a %s instead: %s", pyTaggedType(v), filename)
	}
	var plays []*Play
	for _, item := range items {
		play, err := parsePlay(item, filename)
		if err != nil {
			return nil, err
		}
		plays = append(plays, play)
	}
	return plays, nil
}

func parsePlay(node *yaml.Node, file string) (*Play, error) {
	keys := node.MapKeys()
	if keys == nil {
		return nil, errAt(file, node, "a play must be a mapping")
	}
	play := &Play{
		Src:               Pos{File: file, Line: node.Line, Col: node.Column},
		MaxFailPercentage: -1,
	}
	if node.MapGet("hosts") == nil {
		return nil, errAt(file, node, "a play requires a 'hosts' field")
	}

	for _, key := range keys {
		val := node.MapGet(key)
		if msg, bad := unsupportedPlayKeys[key]; bad {
			if v, err := val.Decode(); err == nil && !emptyValue(v) {
				return nil, errAt(file, val, "%s", msg)
			}
			continue
		}
		if !playKeywords[key] {
			if !playAttributes[key] {
				return nil, errAtKey(file, node, key, "'%s' is not a valid attribute for a Play", key)
			}
			return nil, errAtKey(file, node, key, "play keyword %q is not supported by understudy", key)
		}
		switch key {
		case "name":
			play.Name, _ = val.Str()
		case "check_mode", "diff":
			b, err := decodeBool(val, file, key)
			if err != nil {
				return nil, err
			}
			if key == "check_mode" {
				play.CheckMode = &b
			} else {
				play.Diff = &b
			}
		case "hosts":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			switch t := v.(type) {
			case string:
				play.HostPattern = t
			case []any:
				parts := make([]string, len(t))
				for i, p := range t {
					parts[i] = fmt.Sprintf("%v", p)
				}
				play.HostPattern = strings.Join(parts, ":")
			default:
				return nil, errAt(file, val, "'hosts' must be a string or list")
			}
		case "vars":
			v, err := decodeMap(val, file, "vars")
			if err != nil {
				return nil, err
			}
			if err := checkVarNames(val, file); err != nil {
				return nil, err
			}
			play.Vars = v
			play.VarOrigins = reservedKeyOrigins(val, file)
		case "vars_files":
			items, ok := val.Seq()
			if !ok {
				return nil, errAt(file, val, "'vars_files' must be a list")
			}
			for _, it := range items {
				s, _ := it.Str()
				play.VarsFiles = append(play.VarsFiles, s)
			}
		case "gather_facts":
			b, err := decodeBool(val, file, "gather_facts")
			if err != nil {
				return nil, err
			}
			play.GatherFacts = &b
		case "become", "become_user", "become_method", "become_flags", "become_exe":
			if err := parseBecomeKey(&play.Become, key, val, file); err != nil {
				return nil, err
			}
		case "debugger":
			d, err := parseDebugger(val, file)
			if err != nil {
				return nil, err
			}
			play.Debugger = d
		case "tasks", "pre_tasks", "post_tasks", "handlers":
			if v, err := val.Decode(); err == nil && v != nil {
				if _, isList := v.([]any); !isList {
					return nil, errAt(file, node, "A malformed block was encountered while loading %s: %s should be a list or None but is %s",
						key, template.PyRepr(v), pyTaggedType(v))
				}
			}
			tasks, err := parseTaskList(val, file, key == "handlers")
			if err != nil {
				return nil, err
			}
			switch key {
			case "tasks":
				play.Tasks = tasks
			case "pre_tasks":
				play.PreTasks = tasks
			case "post_tasks":
				play.PostTasks = tasks
			case "handlers":
				play.Handlers = tasks
			}
		case "tags":
			play.Tags = decodeStringList(val)
		case "roles":
			refs, err := parseRoleRefs(val, file)
			if err != nil {
				return nil, err
			}
			play.Roles = refs
		case "environment":
			v, err := decodeEnvironment(val)
			if err != nil {
				return nil, err
			}
			play.Environment = v
		case "timeout":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			play.Timeout = v
		case "serial":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			switch t := v.(type) {
			case []any:
				play.Serial = t
			case nil:
				// absent
			default:
				play.Serial = []any{t}
			}
		case "max_fail_percentage":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			switch t := v.(type) {
			case int64:
				play.MaxFailPercentage = float64(t)
			case float64:
				play.MaxFailPercentage = t
			default:
				return nil, errAt(file, val, "max_fail_percentage must be a number")
			}
		case "vars_prompt":
			prompts, err := parseVarsPrompt(val, file)
			if err != nil {
				return nil, err
			}
			play.VarsPrompt = prompts
		case "strategy":
			s, _ := val.Str()
			switch s {
			case "linear", "free", "host_pinned", "debug":
				play.Strategy = s
			case "ansible.builtin.linear", "ansible.builtin.free", "ansible.builtin.host_pinned", "ansible.builtin.debug":
				play.Strategy = strings.TrimPrefix(s, "ansible.builtin.")
			case "":
			default:
				return nil, errAt(file, val, "strategy %q is not supported (linear, free, host_pinned, debug)", s)
			}
		case "remote_user":
			play.RemoteUser, _ = val.Str()
		case "connection":
			play.Connection, _ = val.Str()
		case "any_errors_fatal":
			b, err := decodeBool(val, file, "any_errors_fatal")
			if err != nil {
				return nil, err
			}
			play.AnyErrorsFatal = b
		case "force_handlers":
			b, err := decodeBool(val, file, "force_handlers")
			if err != nil {
				return nil, err
			}
			play.ForceHandlers = b
		case "ignore_errors":
			inh := &Task{}
			var err error
			if inh.IgnoreErrors, _, err = decodeBoolKW(inh, val, file, "ignore_errors"); err != nil {
				return nil, err
			}
			play.ignoreErrors = inh
		case "gather_subset", "gather_timeout", "fact_path":
			// Passed through to the implicit setup task's arguments.
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			if v != nil {
				if play.GatherArgs == nil {
					play.GatherArgs = map[string]any{}
				}
				play.GatherArgs[key] = yaml.AsMap(v)
			}
		}
	}
	for _, list := range [][]*Task{play.PreTasks, play.Tasks, play.PostTasks, play.Handlers} {
		play.inheritIgnoreErrors(list)
	}
	return play, nil
}

// TaskLoadNotes gathers the tasks' LoadNotes in order.
func TaskLoadNotes(tasks []*Task) []string {
	var out []string
	for _, t := range tasks {
		out = append(out, t.LoadNotes...)
	}
	return out
}

// blockCounter hands out unique block IDs within one Load call.
type blockCounter struct{ next int }

func parseTaskList(node *yaml.Node, file string, handlers bool) ([]*Task, error) {
	return parseTaskListIn(node, file, handlers, &blockCounter{}, nil)
}

func parseTaskListIn(node *yaml.Node, file string, handlers bool, bc *blockCounter, enclosing []BlockRef) ([]*Task, error) {
	if s, ok := node.Str(); ok && s == "" {
		return nil, nil // tasks: (empty)
	}
	items, ok := node.Seq()
	if !ok {
		return nil, errAt(file, node, "expected a list of tasks")
	}
	var tasks []*Task
	for _, item := range items {
		if item.MapGet("block") != nil {
			flat, err := parseBlock(item, file, handlers, bc, enclosing)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, flat...)
			continue
		}
		if node := importTasksNode(item); node != nil {
			flat, err := parseImportTasks(item, node, file, handlers, bc, enclosing)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, flat...)
			continue
		}
		task, err := parseTask(item, file, handlers)
		if err != nil {
			return nil, err
		}
		task.Blocks = enclosing
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func importTasksNode(item *yaml.Node) *yaml.Node {
	for _, key := range []string{"import_tasks", "ansible.builtin.import_tasks"} {
		if n := item.MapGet(key); n != nil {
			return n
		}
	}
	return nil
}

// parseImportTasks splices a static task-file import inline. The import
// entry's own keywords (when/vars/tags/become...) inherit into every
// imported task, like Ansible's static imports.
func parseImportTasks(item, pathNode *yaml.Node, file string, handlers bool, bc *blockCounter, enclosing []BlockRef) ([]*Task, error) {
	rel := importPath(pathNode)
	if rel == "" {
		return nil, errAt(file, pathNode, "import_tasks requires a file name")
	}
	if strings.Contains(rel, "{{") {
		return nil, errAt(file, pathNode,
			"import_tasks cannot use templated paths (imports are static); use include_tasks")
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(file), rel)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// The loader's error, which carries no origin.
		return nil, &parseError{file: file, notParser: true, msg: fmt.Sprintf("Unable to retrieve file contents. Could not find or access '%s' "+
			"on the Ansible Controller: [Errno 2] No such file or directory: '%s' If you are using a module and expect "+
			"the file to exist on the remote, see the remote_src option.", path, path)}
	}
	if err != nil {
		return nil, errAt(file, pathNode, "import_tasks: %v", err)
	}
	doc, err := yaml.ParseSingle(data, path)
	if err != nil || doc == nil {
		return nil, err
	}
	tasks, err := parseTaskListIn(doc, path, handlers, bc, enclosing)
	if err != nil {
		return nil, err
	}
	if len(tasks) > 0 {
		// The loader announces the file before its tasks load.
		tasks[0].LoadNotes = append([]string{"statically imported: " + path}, tasks[0].LoadNotes...)
	}

	// Inheritance from the import entry itself: a static import is its
	// tasks' parent, so every inheritable keyword on it applies.
	inh, err := parseInheritable(item, file)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		applyBlockInheritance(t, inh)
	}
	return tasks, nil
}

// importPath extracts the file path from `import_tasks: x.yml` or the map
// form {file: x.yml}.
func importPath(node *yaml.Node) string {
	if s, ok := node.Str(); ok {
		return s
	}
	if fileNode := node.MapGet("file"); fileNode != nil {
		s, _ := fileNode.Str()
		return s
	}
	return ""
}

// parseRoleRefs handles the roles: list forms — bare names, {role: x, ...},
// and old-style inline params.
func parseRoleRefs(node *yaml.Node, file string) ([]*RoleRef, error) {
	items, ok := node.Seq()
	if !ok {
		if s, isStr := node.Str(); isStr && s == "" {
			return nil, nil
		}
		return nil, errAt(file, node, "'roles' must be a list")
	}
	var refs []*RoleRef
	for _, item := range items {
		ref := &RoleRef{Src: Pos{File: file, Line: item.Line, Col: item.Column}}
		if name, isStr := item.Str(); isStr {
			ref.Name = name
			refs = append(refs, ref)
			continue
		}
		keys := item.MapKeys()
		if keys == nil {
			return nil, errAt(file, item, "a role entry must be a name or a mapping")
		}
		for _, key := range keys {
			val := item.MapGet(key)
			switch key {
			case "role", "name":
				ref.Name, _ = val.Str()
			case "when":
				ref.When = decodeExprList(val)
			case "tags":
				ref.Tags = decodeStringList(val)
			case "vars":
				m, err := decodeMap(val, file, "vars")
				if err != nil {
					return nil, err
				}
				if ref.Params == nil {
					ref.Params = map[string]any{}
				}
				for k, v := range m {
					ref.Params[k] = v
				}
				ref.Vars = m
			case "check_mode", "diff":
				b, err := decodeBool(val, file, key)
				if err != nil {
					return nil, err
				}
				if key == "check_mode" {
					ref.CheckMode = &b
				} else {
					ref.Diff = &b
				}
			case "environment":
				env, err := decodeEnvironment(val)
				if err != nil {
					return nil, err
				}
				ref.Environment = env
			case "timeout":
				v, err := val.Decode()
				if err != nil {
					return nil, err
				}
				ref.Timeout = v
			case "become", "become_user", "delegate_to":
				return nil, errAt(file, val, "role keyword %q is not supported yet", key)
			default:
				// Old-style inline parameter: {role: x, port: 8080}.
				v, err := val.Decode()
				if err != nil {
					return nil, err
				}
				if ref.Params == nil {
					ref.Params = map[string]any{}
				}
				ref.Params[key] = v
				if ref.InlineParams == nil {
					ref.InlineParams = map[string]any{}
				}
				ref.InlineParams[key] = v
			}
		}
		if ref.Name == "" {
			return nil, errAt(file, item, "role entry is missing the role name")
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// blockKeywords are the keys legal on a block entry.
var blockKeywords = map[string]bool{
	"block": true, "rescue": true, "always": true, "name": true,
	"when": true, "become": true, "become_user": true, "become_method": true,
	"vars": true, "tags": true, "environment": true, "no_log": true,
	"ignore_errors": true, "check_mode": true, "diff": true, "delegate_to": true, "any_errors_fatal": true,
	"run_once": true, "remote_user": true, "connection": true, "become_flags": true,
	"become_exe": true, "collections": true, "module_defaults": true, "throttle": true,
	"timeout": true, "ignore_unreachable": true, "debugger": true, "port": true,
	"notify": true,
}

// parseBlock flattens a block/rescue/always entry: block-level keywords are
// inherited into contained tasks, and each task records its block refs so
// the executor can route failures to rescue.
func parseBlock(node *yaml.Node, file string, handlers bool, bc *blockCounter, enclosing []BlockRef) ([]*Task, error) {
	for _, key := range node.MapKeys() {
		if !blockKeywords[key] {
			if !blockAttributes[key] {
				return nil, errAtKey(file, node, key, "'%s' is not a valid attribute for a Block", key)
			}
			return nil, errAtKey(file, node, key, "block keyword %q is not supported by understudy", key)
		}
	}

	// The inheritable keywords, parsed once.
	inh, err := parseInheritable(node, file)
	if err != nil {
		return nil, err
	}

	id := bc.next
	bc.next++
	hasRescue := node.MapGet("rescue") != nil
	hasAlways := node.MapGet("always") != nil

	var out []*Task
	for _, sec := range []struct {
		key     string
		section int
	}{
		{"block", SectionBlock},
		{"rescue", SectionRescue},
		{"always", SectionAlways},
	} {
		secNode := node.MapGet(sec.key)
		if secNode == nil {
			continue
		}
		refs := append(append([]BlockRef{}, enclosing...),
			BlockRef{ID: id, Section: sec.section, HasRescue: hasRescue, HasAlways: hasAlways,
				Parallel: sec.section == SectionBlock && truthyVar(inh.Vars["understudy_parallel"])})
		tasks, err := parseTaskListIn(secNode, file, handlers, bc, refs)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			applyBlockInheritance(t, inh)
		}
		out = append(out, tasks...)
	}
	if len(out) == 0 {
		return nil, errAt(file, node, "a block must contain at least one task")
	}
	return out, nil
}

// parseInheritable parses the keywords a block, a static import or an
// include's apply: passes down to the tasks it contains (other keys are
// ignored).
func parseInheritable(node *yaml.Node, file string) (*Task, error) {
	inh := &Task{LoopVar: "item"}
	for _, key := range node.MapKeys() {
		val := node.MapGet(key)
		var err error
		switch key {
		case "when":
			inh.When, inh.WhenPos = decodeExprList(val), exprPositions(val, file)
		case "become":
			var b, ok bool
			if b, ok, err = decodeBoolKW(inh, val, file, "become"); err == nil && ok {
				inh.Become.Become = &b
			}
		case "become_user", "become_method", "become_flags", "become_exe":
			err = parseBecomeKey(&inh.Become, key, val, file)
		case "debugger":
			inh.Debugger, err = parseDebugger(val, file)
		case "vars":
			inh.Vars, err = decodeMap(val, file, "vars")
			if err == nil {
				err = checkVarNames(val, file)
			}
			inh.VarOrigins = taskVarOrigins(val, file)
		case "tags":
			inh.Tags = decodeStringList(val)
		case "environment":
			inh.Environment, err = decodeEnvironment(val)
		case "timeout":
			inh.Timeout, err = val.Decode()
		case "no_log":
			inh.NoLog, _, err = decodeBoolKW(inh, val, file, "no_log")
		case "ignore_errors":
			inh.IgnoreErrors, _, err = decodeBoolKW(inh, val, file, "ignore_errors")
		case "delegate_to":
			inh.Delegate, _ = val.Str()
		case "check_mode":
			var b bool
			if b, err = decodeBool(val, file, "check_mode"); err == nil {
				inh.CheckMode = &b
			}
		case "diff":
			var b bool
			if b, err = decodeBool(val, file, "diff"); err == nil {
				inh.Diff = &b
			}
		case "run_once":
			inh.RunOnce, err = decodeBool(val, file, "run_once")
		case "any_errors_fatal":
			var b bool
			if b, err = decodeBool(val, file, "any_errors_fatal"); err == nil {
				inh.AnyErrorsFatal = &b
			}
		case "remote_user":
			inh.RemoteUser, _ = val.Str()
		case "connection":
			inh.Connection, _ = val.Str()
		}
		if err != nil {
			return nil, err
		}
	}
	return inh, nil
}

// inheritIgnoreErrors gives t the ignore_errors of inh (an enclosing
// block, include or the play) when t sets none: the nearest setting wins.
func inheritIgnoreErrors(t, inh *Task) {
	if t.hasLiteral("ignore_errors") || t.KeywordTemplates["ignore_errors"] != "" {
		return
	}
	if inh.hasLiteral("ignore_errors") {
		t.IgnoreErrors = inh.IgnoreErrors
		t.markLiteral("ignore_errors")
	} else if s := inh.KeywordTemplates["ignore_errors"]; s != "" {
		setKeywordTemplate(t, "ignore_errors", s)
	} else {
		t.IgnoreErrors = t.IgnoreErrors || inh.IgnoreErrors
	}
}

// inheritIgnoreErrors gives the play's ignore_errors to the tasks that
// set none of their own.
func (p *Play) inheritIgnoreErrors(tasks []*Task) {
	if p.ignoreErrors == nil {
		return
	}
	for _, t := range tasks {
		inheritIgnoreErrors(t, p.ignoreErrors)
		if t.IsDynamicInclude() {
			// What the include's tasks inherit from above it.
			if t.Parents == nil {
				t.Parents = &Task{LoopVar: "item"}
			}
			inheritIgnoreErrors(t.Parents, p.ignoreErrors)
		}
	}
}

// Inherit applies a parent's inheritable keywords to a task, as a block
// does to its tasks (the task's own settings win).
func Inherit(t, parent *Task) { applyBlockInheritance(t, parent) }

// applyBlockInheritance merges block-level keywords into a task: when
// clauses AND together; task-level settings win on conflicts.
func applyBlockInheritance(t *Task, inh *Task) {
	if t.IsDynamicInclude() {
		// A dynamic include is not its tasks' parent (ansible-core skips
		// a parent that is not statically loaded): they inherit from its
		// enclosing blocks and role, not from the include's own keywords.
		if t.Parents == nil {
			t.Parents = &Task{LoopVar: "item"}
		}
		applyBlockInheritance(t.Parents, inh)
	}
	if len(inh.When) > 0 {
		t.When = append(append([]string{}, inh.When...), t.When...)
		for k, p := range inh.WhenPos {
			if _, own := t.WhenPos[k]; !own {
				if t.WhenPos == nil {
					t.WhenPos = map[string]Pos{}
				}
				t.WhenPos[k] = p
			}
		}
	}
	if t.Become.Become == nil {
		t.Become.Become = inh.Become.Become
	}
	if t.Become.BecomeUser == "" {
		t.Become.BecomeUser = inh.Become.BecomeUser
	}
	if t.Become.Method == "" {
		t.Become.Method = inh.Become.Method
	}
	if t.Become.Flags == nil {
		t.Become.Flags = inh.Become.Flags
	}
	if t.Become.Exe == "" {
		t.Become.Exe = inh.Become.Exe
	}
	if t.Debugger == "" {
		t.Debugger = inh.Debugger
	}
	if len(inh.Vars) > 0 {
		merged := make(map[string]any, len(inh.Vars)+len(t.Vars))
		for k, v := range inh.Vars {
			merged[k] = v
		}
		for k, v := range t.Vars {
			merged[k] = v
		}
		t.Vars = merged
	}
	if len(inh.VarOrigins) > 0 {
		t.VarOrigins = append(append([]template.KeyOrigin{}, inh.VarOrigins...), t.VarOrigins...)
	}
	if len(inh.Tags) > 0 {
		t.Tags = append(append([]string{}, inh.Tags...), t.Tags...)
	}
	if len(inh.Environment) > 0 {
		// The enclosing environments come first: their values merge in
		// before the task's own (ansible-core's prepend-extended list).
		t.Environment = append(append([]any{}, inh.Environment...), t.Environment...)
	}
	if t.Timeout == nil {
		t.Timeout = inh.Timeout
	}
	t.NoLog = t.NoLog || inh.NoLog
	inheritIgnoreErrors(t, inh)
	if t.Delegate == "" {
		t.Delegate = inh.Delegate
	}
	if t.AnyErrorsFatal == nil {
		t.AnyErrorsFatal = inh.AnyErrorsFatal
	}
	// check_mode/diff: the nearest explicit setting (task, then the
	// innermost block) wins.
	if t.CheckMode == nil {
		t.CheckMode = inh.CheckMode
	}
	if t.Diff == nil {
		t.Diff = inh.Diff
	}
	t.RunOnce = t.RunOnce || inh.RunOnce
	if t.RemoteUser == "" {
		t.RemoteUser = inh.RemoteUser
	}
	if t.Connection == "" {
		t.Connection = inh.Connection
	}
	// Templated keywords inherit unless the task sets its own value.
	for k, v := range inh.KeywordTemplates {
		if _, own := t.KeywordTemplates[k]; own || t.hasLiteral(k) {
			continue
		}
		if t.KeywordTemplates == nil {
			t.KeywordTemplates = map[string]string{}
		}
		t.KeywordTemplates[k] = v
	}
}

func parseTask(node *yaml.Node, file string, handler bool) (*Task, error) {
	keys := node.MapKeys()
	if keys == nil {
		return nil, errAt(file, node, "a task must be a mapping")
	}
	task := &Task{
		LoopVar: "item",
		Poll:    -1, // unset; 0 means fire-and-forget
		Src:     Pos{File: file, Line: node.Line, Col: node.Column},
	}
	if l := loading.Load(); l != nil {
		l.tasks = append(l.tasks, task)
	}

	// Task.preprocess_data: a with_<lookup> after loop: or another
	// with_<lookup> is a duplicate loop.
	looped := false
	for _, key := range keys {
		if key == "loop" {
			if v, err := node.MapGet(key).Decode(); err == nil && v != nil {
				looped = true
			}
		} else if strings.HasPrefix(key, "with_") {
			if looped {
				e := errAt(file, node, "duplicate loop in task: %s", strings.TrimPrefix(key, "with_")).(*parseError)
				e.notParser = true
				return nil, e
			}
			looped = true
		}
	}

	// Find the module key: exactly one non-keyword key. with_<lookup> keys
	// are loop forms, not modules.
	var moduleKeys []string
	for _, key := range keys {
		if !taskKeywords[key] && !strings.HasPrefix(key, "with_") {
			moduleKeys = append(moduleKeys, key)
		}
	}
	// action:/local_action: spell the module as a value instead of a key.
	for _, key := range []string{"action", "local_action"} {
		if val := node.MapGet(key); val != nil {
			if len(moduleKeys) > 0 {
				return nil, errAt(file, val, "conflicting action statements: %s, %s", key, moduleKeys[0])
			}
			if err := parseActionValue(task, val, file); err != nil {
				return nil, err
			}
			if key == "local_action" {
				task.Delegate = "localhost"
			}
			moduleKeys = []string{""}
		}
	}
	// ModuleArgsParser's wording: every key that is not a task keyword
	// competes for the action.
	if len(moduleKeys) == 1 && moduleKeys[0] == "" {
		// Module and args already set from action:/local_action:.
	} else if len(moduleKeys) == 0 {
		return nil, errAt(file, node, "no module/action detected in task.")
	}
	if len(moduleKeys) > 1 {
		return nil, errAt(file, node, "conflicting action statements: %s, %s", moduleKeys[0], moduleKeys[1])
	}
	if moduleKeys[0] != "" {
		moduleName := normalizeModuleName(moduleKeys[0])
		if !ModuleKnown(moduleName) && !executorStatement(moduleName) && moduleName != "meta" {
			return nil, errAt(file, node, "couldn't resolve module/action '%s'. This often indicates a "+
				"misspelling, missing collection, or incorrect module path.", moduleKeys[0])
		}
		task.Module = moduleName
		task.Action = moduleKeys[0]
		task.ActionPos = keyPos(node, moduleKeys[0], file)

		// Module args: map form, k=v string form, or null.
		argsNode := node.MapGet(moduleKeys[0])
		if err := parseModuleArgs(task, argsNode, file); err != nil {
			return nil, err
		}
	}
	if task.IsDynamicInclude() {
		if err := checkIncludeKeywords(task, node, keys, moduleKeys[0], file, handler); err != nil {
			return nil, err
		}
	}

	for _, key := range keys {
		val := node.MapGet(key)
		if (taskKeywords[key] || strings.HasPrefix(key, "with_")) && val != nil {
			if task.KeywordPos == nil {
				task.KeywordPos = map[string]Pos{}
			}
			task.KeywordPos[key] = Pos{File: file, Line: val.Line, Col: val.Column}
		}
		switch key {
		case "name":
			task.Name, _ = val.Str()
		case "args":
			raw, err := val.Decode()
			if err != nil {
				return nil, err
			}
			const argsHelp = "A mapping or template which resolves to a mapping is required."
			if s, isStr := raw.(string); isStr && isAllTemplate(s) {
				// A template resolving to the args (_variable_params).
				task.VarArgs, task.VarArgsPos = s, Pos{File: file, Line: val.Line, Col: val.Column}
				break
			}
			if raw == nil {
				task.LoadDeprecations = append(task.LoadDeprecations, LoadDeprecation{Msg: "Ignoring empty task `args` keyword.",
					Help: argsHelp, Version: "2.23", Pos: Pos{File: file, Line: node.Line, Col: node.Column}})
				break
			}
			m, ok := yaml.PlainMap(raw)
			if !ok {
				return nil, &parseError{file: file, line: val.Line, col: val.Column,
					msg: "The value of the task `args` keyword is invalid.", help: argsHelp}
			}
			if task.Args == nil {
				task.Args = map[string]any{}
			}
			for k, v := range m {
				task.Args[k] = v
			}
		case "when":
			task.When, task.WhenPos = decodeExprList(val), exprPositions(val, file)
		case "loop", "with_list":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			task.Loop = v
		case "with_items":
			// with_items flattens one level (the items lookup).
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			task.Loop = v
			task.LoopWith = "items"
		case "with_dict":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			task.Loop = v
			task.LoopWith = "dict"
		case "loop_control":
			m, err := decodeMap(val, file, "loop_control")
			if err != nil {
				return nil, err
			}
			if lv, ok := m["loop_var"].(string); ok && lv != "" {
				task.LoopVar = lv
			}
			if iv, ok := m["index_var"].(string); ok && iv != "" {
				task.IndexVar = iv
			}
			if label, ok := m["label"]; ok {
				task.LoopLabel = label
			}
			if pause, ok := m["pause"]; ok {
				task.LoopPause = pause
			}
			if bw := val.MapGet("break_when"); bw != nil && !bw.IsNull() {
				task.BreakWhen, task.BreakWhenPos = decodeExprList(bw), exprPositions(bw, file)
			}
			// loop_control's own fields are origins of their errors
			// and warnings.
			for _, k := range []string{"loop_var", "index_var", "pause"} {
				if n := val.MapGet(k); n != nil {
					task.KeywordPos["loop_control."+k] = Pos{File: file, Line: n.Line, Col: n.Column}
				}
			}
			if v, ok := m["extended"]; ok {
				task.LoopExtended = v
			}
			if v, ok := m["extended_allitems"]; ok {
				task.LoopAllItems = v
			}
		case "register":
			task.Register, _ = val.Str()
			if task.Register != "" && !strings.Contains(task.Register, "{{") && !ValidVariableName(task.Register) {
				msg, help := InvalidVariableName(task.Register)
				return nil, &parseError{file: file, line: val.Line, col: val.Column, msg: "Invalid 'register' specified.",
					cause: &parseError{file: file, line: val.Line, col: val.Column, msg: msg, help: help}}
			}
		case "ignore_errors":
			b, _, err := decodeBoolKW(task, val, file, "ignore_errors")
			if err != nil {
				return nil, err
			}
			task.IgnoreErrors = b
		case "failed_when":
			task.FailedWhen = decodeExprList(val)
		case "changed_when":
			task.ChangedWhen = decodeExprList(val)
		case "until":
			task.Until, _ = val.Str()
			if task.Delay == 0 {
				task.Delay = 5
			}
		case "retries":
			n, ok, err := decodeIntKW(task, val, file, "retries")
			if err != nil {
				return nil, err
			}
			if ok {
				task.Retries = int(n)
			} else {
				task.Retries = 3 // placeholder until resolved per host
			}
			task.RetriesSet = true
		case "delay":
			n, ok, err := decodeIntKW(task, val, file, "delay")
			if err != nil {
				return nil, err
			}
			if ok {
				task.Delay = int(n)
			} else {
				task.Delay = 5
			}
		case "become":
			b, ok, err := decodeBoolKW(task, val, file, "become")
			if err != nil {
				return nil, err
			}
			if ok {
				task.Become.Become = &b
			}
		case "become_user", "become_method", "become_flags", "become_exe":
			if err := parseBecomeKey(&task.Become, key, val, file); err != nil {
				return nil, err
			}
		case "debugger":
			d, err := parseDebugger(val, file)
			if err != nil {
				return nil, err
			}
			task.Debugger = d
		case "vars":
			m, err := decodeMap(val, file, "vars")
			if err != nil {
				return nil, err
			}
			if err := checkVarNames(val, file); err != nil {
				return nil, err
			}
			task.Vars = m
			task.VarOrigins = taskVarOrigins(val, file)
		case "environment":
			v, err := decodeEnvironment(val)
			if err != nil {
				return nil, err
			}
			task.Environment = v
		case "timeout":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			task.Timeout = v
		case "notify":
			task.Notify = decodeStringList(val)
		case "listen":
			if !handler {
				return nil, errAt(file, val, "'listen' is only valid on handlers")
			}
			task.Listen = decodeStringList(val)
		case "tags":
			task.Tags = decodeStringList(val)
		case "no_log":
			b, _, err := decodeBoolKW(task, val, file, "no_log")
			if err != nil {
				return nil, err
			}
			task.NoLog = b
		case "delegate_to":
			task.Delegate, _ = val.Str()
		case "check_mode":
			b, ok, err := decodeBoolKW(task, val, file, "check_mode")
			if err != nil {
				return nil, err
			}
			if ok {
				task.CheckMode = &b
			}
		case "run_once":
			b, err := decodeBool(val, file, "run_once")
			if err != nil {
				return nil, err
			}
			task.RunOnce = b
		case "delegate_facts":
			b, err := decodeBool(val, file, "delegate_facts")
			if err != nil {
				return nil, err
			}
			task.DelegateFacts = b
		case "any_errors_fatal":
			b, err := decodeBool(val, file, "any_errors_fatal")
			if err != nil {
				return nil, err
			}
			task.AnyErrorsFatal = &b
		case "remote_user":
			task.RemoteUser, _ = val.Str()
		case "connection":
			task.Connection, _ = val.Str()
		case "diff":
			b, ok, err := decodeBoolKW(task, val, file, "diff")
			if err != nil {
				return nil, err
			}
			if ok {
				task.Diff = &b
			}
		case "throttle", "ignore_unreachable", "collections",
			"module_defaults", "port":
			// Accepted: no effect on execution outcome here.
		case "async":
			// Async with poll > 0 runs synchronously (same outcome; the
			// timeout is not enforced yet). poll: 0 = fire-and-forget.
			n, err := decodeInt(val, file, "async")
			if err != nil {
				return nil, err
			}
			task.Async = int(n)
		case "poll":
			n, err := decodeInt(val, file, "poll")
			if err != nil {
				return nil, err
			}
			task.Poll = int(n)
		default:
			if strings.HasPrefix(key, "with_") {
				// with_<lookup>: terms feed the named lookup plugin.
				v, err := val.Decode()
				if err != nil {
					return nil, err
				}
				task.Loop = v
				task.LoopWith = strings.TrimPrefix(key, "with_")
			}
		}
	}
	task.LoadNotes = loadNotes(task)
	if task.IsDynamicInclude() {
		if err := parseIncludeApply(task, node, moduleKeys[0], file); err != nil {
			return nil, err
		}
	}
	return task, nil
}

// includeKeywords are TaskInclude.VALID_INCLUDE_KEYWORDS: the only task
// keywords include_tasks and include_role accept (a handler's also takes
// listen). The rest would apply to the include itself, not its tasks, so
// ansible-core rejects them; apply: passes keywords down instead.
var includeKeywords = map[string]bool{
	"action": true, "args": true, "collections": true, "debugger": true, "ignore_errors": true,
	"loop": true, "loop_control": true, "name": true, "no_log": true, "register": true,
	"run_once": true, "tags": true, "timeout": true, "vars": true, "when": true,
}

// checkIncludeKeywords is TaskInclude.preprocess_data's check: a keyword
// an include does not accept is an error.
func checkIncludeKeywords(task *Task, node *yaml.Node, keys []string, moduleKey, file string, handler bool) error {
	class := "TaskInclude"
	switch {
	case task.Module == "include_role":
		class = "IncludeRole"
	case handler:
		class = "HandlerTaskInclude"
	}
	for _, key := range keys {
		if key == moduleKey || includeKeywords[key] || strings.HasPrefix(key, "with_") || (handler && key == "listen") {
			continue
		}
		if key == "local_action" {
			key = "delegate_to" // local_action is delegate_to: localhost
		}
		return errAt(file, node, "'%s' is not a valid attribute for a %s", key, class)
	}
	return nil
}

// parseIncludeApply parses an include's apply: argument, the block
// keywords its included tasks inherit (TaskInclude.build_parent_block).
func parseIncludeApply(task *Task, node *yaml.Node, moduleKey, file string) error {
	var applyNode *yaml.Node
	for _, argsNode := range []*yaml.Node{node.MapGet(moduleKey), node.MapGet("args")} {
		if argsNode != nil && argsNode.Kind == yaml.MappingNode {
			if n := argsNode.MapGet("apply"); n != nil {
				applyNode = n
			}
		}
	}
	if _, ok := task.Args["apply"]; !ok {
		return nil
	}
	delete(task.Args, "apply")
	if applyNode == nil {
		return nil
	}
	if applyNode.Kind != yaml.MappingNode {
		v, _ := applyNode.Decode()
		return errAt(file, node, "Expected a dict for apply but got %s instead", pyTaggedType(v))
	}
	for _, key := range applyNode.MapKeys() {
		if !blockKeywords[key] {
			// The apply block loads when the include runs: the error
			// ends the run then.
			task.ApplyErr = errAtKey(file, applyNode, key, "'%s' is not a valid attribute for a Block", key)
			return nil
		}
	}
	inh, err := parseInheritable(applyNode, file)
	if err != nil {
		return err
	}
	task.Apply = inh
	return nil
}

// parseActionValue handles action:/local_action: — "module k=v ..." or a
// mapping with a module: key plus args.
func parseActionValue(task *Task, node *yaml.Node, file string) error {
	v, err := node.Decode()
	if err != nil {
		return err
	}
	if m, ok := yaml.PlainMap(v); ok {
		mod, _ := m["module"].(string)
		if mod == "" {
			return errAt(file, node, "action: mapping form requires a module key")
		}
		task.LoadDeprecations = append(task.LoadDeprecations, LoadDeprecation{Msg: "Using a mapping for `action` is deprecated.",
			Help: "Use a string value for `action`.", Version: "2.23", Pos: Pos{File: file, Line: node.Line, Col: node.Column}})
		// The action comes from the module: value ("module: copy src=a"
		// may carry k=v args too).
		modNode := node.MapGet("module")
		task.ActionPos = Pos{File: file, Line: modNode.Line, Col: modNode.Column}
		fields := strings.SplitN(strings.TrimSpace(mod), " ", 2)
		task.Module = normalizeModuleName(fields[0])
		task.Action = fields[0]
		args := map[string]any{}
		task.ArgPos = map[string]Pos{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.Value != "module" && key.Value != "args" {
				args[key.Value] = m[key.Value]
				task.ArgPos[key.Value] = Pos{File: file, Line: val.Line, Col: val.Column}
			}
		}
		if len(fields) == 2 {
			kv, raw := parseKVRaw(fields[1])
			for k, val := range kv {
				args[k] = val
				task.ArgPos[k] = task.ActionPos
			}
			task.RawArgs, task.ArgsPos = raw, task.ActionPos
		}
		// args: in the mapping merges in (a string as k=v, its free text
		// the raw params).
		if argsNode := node.MapGet("args"); argsNode != nil {
			argsPos := Pos{File: file, Line: argsNode.Line, Col: argsNode.Column}
			switch a := m["args"].(type) {
			case string:
				kv, raw := parseKVRaw(a)
				for k, val := range kv {
					args[k] = val
					task.ArgPos[k] = argsPos
				}
				if raw != "" {
					task.RawArgs, task.ArgsPos = raw, argsPos
				}
			default:
				if am, ok := yaml.PlainMap(a); ok {
					for k, val := range am {
						args[k] = val
					}
					for i := 0; argsNode.Kind == yaml.MappingNode && i+1 < len(argsNode.Content); i += 2 {
						key, val := argsNode.Content[i], argsNode.Content[i+1]
						task.ArgPos[key.Value] = Pos{File: file, Line: val.Line, Col: val.Column}
					}
				}
			}
		}
		if len(args) > 0 {
			task.Args = args
		}
	} else {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return errAt(file, node, "action: must name a module")
		}
		// The action and its k=v args come from the value.
		task.ActionPos = Pos{File: file, Line: node.Line, Col: node.Column}
		task.ArgsPos = task.ActionPos
		fields := strings.SplitN(strings.TrimSpace(s), " ", 2)
		rest := ""
		if len(fields) == 2 {
			rest = fields[1]
		}
		if err := ParseAdhocArgs(task, fields[0], rest); err != nil {
			return errAt(file, node, "%v", err)
		}
	}
	if !ModuleKnown(task.Module) && !executorStatement(task.Module) && task.Module != "meta" {
		return errAt(file, node, "couldn't resolve module/action '%s'. This often indicates a "+
			"misspelling, missing collection, or incorrect module path.", task.Module)
	}
	return nil
}

// parseModuleArgs handles the three arg spellings:
//
//	module: {k: v, ...}      map form
//	module: k=v k2=v2        string form (mini-shlex)
//	module: some free text   free-form (command/shell/raw only)
//	module:                  null (no args)
func parseModuleArgs(task *Task, node *yaml.Node, file string) error {
	v, err := node.Decode()
	if err != nil {
		return err
	}
	if node != nil {
		task.ArgsPos = Pos{File: file, Line: node.Line, Col: node.Column}
	}
	if m, ok := yaml.PlainMap(v); ok {
		task.Args = m
		if node.Kind == yaml.MappingNode {
			task.ArgPos = map[string]Pos{}
			task.ArgKeyPos = map[string]Pos{}
			for i := 0; i+1 < len(node.Content); i += 2 {
				key, val := node.Content[i], node.Content[i+1]
				task.ArgPos[key.Value] = Pos{File: file, Line: val.Line, Col: val.Column}
				task.ArgKeyPos[key.Value] = Pos{File: file, Line: key.Line, Col: key.Column}
				if val.Kind == yaml.MappingNode {
					if task.ArgSubKeys == nil {
						task.ArgSubKeys = map[string][]ArgKey{}
					}
					for j := 0; j+1 < len(val.Content); j += 2 {
						sub := val.Content[j]
						kv, err := sub.Decode()
						if err != nil {
							kv = sub.Value
						}
						task.ArgSubKeys[key.Value] = append(task.ArgSubKeys[key.Value], ArgKey{
							Name: yaml.KeyName(sub), Value: kv, Pos: Pos{File: file, Line: sub.Line, Col: sub.Column}})
					}
				}
			}
		}
		return nil
	}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if executorStatement(task.Module) {
			// include_role: rolename normalizes to name:, everything else
			// (include_tasks/include_vars) to file:.
			key := "file"
			if task.Module == "include_role" || task.Module == "import_role" {
				key = "name"
			}
			task.Args = map[string]any{key: t}
			return nil
		}
		if freeFormModule(task.Module) {
			free, kv := splitFreeForm(t, task.Module)
			task.FreeForm = free
			if task.Args == nil && len(kv) > 0 {
				task.Args = kv
			}
			return nil
		}
		kv, raw := parseKVRaw(t)
		task.Args = kv
		if rawParamModule(task.Module) {
			// These take raw params: an argument like any other.
			if raw != "" {
				kv["_raw_params"] = raw
			}
		} else {
			task.RawArgs = raw
		}
		return nil
	default:
		return errAt(file, node, "module arguments must be a mapping or string, got %T", v)
	}
}

// ParseAdhocArgs applies the module-args parsing rules to an ad-hoc -a
// value (k=v pairs, or free-form for command/shell/raw).
func ParseAdhocArgs(task *Task, module, raw string) error {
	task.Module = normalizeModuleName(module)
	task.Action = module
	if freeFormModule(task.Module) {
		free, kv := splitFreeForm(raw, task.Module)
		task.FreeForm = free
		if len(kv) > 0 {
			task.Args = kv
		}
		return nil
	}
	kv, rawParams := parseKVRaw(raw)
	task.Args = kv
	if rawParamModule(task.Module) {
		if rawParams != "" {
			kv["_raw_params"] = rawParams
		}
	} else {
		task.RawArgs = rawParams
	}
	return nil
}

// rawParamModule names the actions that take raw params beyond the
// free-form modules (RAW_PARAM_MODULES): their free text is an argument.
func rawParamModule(name string) bool {
	switch name {
	case "set_fact", "add_host", "group_by":
		return true
	}
	return false
}

// executorStatement names task keys handled by the executor itself rather
// than a module or action (dynamic includes).
func executorStatement(name string) bool {
	switch name {
	case "include_tasks", "include_vars", "include_role", "import_role":
		return true
	}
	return false
}

func normalizeModuleName(name string) string {
	// Fully-qualified collection names collapse to the short name.
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func freeFormModule(name string) bool {
	switch name {
	case "command", "shell", "raw", "script", "meta":
		return true
	}
	return false
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// decodeEnvironment decodes the environment keyword: a mapping, a
// template, or a list of them. Each is an entry that merges, in order, into
// the environment at run time (ansible-core's list-valued environment,
// extended by prepending the enclosing blocks', role's and play's).
func decodeEnvironment(node *yaml.Node) ([]any, error) {
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return t, nil
	}
	return []any{v}, nil
}

func decodeMap(node *yaml.Node, file, key string) (map[string]any, error) {
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return nil, errAt(file, node, "%q must be a mapping", key)
	}
	return m, nil
}

func decodeBool(node *yaml.Node, file, key string) (bool, error) {
	v, err := node.Decode()
	if err != nil {
		return false, err
	}
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		if b, ok := ParseBool(t); ok {
			return b, nil
		}
		if isTemplate(t) {
			return false, errAt(file, node, "%q cannot be templated here", key)
		}
	case int64:
		if b, ok := ParseBool(t); ok {
			return b, nil
		}
	}
	return false, errAt(file, node, "%q must be a boolean", key)
}

// ParseBool is Ansible's strict boolean(): the accepted true/false
// spellings of a keyword value.
func ParseBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case int64:
		if t == 0 || t == 1 {
			return t == 1, true
		}
	case int:
		if t == 0 || t == 1 {
			return t == 1, true
		}
	case float64:
		if t == 0 || t == 1 {
			return t == 1, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "y", "yes", "on", "1", "true", "t":
			return true, true
		case "n", "no", "off", "0", "false", "f":
			return false, true
		}
	}
	return false, false
}

func isTemplate(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%")
}

// decodeBoolKW decodes a keyword that may be a template: a template is
// stashed in task.KeywordTemplates for per-host resolution and ok=false.
func decodeBoolKW(task *Task, node *yaml.Node, file, key string) (b, ok bool, err error) {
	if s, isStr := node.Str(); isStr && isTemplate(s) {
		if v, _ := node.Decode(); v != nil {
			if _, str := v.(string); str {
				setKeywordTemplate(task, key, s)
				return false, false, nil
			}
		}
	}
	b, err = decodeBool(node, file, key)
	if err == nil {
		task.markLiteral(key)
	}
	return b, err == nil, err
}

// decodeIntKW is decodeBoolKW for integer keywords (retries, delay).
func decodeIntKW(task *Task, node *yaml.Node, file, key string) (n int64, ok bool, err error) {
	v, err := node.Decode()
	if err != nil {
		return 0, false, err
	}
	if s, isStr := v.(string); isStr {
		if isTemplate(s) {
			setKeywordTemplate(task, key, s)
			return 0, false, nil
		}
		if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			task.markLiteral(key)
			return i, true, nil
		}
	}
	n, err = decodeInt(node, file, key)
	if err == nil {
		task.markLiteral(key)
	}
	return n, err == nil, err
}

func (t *Task) markLiteral(key string) {
	if t.KeywordTemplates != nil {
		delete(t.KeywordTemplates, key)
	}
	if t.literalKW == nil {
		t.literalKW = map[string]bool{}
	}
	t.literalKW[key] = true
}

func (t *Task) hasLiteral(key string) bool { return t.literalKW[key] }

func setKeywordTemplate(task *Task, key, s string) {
	if task.KeywordTemplates == nil {
		task.KeywordTemplates = map[string]string{}
	}
	task.KeywordTemplates[key] = s
}

func decodeInt(node *yaml.Node, file, key string) (int64, error) {
	v, err := node.Decode()
	if err != nil {
		return 0, err
	}
	if n, ok := v.(int64); ok {
		return n, nil
	}
	return 0, errAt(file, node, "%q must be an integer", key)
}

// decodeExprList normalizes when:/failed_when:/changed_when: to a string
// list (a bare string becomes a one-element list; a list stays; booleans
// become "True"/"False").
func decodeExprList(node *yaml.Node) []string {
	if v, err := node.Decode(); err == nil && v == nil {
		// when: with no value: no conditions.
		return nil
	}
	if items, ok := node.Seq(); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, exprString(it))
		}
		return out
	}
	return []string{exprString(node)}
}

// exprPositions maps each condition of a when: value (one expression or
// a list of them) to where it was written.
func exprPositions(node *yaml.Node, file string) map[string]Pos {
	items, ok := node.Seq()
	if !ok {
		items = []*yaml.Node{node}
	}
	out := make(map[string]Pos, len(items))
	for _, it := range items {
		if _, seen := out[exprString(it)]; !seen {
			out[exprString(it)] = Pos{File: file, Line: it.Line, Col: it.Column}
		}
	}
	return out
}

func exprString(node *yaml.Node) string {
	v, err := node.Decode()
	if err != nil {
		s, _ := node.Str()
		return s
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case nil:
		return ""
	default:
		// Not a string: a broken conditional when evaluated.
		return template.NonStringConditional(t)
	}
}

func decodeStringList(node *yaml.Node) []string {
	if items, ok := node.Seq(); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			if s, ok := it.Str(); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := node.Str(); ok && s != "" {
		// Comma-separated shorthand: tags: a,b
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// parseVarsPrompt reads vars_prompt: a list of {name, prompt, default,
// private, confirm, encrypt, salt, salt_size, unsafe}.
func parseVarsPrompt(node *yaml.Node, file string) ([]VarPrompt, error) {
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errAt(file, node, "vars_prompt must be a list")
	}
	var out []VarPrompt
	for _, item := range list {
		m, ok := yaml.PlainMap(item)
		if !ok {
			return nil, errAt(file, node, "vars_prompt entries must be mappings")
		}
		name, _ := m["name"].(string)
		if name == "" {
			return nil, errAt(file, node, "vars_prompt entry requires a name")
		}
		vp := VarPrompt{Name: name, Private: true, Default: m["default"]}
		vp.Prompt, _ = m["prompt"].(string)
		if vp.Prompt == "" {
			vp.Prompt = name
		}
		if b, ok := m["private"].(bool); ok {
			vp.Private = b
		}
		vp.Confirm, _ = m["confirm"].(bool)
		vp.Encrypt, _ = m["encrypt"].(string)
		vp.Salt, _ = m["salt"].(string)
		if n, ok := m["salt_size"].(int64); ok {
			vp.SaltSize = int(n)
		}
		vp.Unsafe, _ = m["unsafe"].(bool)
		out = append(out, vp)
	}
	return out, nil
}

func truthyVar(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(t) {
		case "yes", "true", "on", "1":
			return true
		}
	}
	return false
}

// BecomeMethods are the supported become plugins (by normalized name).
var BecomeMethods = []string{"sudo", "su", "doas"}

// NormalizeBecomeMethod maps a become_method value to a supported plugin
// name ("community.general.doas" is doas), or "" when unsupported.
func NormalizeBecomeMethod(s string) string {
	switch s {
	case "sudo", "ansible.builtin.sudo", "ansible.legacy.sudo":
		return "sudo"
	case "su", "ansible.builtin.su", "ansible.legacy.su":
		return "su"
	case "doas", "community.general.doas":
		return "doas"
	}
	return ""
}

// parseBecomeKey decodes one become keyword into bf.
func parseBecomeKey(bf *BecomeFields, key string, val *yaml.Node, file string) error {
	switch key {
	case "become":
		b, err := decodeBool(val, file, "become")
		if err != nil {
			return err
		}
		bf.Become = &b
	case "become_user":
		bf.BecomeUser, _ = val.Str()
	case "become_method":
		s, _ := val.Str()
		if s == "" {
			return nil
		}
		if strings.Contains(s, "{{") {
			bf.Method = s // resolved per host at run time
			return nil
		}
		m := NormalizeBecomeMethod(s)
		if m == "" {
			return errAt(file, val, "become_method %q is not supported (supported: sudo, su, doas)", s)
		}
		bf.Method = m
	case "become_flags":
		s, _ := val.Str()
		bf.Flags = &s
	case "become_exe":
		bf.Exe, _ = val.Str()
	}
	return nil
}

// parseDebugger validates the debugger keyword.
func parseDebugger(val *yaml.Node, file string) (string, error) {
	s, _ := val.Str()
	switch s {
	case "", "always", "never", "on_failed", "on_unreachable", "on_skipped":
		return s, nil
	}
	return "", errAt(file, val, "debugger must be one of always, never, on_failed, on_unreachable or on_skipped, got %q", s)
}

// reservedKeyOrigins lists the keys of a vars mapping that use a reserved
// name, where each was written.
func reservedKeyOrigins(node *yaml.Node, file string) []template.KeyOrigin {
	if node == nil {
		return nil
	}
	var out []template.KeyOrigin
	for _, k := range node.MapKeys() {
		if !template.IsReservedName(k) {
			continue
		}
		o := template.KeyOrigin{Name: k}
		if kn := node.MapKeyNode(k); kn != nil {
			o.File, o.Line, o.Col = file, kn.Line, kn.Column
		}
		out = append(out, o)
	}
	return out
}

// ReservedKeyOrigins is reservedKeyOrigins for a vars file's document.
func ReservedKeyOrigins(node *yaml.Node, file string) []template.KeyOrigin {
	return reservedKeyOrigins(node, file)
}

// taskVarOrigins is reservedKeyOrigins for a task's or block's vars:
// Task.get_vars leaves their tags and when out.
func taskVarOrigins(node *yaml.Node, file string) []template.KeyOrigin {
	var out []template.KeyOrigin
	for _, o := range reservedKeyOrigins(node, file) {
		if o.Name != "tags" && o.Name != "when" {
			out = append(out, o)
		}
	}
	return out
}

// variableNameHelp is validate_variable_name's help text.
const variableNameHelp = "Variable names must be strings starting with a letter or underscore character, and contain only letters, numbers and underscores."

// ValidVariableName is validate_variable_name's test: an ASCII Python
// identifier that is not a Jinja keyword.
func ValidVariableName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	switch name {
	case "False", "None", "True", "false", "none", "not", "true":
		return false
	}
	return true
}

// InvalidVariableName is validate_variable_name's message for name.
func InvalidVariableName(name string) (msg, help string) {
	return fmt.Sprintf("Invalid variable name %s.", pyQuoteName(name)), variableNameHelp
}

// pyQuoteName is repr() of a str key.
func pyQuoteName(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// checkVarNames is Base._load_vars: every vars: key must be a valid
// variable name (an AnsibleError: exit 1).
func checkVarNames(node *yaml.Node, file string) error {
	if node == nil {
		return nil
	}
	for _, k := range node.MapKeys() {
		if ValidVariableName(k) {
			continue
		}
		msg, help := InvalidVariableName(k)
		e := &parseError{file: file, msg: msg, help: help, notParser: true}
		if kn := node.MapKeyNode(k); kn != nil {
			e.line, e.col = kn.Line, kn.Column
		}
		return e
	}
	return nil
}
