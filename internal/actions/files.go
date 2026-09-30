package actions

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
	"github.com/giraffesyo/understudy/internal/template"
)

func init() {
	Register("copy", actionFunc(runCopy))
	Register("template", actionFunc(runTemplate))
}

// actionFail is an action-plugin failure (ansible-core shows "Action
// failed").
func actionFail(format string, a ...any) *agentproto.Result {
	res := agentproto.Fail(format, a...)
	res.Origin = "action"
	return res
}

// actionRaise is an AnsibleActionFail raised by an action plugin
// (ansible-core shows "Task failed: <msg>").
func actionRaise(format string, a ...any) *agentproto.Result {
	res := agentproto.Fail(format, a...)
	res.Origin = "raised"
	return res
}

// runCopy is ansible.builtin.copy's action plugin: validate the arguments,
// resolve the local source (or inline content), then hand the bytes to the
// target, which performs ActionModule._copy_file and the copy/file module
// calls in one round trip.
func runCopy(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	for _, internal := range []string{"_original_basename", "_diff_peek"} {
		if args[internal] != nil {
			return actionFail("Invalid parameter specified: \"%s\"", internal)
		}
	}
	if err := checkFileArgs("copy", args); err != nil {
		return agentproto.Fail("%v", err)
	}
	source, _ := args["src"].(string)
	content, hasContent := args["content"]
	if content == nil {
		hasContent = false
	}
	dest, _ := args["dest"].(string)
	switch {
	case source == "" && !hasContent:
		return actionFail("src (or content) is required")
	case dest == "":
		return actionFail("dest is required")
	case source != "" && hasContent:
		return actionFail("src and content are mutually exclusive")
	case hasContent && strings.HasSuffix(dest, "/"):
		return actionFail("can not use content with a dir as dest")
	}

	if hasContent {
		var data []byte
		switch t := content.(type) {
		case string:
			data = []byte(t)
		case map[string]any, []any:
			data = []byte(template.PyJSON(t, 0, false, true))
		default:
			if m := template.Plain(t); m != nil {
				if _, isMap := m.(map[string]any); isMap {
					data = []byte(template.PyJSON(m, 0, false, true))
					break
				}
			}
			data = []byte(template.PyStr(t))
		}
		tmp := filepath.Join(localTmp(), "."+randomName())
		return stripNone(forwardToCopy(ctx, actx, args, data, tmp, filepath.Base(tmp), true))
	}
	if isTruthy(args["remote_src"]) {
		// The source lives on the target: the module reads it there.
		return (&Normal{Module: "copy"}).Run(ctx, actx, args, "")
	}

	trailing := strings.HasSuffix(source, "/")
	resolved, fail := findNeedle(actx, "files", source)
	if fail != nil {
		return fail
	}
	if trailing != strings.HasSuffix(resolved, "/") {
		if strings.HasSuffix(resolved, "/") {
			resolved = resolved[:len(resolved)-1]
		} else {
			resolved += "/"
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return actionFail("could not find src=%s, %v", resolved, err)
	}
	if info.IsDir() {
		return copyLocalTree(ctx, actx, args, source, strings.TrimSuffix(resolved, "/"))
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return actionFail("could not find src=%s, %v", resolved, err)
	}
	fwd := args
	if args["mode"] == "preserve" {
		fwd = withArg(args, "mode", fmt.Sprintf("0%03o", fsutil.UnixBits(info.Mode())))
	}
	return stripNone(forwardToCopy(ctx, actx, fwd, data, resolved, filepath.Base(resolved), false))
}

// stripNone drops the directory-copy marker from a single file's result.
func stripNone(res *agentproto.Result) *agentproto.Result {
	if res.Extra != nil {
		delete(res.Extra, copyNoneKey)
	}
	return res
}

func withArg(args map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(args)+1)
	for key, val := range args {
		out[key] = val
	}
	out[k] = v
	return out
}

