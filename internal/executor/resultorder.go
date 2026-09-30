package executor

import (
	"slices"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// ansible-core builds a registered result (UnifiedTaskResult.as_result_dict)
// from its known fields, in their declaration order, then the module's own
// keys in the order the module returned them, then stdout_lines and
// stderr_lines when it split them itself, then a loop's results. Python
// dicts keep that order, so it shows wherever a result is iterated or
// turned into text: {{ r }}, r.keys(), to_json, dict2items.

// resultFields are UnifiedTaskResult's exported fields, in order. "$loop"
// and "$index" stand for the loop and index variable names.
var resultFields = []string{
	"changed", "failed", "unreachable", "skipped", "warnings", "deprecations",
	"ansible_facts", "async_result", "exception", "finished", "msg", "rc",
	"module_stderr", "module_stdout", "attempts", "retries", "diff", "ansible_stats",
	"ansible_job_id", "skip_reason", "skipped_reason", "false_condition",
	"changed_when_result", "failed_when_result", "break_when_result",
	"changed_when_suppressed_exception", "failed_when_suppressed_exception",
	"break_when_suppressed_exception", "ansible_loop_var", "ansible_index_var",
	"$loop", "$index", "ansible_loop",
}

// pathInfo is AnsibleModule.add_path_info's keys, which exit_json adds to a
// result that has a path or dest (a key already present keeps its place).
var pathInfo = []string{"uid", "gid", "owner", "group", "mode", "state", "secontext", "size"}

// moduleKeyOrders is the order each module returns its keys in, one list
// per code path whose order differs ("@path" expands to pathInfo). A
// result takes the list sharing the most keys with it; keys no list names
// follow in sorted order.
var moduleKeyOrders = map[string][][]string{
	"command":     {{"stdout", "stderr", "cmd", "start", "end", "delta"}},
	"shell":       {{"stdout", "stderr", "cmd", "start", "end", "delta"}},
	"raw":         {{"stdout", "stdout_lines", "stderr", "stderr_lines"}},
	"script":      {{"stdout", "stdout_lines", "stderr", "stderr_lines"}},
	"stat":        {{"stat"}},
	"file":        {{"path", "@path"}, {"dest", "src", "@path"}, {"path", "state"}},
	"copy":        {{"dest", "src", "md5sum", "checksum", "backup_file", "@path"}, {"path", "@path", "checksum", "dest"}},
	"template":    {{"dest", "src", "md5sum", "checksum", "backup_file", "@path"}, {"path", "@path", "checksum", "dest"}},
	"lineinfile":  {{"found", "backup"}},
	"blockinfile": {{"backup_file"}},
	"replace":     {{"backup_file"}},
	"ini_file":    {{"path", "@path", "backup_file"}},
	"find":        {{"files", "matched", "examined", "skipped_paths"}},
	"slurp":       {{"content", "source", "encoding"}},
	"tempfile":    {{"path", "@path"}},
	"ping":        {{"ping"}},
	"wait_for":    {{"state", "port", "search_regex", "match_groups", "match_groupdict", "path", "elapsed", "@path"}},
	"get_url": {{"status_code", "checksum_dest", "checksum_src", "dest", "elapsed", "url", "src", "md5sum", "backup_file", "@path"},
		{"dest", "url", "checksum_dest", "checksum_src", "@path", "elapsed"}},
	"unarchive":    {{"handler", "dest", "src", "extract_results", "@path"}},
	"async_status": {{"started", "stdout", "stderr", "stdout_lines", "stderr_lines", "results_file", "cmd", "start", "end", "delta"}},
	"include_vars": {{"message", "ansible_included_var_files"}},
	"user": {{"name", "state", "force", "remove", "system", "create_home", "append", "move_home", "password",
		"stdout", "stderr", "uid", "group", "comment", "home", "shell", "groups",
		"ssh_fingerprint", "ssh_key_file", "ssh_public_key"}},
	"group":           {{"name", "state", "stdout", "stderr", "system", "gid"}},
	"service":         {{"name", "enabled", "state", "status"}},
	"systemd":         {{"name", "status", "enabled", "state"}},
	"systemd_service": {{"name", "status", "enabled", "state"}},
	"apt":             {{"stdout", "stderr", "cache_updated", "cache_update_time"}},
	"git":             {{"before", "after", "remote_url_changed"}},
	"hostname":        {{"name"}},
}

// nestedKeyOrders orders dicts a module nests in its result.
var nestedKeyOrders = map[string]map[string][]string{
	"stat": {"stat": statKeyOrder},
	"find": {"files": findFileKeyOrder},
}

// statKeyOrder is the stat module's format_output and what main() adds.
var statKeyOrder = []string{
	"exists", "path", "mode", "isdir", "ischr", "isblk", "isreg", "isfifo", "islnk", "issock",
	"uid", "gid", "size", "inode", "dev", "nlink", "atime", "mtime", "ctime",
	"wusr", "rusr", "xusr", "wgrp", "rgrp", "xgrp", "woth", "roth", "xoth", "isuid", "isgid",
	"blocks", "disk_usage_bytes", "block_size", "device_type", "flags", "generation", "birthtime",
	"file_type", "attrs", "object_type", "real_size", "creator",
	"readable", "writeable", "executable", "lnk_source", "lnk_target", "pw_name", "gr_name",
	"checksum", "mimetype", "charset", "version", "attributes", "attr_flags", "selinux_context",
}

// findFileKeyOrder is the find module's statinfo and what it adds.
var findFileKeyOrder = []string{
	"path", "mode", "isdir", "ischr", "isblk", "isreg", "isfifo", "islnk", "issock",
	"uid", "gid", "size", "inode", "dev", "nlink", "atime", "mtime", "ctime",
	"gr_name", "pw_name", "wusr", "rusr", "xusr", "wgrp", "rgrp", "xgrp", "woth", "roth", "xoth",
	"isuid", "isgid", "blocks", "disk_usage_bytes", "lnk_source", "lnk_target", "checksum",
}

// shortModule strips the builtin collection prefixes from a module name.
func shortModule(name string) string {
	return strings.TrimPrefix(strings.TrimPrefix(name, "ansible.builtin."), "ansible.legacy.")
}

// orderedResult is a result's registered value (ToVars) with its keys in
// ansible-core's order.
func orderedResult(task *playbook.Task, module string, m map[string]any) *yaml.OMap {
	out := yaml.NewOMap()
	loopVar, indexVar := "", ""
	if task != nil {
		loopVar, indexVar = task.LoopVar, task.IndexVar
	}
	if loopVar == "" {
		loopVar = "item"
	}
	placed := map[string]bool{}
	put := func(k string) {
		if v, ok := m[k]; ok && !placed[k] {
			placed[k] = true
			out.Set(k, v)
		}
	}
	for _, k := range resultFields {
		switch k {
		case "$loop":
			if _, ok := m["ansible_loop_var"]; ok {
				put(loopVar)
			}
		case "$index":
			if indexVar != "" {
				put(indexVar)
			}
		default:
			put(k)
		}
	}
	module = shortModule(module)
	if _, ok := m["results_file"]; ok {
		module = "async_status" // an async task's result is async_status's
	}
	for _, k := range bestKeyOrder(moduleKeyOrders[module], m, placed) {
		put(k)
	}
	var rest []string
	for k := range m {
		if !placed[k] && k != "stdout_lines" && k != "stderr_lines" && k != "results" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		put(k)
	}
	put("stdout_lines")
	put("stderr_lines")
	put("results")
	for key, order := range nestedKeyOrders[module] {
		if v, ok := out.GetItem(key); ok {
			out.Set(key, orderNested(v, order))
		}
	}
	return out
}

// bestKeyOrder picks the module's key list sharing the most keys with m
// (the first on a tie) and expands "@path".
func bestKeyOrder(orders [][]string, m map[string]any, placed map[string]bool) []string {
	var best []string
	bestScore := -1
	for _, order := range orders {
		var keys []string
		score := 0
		for _, k := range order {
			if k == "@path" {
				for _, p := range pathInfo {
					if !slices.Contains(keys, p) {
						keys = append(keys, p)
						if _, ok := m[p]; ok && !placed[p] {
							score++
						}
					}
				}
				continue
			}
			keys = append(keys, k)
			if _, ok := m[k]; ok && !placed[k] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = keys, score
		}
	}
	return best
}

// orderNested orders a nested dict (or each dict in a list) by order.
func orderNested(v any, order []string) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderNested(item, order)
		}
		return out
	case map[string]any:
		out := yaml.NewOMap()
		for _, k := range order {
			if val, ok := t[k]; ok {
				out.Set(k, val)
			}
		}
		var rest []string
		for k := range t {
			if !out.Has(k) {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			out.Set(k, t[k])
		}
		return out
	}
	return v
}
