package playbook

import (
	"fmt"
	"os"
	"strings"

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
}

// Deferred play keys that must fail loudly rather than be ignored.
var unsupportedPlayKeys = map[string]string{
	"roles":       "roles are not supported yet",
	"serial":      "'serial' is not supported yet",
	"strategy":    "only the linear strategy is supported",
	"vars_prompt": "'vars_prompt' is not supported yet",
}

type parseError struct {
	file string
	line int
	msg  string
}

func (e *parseError) Error() string {
	return fmt.Sprintf("%s:%d: %s", e.file, e.line, e.msg)
}

func errAt(file string, node *yaml.Node, format string, args ...any) error {
	line := 0
	if node != nil {
		line = node.Line
	}
	return &parseError{file: file, line: line, msg: fmt.Sprintf(format, args...)}
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
	f, err := yaml.Parse(data, filename)
	if err != nil {
		return nil, err
	}
	var plays []*Play
	for _, doc := range f.Docs {
		items, ok := doc.Seq()
		if !ok {
			if s, isScalar := doc.Str(); isScalar && s == "" {
				continue // empty document
			}
			return nil, errAt(filename, doc, "a playbook must be a list of plays")
		}
		for _, item := range items {
			play, err := parsePlay(item, filename)
			if err != nil {
				return nil, err
			}
			plays = append(plays, play)
		}
	}
	return plays, nil
}

func parsePlay(node *yaml.Node, file string) (*Play, error) {
	keys := node.MapKeys()
	if keys == nil {
		return nil, errAt(file, node, "a play must be a mapping")
	}
	play := &Play{Src: Pos{File: file, Line: node.Line, Col: node.Column}}
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
			return nil, errAt(file, val, "unknown play keyword %q", key)
		}
		switch key {
		case "name":
			play.Name, _ = val.Str()
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
			play.Vars = v
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
		case "become":
			b, err := decodeBool(val, file, "become")
			if err != nil {
				return nil, err
			}
			play.Become.Become = &b
		case "become_user":
			play.Become.BecomeUser, _ = val.Str()
		case "become_method":
			if s, _ := val.Str(); s != "" && s != "sudo" {
				return nil, errAt(file, val, "become_method %q is not supported (only sudo)", s)
			}
		case "tasks", "pre_tasks", "post_tasks", "handlers":
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
		case "environment":
			v, err := decodeMap(val, file, "environment")
			if err != nil {
				return nil, err
			}
			play.Environment = v
		case "remote_user", "connection", "max_fail_percentage",
			"any_errors_fatal", "force_handlers":
			// Parsed but handled elsewhere (or benignly ignored in v0.1).
		}
	}
	return play, nil
}