// runTemplate renders the local template with host vars, then copies the
// result like the copy action does (ansible's template action delegates
// to copy with the rendered file as src).
func runTemplate(ctx context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	src, _ := args["src"].(string)
	dest, _ := args["dest"].(string)
	switch {
	case args["state"] != nil:
		return actionRaise("'state' cannot be specified on a template")
	case args["src"] == nil || args["dest"] == nil:
		return actionRaise("src and dest are required")
	}
	if err := checkFileArgs("template", args); err != nil {
		return agentproto.Fail("%v", err)
	}
	resolved, searched := searchNeedle(actx, "templates", src)
	if resolved == "" {
		// AnsibleActionFail(to_text(AnsibleFileNotFound)) raised while
		// handling the lookup error.
		msg := fileNotFound(src, searched)
		res := actionRaise("%s", msg)
		res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: msg}
		return res
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return actionFail("template: %v", err)
	}
	mode := args["mode"]
	if mode == "preserve" {
		if info, err := os.Stat(resolved); err == nil {
			mode = fmt.Sprintf("0%03o", fsutil.UnixBits(info.Mode()))
		}
	}

	tvars := actx.Vars.WithOverlay(map[string]any{
		"template_path":   resolved,
		"template_host":   actx.Host,
		"ansible_managed": "Ansible managed",
	})
	pos := template.Position{File: resolved, Line: 1, Col: 1}
	// Ansible's template search path: the template's own directory, then
	// the role's and the playbook's templates directories.
	searchPath := []string{filepath.Dir(resolved)}
	if actx.SrcDir != "" {
		searchPath = append(searchPath, filepath.Join(actx.SrcDir, "templates"), actx.SrcDir)
	}
	searchPath = append(searchPath, filepath.Join(actx.BaseDir, "templates"), actx.BaseDir)
	rendered, err := tvars.RenderFile(string(raw), pos, searchPath...)
	if err != nil {
		return actionFail("template: error rendering %s: %v", src, err)
	}

	fwd := make(map[string]any, len(args))
	for k, v := range args {
		switch k {
		case "newline_sequence", "block_start_string", "block_end_string", "variable_start_string",
			"variable_end_string", "comment_start_string", "comment_end_string", "trim_blocks",
			"lstrip_blocks", "output_encoding", "src":
			continue
		}
		fwd[k] = v
	}
	if mode != nil {
		fwd["mode"] = mode
	} else {
		delete(fwd, "mode")
	}
	_ = dest
	local := filepath.Join(localTmp(), "tmp"+randomName(), filepath.Base(resolved))
	return stripNone(forwardToCopy(ctx, actx, fwd, []byte(rendered), local, filepath.Base(resolved), false))
}

// forwardToCopy sends one file's content to the target's copy module,
// which performs the copy action's per-file work. source is the local path
// Ansible would report (a temp file for content/template), sourceRel its
// name relative to the copy root.
func forwardToCopy(ctx context.Context, actx *Context, args map[string]any, content []byte, source, sourceRel string, isContent bool) *agentproto.Result {
	fwd := make(map[string]any, len(args)+1)
	for k, v := range args {
		switch k {
		case "src", "content", "decrypt", "local_follow", "remote_src":
			continue
		}
		fwd[k] = v
	}
	fwd["_copy_action"] = map[string]any{
		"source": source, "source_rel": sourceRel, "content": isContent,
	}
	req := &agentproto.TaskRequest{
		Proto:      agentproto.ProtoVersion,
		Op:         "task",
		Module:     "copy",
		Args:       fwd,
		CheckMode:  actx.CheckMode,
		Diff:       actx.Diff,
		PayloadLen: int64(len(content)),
	}
	res, err := actx.RunModule(ctx, req, bytes.NewReader(content))
	if err != nil {
		return agentproto.Fail("copy: %v", err)
	}
	return res
}

// copyNoneKey marks the copy module's "nothing to do" answer for one file
// of a directory copy (ActionModule._copy_file returning None).
const copyNoneKey = "_copy_none"

var (
	localTmpOnce sync.Once
	localTmpDir  string
)

// localTmp is the name ansible-core gives its per-run controller temp
// directory (C.DEFAULT_LOCAL_TMP): ~/.ansible/tmp/ansible-local-<pid><rand>.
// Results report paths under it (the content/template staging file); it
// is never created.
func localTmp() string {
	localTmpOnce.Do(func() {
		home, _ := os.UserHomeDir()
		localTmpDir = filepath.Join(home, ".ansible", "tmp", fmt.Sprintf("ansible-local-%d%s", os.Getpid(), randomName()))
	})
	return localTmpDir
}

