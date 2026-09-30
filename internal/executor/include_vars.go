package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// include_vars argument groups (ansible-core's action plugin).
var (
	includeVarsDirArgs  = map[string]bool{"dir": true, "depth": true, "files_matching": true, "ignore_files": true, "extensions": true, "ignore_unknown_extensions": true}
	includeVarsFileArgs = map[string]bool{"file": true, "_raw_params": true}
	includeVarsAllArgs  = map[string]bool{"name": true, "hash_behaviour": true}
)

// includeVarsRun is one include_vars invocation's state.
type includeVarsRun struct {
	r           *Runner
	task        *playbook.Task
	actx        *actions.Context
	included    []any
	showContent bool
	matcher     *regexp.Regexp
	ignore      []string
	extensions  []string
	ignoreExt   bool
}

// runIncludeVars is ansible-core's include_vars action: load a vars file
// (found through the vars/ search path) or a directory of them, optionally
// scoped under name:, into the host's include_vars precedence layer.
func (r *Runner) runIncludeVars(task *playbook.Task, actx *actions.Context, args map[string]any) *agentproto.Result {
	raised := func(format string, a ...any) *agentproto.Result {
		res := agentproto.Fail("Task failed: "+format, a...)
		res.Origin = "verbatim"
		return res
	}
	dirs, files := 0, 0
	for _, k := range sortedKeys(args) {
		switch {
		case includeVarsDirArgs[k]:
			dirs++
		case includeVarsFileArgs[k]:
			files++
		case includeVarsAllArgs[k]:
		default:
			return raised("%s is not a valid option in include_vars", k)
		}
	}
	if dirs > 0 && files > 0 {
		return raised("You are mixing file only and dir only arguments, these are incompatible")
	}

	iv := &includeVarsRun{r: r, task: task, actx: actx, showContent: true,
		extensions: []string{"yaml", "yml", "json"}}
	hashBehaviour, _ := args["hash_behaviour"].(string)
	name := argStr(args, "name")
	sourceDir := argStr(args, "dir")
	sourceFile := argStr(args, "file")
	if sourceDir == "" && sourceFile == "" {
		sourceFile = strings.TrimRight(argStr(args, "_raw_params"), "\n")
	}
	if v, ok := args["extensions"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return raised("The 'extensions' option must be a list.")
		}
		iv.extensions = nil
		for _, e := range list {
			iv.extensions = append(iv.extensions, template.PyStr(e))
		}
	}
	if b, ok := args["ignore_unknown_extensions"]; ok {
		iv.ignoreExt = truthy(b)
	}

	results := map[string]any{}
	failed := false
	errMsg := ""
	if sourceDir != "" {
		depth := 0
		if v, ok := args["depth"]; ok && v != nil {
			n, err := toInt(v)
			if err != nil {
				return raised("depth: %v", err)
			}
			depth = n
		}
		if fm := argStr(args, "files_matching"); fm != "" {
			re, err := template.PyRegexCompile(fm)
			if err != nil {
				return raised("%v", err)
			}
			iv.matcher = re
		}
		switch t := args["ignore_files"].(type) {
		case nil:
		case string:
			iv.ignore = strings.Fields(t)
		case []any:
			for _, e := range t {
				iv.ignore = append(iv.ignore, template.PyStr(e))
			}
		default:
			return raised("The 'ignore_files' option must be a list.")
		}
		sourceDir = iv.rootDir(sourceDir)
		info, err := os.Stat(sourceDir)
		switch {
		case err != nil:
			failed, errMsg = true, sourceDir+" directory does not exist"
		case !info.IsDir():
			failed, errMsg = true, sourceDir+" is not a directory"
		default:
			for _, d := range walkSorted(sourceDir, depth) {
				var loaded map[string]any
				failed, errMsg, loaded = iv.loadDir(d.root, d.files)
				if failed {
					break
				}
				for k, v := range loaded {
					results[k] = v
				}
			}
		}
	} else {
		found, searched := actions.SearchNeedle(actx, "vars", sourceFile)
		if found == "" {
			failed, errMsg = true, actions.FileNotFound(sourceFile, searched)
		} else {
			var loaded map[string]any
			failed, errMsg, loaded = iv.loadFile(found, false)
			if !failed {
				for k, v := range loaded {
					results[k] = v
				}
			}
		}
	}
	if name != "" {
		results = map[string]any{name: results}
	}

	res := &agentproto.Result{Extra: map[string]any{}}
	if failed {
		res.Failed = true
		res.Msg = "Task failed: Action failed: Unknown error."
		res.Origin = "verbatim"
		res.Extra["message"] = errMsg
	} else if hashBehaviour == "merge" {
		for k, v := range results {
			if old, ok := actx.Vars.Get(k); ok {
				results[k] = mergeHashes(old, v)
			}
		}
	}
	if iv.included == nil {
		iv.included = []any{}
	}
	res.Extra["ansible_included_var_files"] = iv.included
	res.Extra["ansible_facts"] = results
	if actx.SetIncludeVars != nil && len(results) > 0 {
		actx.SetIncludeVars(results)
	}
	if !iv.showContent {
		res.Extra["_ansible_no_log"] = true
	}
	return res
}