func parseTaskList(node *yaml.Node, file string, handlers bool) ([]*Task, error) {
	if s, ok := node.Str(); ok && s == "" {
		return nil, nil // tasks: (empty)
	}
	items, ok := node.Seq()
	if !ok {
		return nil, errAt(file, node, "expected a list of tasks")
	}
	var tasks []*Task
	for _, item := range items {
		task, err := parseTask(item, file, handlers)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func parseTask(node *yaml.Node, file string, handler bool) (*Task, error) {
	keys := node.MapKeys()
	if keys == nil {
		return nil, errAt(file, node, "a task must be a mapping")
	}
	task := &Task{
		LoopVar: "item",
		Src:     Pos{File: file, Line: node.Line, Col: node.Column},
	}

	// Find the module key: exactly one non-keyword key.
	var moduleKeys []string
	for _, key := range keys {
		if !taskKeywords[key] {
			moduleKeys = append(moduleKeys, key)
		}
	}
	if len(moduleKeys) == 0 {
		return nil, errAt(file, node, "no module found in task (keys: %s)", strings.Join(keys, ", "))
	}
	if len(moduleKeys) > 1 {
		return nil, errAt(file, node,
			"multiple module-like keys in one task: %s (only one module per task)",
			strings.Join(moduleKeys, ", "))
	}
	moduleName := normalizeModuleName(moduleKeys[0])
	if !ModuleKnown(moduleName) {
		return nil, errAt(file, node.MapGet(moduleKeys[0]),
			"couldn't resolve module/action %q", moduleKeys[0])
	}
	task.Module = moduleName

	// Module args: map form, k=v string form, or null.
	argsNode := node.MapGet(moduleKeys[0])
	if err := parseModuleArgs(task, argsNode, file); err != nil {
		return nil, err
	}

	for _, key := range keys {
		val := node.MapGet(key)
		switch key {
		case "name":
			task.Name, _ = val.Str()
		case "args":
			m, err := decodeMap(val, file, "args")
			if err != nil {
				return nil, err
			}
			if task.Args == nil {
				task.Args = map[string]any{}
			}
			for k, v := range m {
				task.Args[k] = v
			}
		case "when":
			task.When = decodeExprList(val)
		case "loop", "with_items":
			v, err := val.Decode()
			if err != nil {
				return nil, err
			}
			task.Loop = v
		case "loop_control":
			m, err := decodeMap(val, file, "loop_control")
			if err != nil {
				return nil, err
			}
			if lv, ok := m["loop_var"].(string); ok && lv != "" {
				task.LoopVar = lv
			}
		case "register":
			task.Register, _ = val.Str()
		case "ignore_errors":
			b, err := decodeBool(val, file, "ignore_errors")
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
			if task.Retries == 0 {
				task.Retries = 3
			}
			if task.Delay == 0 {
				task.Delay = 5
			}
		case "retries":
			n, err := decodeInt(val, file, "retries")
			if err != nil {
				return nil, err
			}
			task.Retries = int(n)
		case "delay":
			n, err := decodeInt(val, file, "delay")
			if err != nil {
				return nil, err
			}
			task.Delay = int(n)
		case "become":
			b, err := decodeBool(val, file, "become")
			if err != nil {
				return nil, err
			}
			task.Become.Become = &b
		case "become_user":
			task.Become.BecomeUser, _ = val.Str()
		case "become_method":
			if s, _ := val.Str(); s != "" && s != "sudo" {
				return nil, errAt(file, val, "become_method %q is not supported (only sudo)", s)
			}
		case "vars":
			m, err := decodeMap(val, file, "vars")
			if err != nil {
				return nil, err
			}
			task.Vars = m
		case "environment":
			m, err := decodeMap(val, file, "environment")
			if err != nil {
				return nil, err
			}
			task.Environment = m
		case "notify":
			task.Notify = decodeStringList(val)
		case "listen":
			if !handler {
				return nil, errAt(file, val, "'listen' is only valid on handlers")
			}
			task.Notify = nil // listen topics resolved at flush time (M5)
		case "tags":
			task.Tags = decodeStringList(val)
		case "no_log":
			b, err := decodeBool(val, file, "no_log")
			if err != nil {
				return nil, err
			}
			task.NoLog = b
		case "delegate_to":
			task.Delegate, _ = val.Str()
		case "check_mode", "diff", "run_once", "any_errors_fatal":
			// Accepted; wired in later milestones.
		}
	}
	return task, nil
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
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		task.Args = t
		return nil
	case string:
		if freeFormModule(task.Module) {
			free, kv := splitFreeForm(t, task.Module)
			task.FreeForm = free
			if task.Args == nil && len(kv) > 0 {
				task.Args = kv
			}
			return nil
		}
		kv, err := parseKV(t)
		if err != nil {
			return errAt(file, node, "cannot parse %q as module arguments: %v", t, err)
		}
		task.Args = kv
		return nil
	default:
		return errAt(file, node, "module arguments must be a mapping or string, got %T", v)
	}
}

// ParseAdhocArgs applies the module-args parsing rules to an ad-hoc -a
// value (k=v pairs, or free-form for command/shell/raw).
func ParseAdhocArgs(task *Task, module, raw string) error {
	task.Module = normalizeModuleName(module)
	if freeFormModule(task.Module) {
		free, kv := splitFreeForm(raw, task.Module)
		task.FreeForm = free
		if len(kv) > 0 {
			task.Args = kv
		}
		return nil
	}
	kv, err := parseKV(raw)
	if err != nil {
		return fmt.Errorf("cannot parse %q as module arguments: %w", raw, err)
	}
	task.Args = kv
	return nil
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

func decodeMap(node *yaml.Node, file, key string) (map[string]any, error) {
	v, err := node.Decode()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
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
		// A template string like "{{ x }}" is resolved at run time; v0.1
		// requires literal booleans on these keywords.
		return false, errAt(file, node, "%q must be a literal boolean in this version", key)
	}
	return false, errAt(file, node, "%q must be a boolean", key)
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
	if items, ok := node.Seq(); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, exprString(it))
		}
		return out
	}
	return []string{exprString(node)}
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
		return fmt.Sprintf("%v", t)
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