// randomName is tempfile's 8-character random name.
func randomName() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789_"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// searchNeedle is DataLoader.path_dwim_relative_stack over the task's
// search path: the role (if any), the task file's directory, then the
// playbook directory, each with and without the conventional subdir.
func searchNeedle(actx *Context, dirname, source string) (string, []string) {
	if strings.HasPrefix(source, "~") || strings.HasPrefix(source, "/") {
		p := source
		if strings.HasPrefix(p, "~") {
			if home, err := os.UserHomeDir(); err == nil && (p == "~" || strings.HasPrefix(p, "~/")) {
				p = home + p[1:]
			}
		}
		if _, err := os.Stat(p); err == nil {
			return filepath.Clean(p), nil
		}
		return "", nil
	}
	root := strings.SplitN(source, "/", 2)[0]
	var paths []string
	if actx.SrcDir != "" {
		paths = append(paths, actx.SrcDir)
	}
	if actx.TaskDir != "" && !contains(paths, actx.TaskDir) {
		paths = append(paths, actx.TaskDir)
	}
	var search []string
	add := func(base string) {
		if dirname != "" && root != dirname {
			search = append(search, pyJoin(base, dirname, source))
		}
		search = append(search, pyJoin(base, source))
	}
	for _, p := range paths {
		add(p)
	}
	add(actx.BaseDir)
	for _, c := range search {
		if _, err := os.Stat(c); err == nil {
			return c, search
		}
	}
	return "", search
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
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

// fileNotFound is AnsibleFileNotFound's message.
func fileNotFound(source string, searched []string) string {
	msg := fmt.Sprintf("Could not find or access '%s'", source)
	if len(searched) > 0 {
		msg += "\nSearched in:\n\t" + strings.Join(searched, "\n\t")
	}
	return msg + " on the Ansible Controller."
}

// findNeedle is the copy action's _find_needle: a miss is an
// AnsibleActionFail caused by AnsibleFileNotFound.
func findNeedle(actx *Context, dirname, source string) (string, *agentproto.Result) {
	found, searched := searchNeedle(actx, dirname, source)
	if found != "" {
		return found, nil
	}
	cause := fileNotFound(source, searched)
	res := actionFail("Unexpected AnsibleActionFail error: %s", cause)
	res.ErrorChain = &agentproto.ErrorChain{
		Outer: "Task failed: Unexpected AnsibleActionFail error.",
		Inner: cause,
		Help:  "If you are using a module and expect the file to exist on the remote, see the remote_src option.",
	}
	return "", res
}

// resolveSrc finds a source file: absolute, role-relative (roles/x/files or
// roles/x/templates), playbook-relative, or under the conventional
// subdirectory.
func resolveSrc(actx *Context, src, subdir string) string {
	found, _ := searchNeedle(actx, subdir, src)
	return found
}

// forwardedFileArgs are the copy/template options the target-side copy
// module implements.
var forwardedFileArgs = map[string]bool{
	"src": true, "dest": true, "mode": true, "owner": true, "group": true,
	"backup": true, "force": true, "directory_mode": true, "validate": true,
	"follow": true, "unsafe_writes": true, "remote_src": true,
}

// fileActionArgs lists what each action accepts; anything else fails like
// Ansible's "Unsupported parameters" rather than being silently dropped.
var fileActionArgs = map[string]map[string]bool{
	"copy":     {"content": true, "local_follow": true, "decrypt": true},
	"template": {"newline_sequence": true, "trim_blocks": true, "lstrip_blocks": true},
}

func checkFileArgs(action string, args map[string]any) error {
	var bad []string
	for k := range args {
		if !forwardedFileArgs[k] && !fileActionArgs[action][k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("Unsupported parameters for (%s) module: %s", action, strings.Join(bad, ", "))
	}
	if action == "template" {
		// Ansible's defaults (trim_blocks on, lstrip_blocks off, \n) are the
		// engine's behavior; other values would silently render differently.
		if v, ok := args["trim_blocks"]; ok && !isTruthy(v) {
			return fmt.Errorf("template: trim_blocks: false is not supported yet")
		}
		if v, ok := args["lstrip_blocks"]; ok && isTruthy(v) {
			return fmt.Errorf("template: lstrip_blocks: true is not supported yet")
		}
		if v, ok := args["newline_sequence"]; ok && v != "\n" {
			return fmt.Errorf("template: newline_sequence other than \\n is not supported yet")
		}
	}
	return nil
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(t) {
		case "yes", "true", "on", "1", "y":
			return true
		}
	}
	return false
}

// copyLocalTree is the copy action for a local directory (_walk_dirs plus
// the files/directories/symlinks passes of ActionModule.run): "dir/"
// copies the contents into dest, "dir" the directory itself. Files go to
// the target's copy module with dest as a directory; leaf directories
// and symlinks are made by the file module.
func copyLocalTree(ctx context.Context, actx *Context, args map[string]any, src, resolved string) *agentproto.Result {
	trailing := strings.HasSuffix(src, "/")
	source := resolved
	base := resolved
	if trailing {
		source += "/"
	} else {
		base = filepath.Dir(resolved)
	}
	localFollow := args["local_follow"] == nil || isTruthy(args["local_follow"])
	type entry struct{ from, rel string }
	var files, dirs, links []entry
	var walk func(dir, relBase string)
	walk = func(dir, relBase string) {
		fh, err := os.Open(dir)
		if err != nil {
			return
		}
		ents, _ := fh.ReadDir(-1) // directory order, like os.walk
		fh.Close()
		var subdirs []os.DirEntry
		for _, e := range ents {
			full := filepath.Join(dir, e.Name())
			rel := pyJoin(relBase, e.Name())
			info, err := os.Stat(full)
			if err == nil && info.IsDir() {
				subdirs = append(subdirs, e)
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				real, _ := filepath.EvalSymlinks(full)
				if ri, err := os.Stat(real); localFollow && err == nil && ri.Mode().IsRegular() {
					files = append(files, entry{real, rel})
				} else {
					target, _ := os.Readlink(full)
					links = append(links, entry{target, rel})
				}
				continue
			}
			files = append(files, entry{full, rel})
		}
		for _, e := range subdirs {
			full := filepath.Join(dir, e.Name())
			rel := pyJoin(relBase, e.Name())
			if e.Type()&os.ModeSymlink != 0 && !localFollow {
				target, _ := os.Readlink(full)
				links = append(links, entry{target, rel})
				continue
			}
			dirs = append(dirs, entry{full, rel})
			walk(full, rel)
		}
	}
	rootRel := ""
	if !trailing {
		rootRel, _ = filepath.Rel(base, resolved)
	}
	walk(resolved, rootRel)

	dest, _ := args["dest"].(string)
	if !strings.HasSuffix(dest, "/") {
		dest += "/"
	}
	changed, executed := false, false
	var last *agentproto.Result
	implicit := map[string]bool{}
	for _, f := range files {
		content, err := os.ReadFile(f.from)
		if err != nil {
			return actionFail("could not find src=%s, %v", f.from, err)
		}
		fargs := withArg(args, "dest", dest)
		fargs["follow"] = false
		res := forwardToCopy(ctx, actx, fargs, content, f.from, f.rel, false)
		if res.Extra != nil && res.Extra[copyNoneKey] == true {
			continue
		}
		if res.Failed {
			return res
		}
		for d := filepath.Dir(f.rel); d != "." && d != "/"; d = filepath.Dir(d) {
			implicit[d] = true
		}
		executed, changed, last = true, changed || res.Changed, res
	}
	fileArgs := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range args {
			switch k {
			case "mode", "owner", "group", "seuser", "serole", "selevel", "setype", "attributes",
				"attr", "unsafe_writes", "force":
				out[k] = v
			}
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	runFile := func(a map[string]any) *agentproto.Result {
		for k, v := range a {
			if v == nil {
				delete(a, k)
			}
		}
		res, err := actx.RunModule(ctx, &agentproto.TaskRequest{
			Proto: agentproto.ProtoVersion, Op: "task", Module: "file",
			Args: a, CheckMode: actx.CheckMode, Diff: actx.Diff,
		}, nil)
		if err != nil {
			return agentproto.Fail("copy: %v", err)
		}
		return res
	}
	for _, d := range dirs {
		if implicit[d.rel] {
			continue
		}
		res := runFile(fileArgs(map[string]any{"path": pyJoin(dest, d.rel), "state": "directory",
			"mode": args["directory_mode"], "recurse": false}))
		if res.Failed {
			return res
		}
		executed, changed, last = true, changed || res.Changed, res
	}
	for _, l := range links {
		a := fileArgs(map[string]any{"path": pyJoin(dest, l.rel), "src": l.from, "state": "link", "force": true})
		if len(dirs) > 0 {
			a["follow"] = false
		}
		if a["mode"] == "preserve" {
			delete(a, "mode")
		}
		res := runFile(a)
		executed = true
		if res.Failed {
			return res
		}
		changed, last = changed || res.Changed, res
	}
	if executed && len(files) == 1 && last != nil {
		if p, ok := last.Extra["path"]; ok {
			if _, has := last.Extra["dest"]; !has {
				last.Extra["dest"] = p
			}
		}
		return last
	}
	return &agentproto.Result{Changed: changed, Extra: map[string]any{"dest": dest, "src": source}}
}

// SearchNeedle exposes the control-side file search (_find_needle) to the
// executor's own actions (include_vars): the found path, or "" plus the
// searched candidates.
func SearchNeedle(actx *Context, dirname, source string) (string, []string) {
	return searchNeedle(actx, dirname, source)
}

// FileNotFound is AnsibleFileNotFound's message.
func FileNotFound(source string, searched []string) string {
	return fileNotFound(source, searched)
}