// rootDir anchors a relative dir: under the role's vars/ (a leading vars/
// is taken as role-relative), else next to the task's file.
func (iv *includeVarsRun) rootDir(dir string) string {
	if iv.actx.SrcDir != "" {
		if strings.Split(dir, "/")[0] == "vars" {
			p := pyJoin(iv.actx.SrcDir, dir)
			if _, err := os.Stat(p); err == nil {
				return p
			}
			return dir
		}
		return pyJoin(iv.actx.SrcDir, "vars", dir)
	}
	if iv.task.Src.File != "" {
		return pyJoin(filepath.Dir(iv.task.Src.File), dir)
	}
	return dir
}

func (iv *includeVarsRun) validExt(name string) bool {
	ext := filepath.Ext(name)
	if ext == "" {
		return false
	}
	for _, e := range iv.extensions {
		if ext[1:] == e {
			return true
		}
	}
	return false
}

func (iv *includeVarsRun) ignored(name string) (bool, error) {
	for _, pat := range iv.ignore {
		re, err := template.PyRegexCompile(pat + "$")
		if err != nil {
			return false, fmt.Errorf("Invalid regular expression: %s", template.PyRepr(pat))
		}
		if re.MatchString(name) {
			return true, nil
		}
	}
	return false, nil
}

// loadFile is _load_files: parse one vars file into a mapping.
func (iv *includeVarsRun) loadFile(path string, validateExt bool) (bool, string, map[string]any) {
	if validateExt && !iv.validExt(path) {
		return true, fmt.Sprintf("%s does not have a valid extension: %s",
			template.PyRepr(path), strings.Join(iv.extensions, ", ")), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, err.Error(), nil
	}
	if iv.r.Opts.Vault != nil {
		plain, err := iv.r.Opts.Vault.MaybeDecryptFile(data)
		if err != nil {
			return true, err.Error(), nil
		}
		if len(plain) != len(data) || string(plain) != string(data) {
			iv.showContent = false
		}
		data = plain
	}
	v, err := yaml.Unmarshal(data, path)
	if err != nil {
		var ye *yaml.Error
		if errors.As(err, &ye) {
			return true, ye.Message(), nil
		}
		return true, err.Error(), nil
	}
	if v == nil {
		v = map[string]any{}
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return true, template.PyRepr(path) + " must be stored as a dictionary/hash", nil
	}
	iv.included = append(iv.included, path)
	return false, "", m
}

// loadDir is _load_files_in_dir.
func (iv *includeVarsRun) loadDir(root string, names []string) (bool, string, map[string]any) {
	results := map[string]any{}
	for _, name := range names {
		if iv.actx.SrcDir != "" && pyJoin(iv.actx.SrcDir, name) == pyJoin(root, "vars", "main.yml") {
			continue
		}
		path := pyJoin(root, name)
		if iv.matcher != nil && !iv.matcher.MatchString(name) {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		skip, err := iv.ignored(name)
		if err != nil {
			return true, err.Error(), nil
		}
		if skip || (iv.ignoreExt && !iv.validExt(name)) {
			continue
		}
		failed, msg, loaded := iv.loadFile(path, true)
		if failed {
			return true, msg, nil
		}
		for k, v := range loaded {
			results[k] = v
		}
	}
	return false, "", results
}

type walkedDir struct {
	root  string
	files []string
}

// walkSorted is _traverse_dir_depth: os.walk (following symlinks) sorted
// by directory path, files sorted, limited to depth levels (0 = all).
func walkSorted(root string, depth int) []walkedDir {
	var out []walkedDir
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		wd := walkedDir{root: dir}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			isDir := e.IsDir()
			if e.Type()&fs.ModeSymlink != 0 {
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					isDir = true
				}
			}
			if isDir {
				walk(p, level+1)
			} else {
				wd.files = append(wd.files, e.Name())
			}
		}
		if depth == 0 || level <= depth {
			sort.Strings(wd.files)
			out = append(out, wd)
		}
	}
	walk(root, 1)
	sort.SliceStable(out, func(i, j int) bool { return out[i].root < out[j].root })
	return out
}

// mergeHashes is combine_vars(merge=True) for one variable.
func mergeHashes(old, cur any) any {
	om, ok1 := yaml.PlainMap(old)
	nm, ok2 := yaml.PlainMap(cur)
	if !ok1 || !ok2 {
		return cur
	}
	out := make(map[string]any, len(om)+len(nm))
	for k, v := range om {
		out[k] = v
	}
	for k, v := range nm {
		if prev, ok := out[k]; ok {
			out[k] = mergeHashes(prev, v)
		} else {
			out[k] = v
		}
	}
	return out
}

// pyJoin is os.path.join.
func pyJoin(parts ...string) string {
	out := ""
	for i, p := range parts {
		switch {
		case i == 0 || strings.HasPrefix(p, "/"):
			out = p
		case out == "" || strings.HasSuffix(out, "/"):
			out += p
		default:
			out += "/" + p
		}
	}
	return out
}

func argStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	return template.PyStr(v)
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(t) {
		case "yes", "on", "1", "true", "y", "t":
			return true
		}
	case int64:
		return t != 0
	case int:
		return t != 0
	}
	return false
}

func toInt(v any) (int, error) {
	switch t := v.(type) {
	case int64:
		return int(t), nil
	case int:
		return t, nil
	case float64:
		return int(t), nil
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cannot convert %v to an int", v)
}
