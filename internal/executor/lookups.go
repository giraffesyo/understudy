package executor

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
)

// lookupPlugin is one control-side lookup. Like Ansible's LookupBase.run it
// always returns a list; lookup() then joins/unwraps it, while query() and
// with_<name> loops use the list as-is.
type lookupPlugin func(r *Runner, ec *template.EvalCtx, terms []any, kw map[string]any) ([]any, error)

var lookupPlugins = map[string]lookupPlugin{
	"env":                 lookupEnv,
	"file":                lookupFile,
	"fileglob":            lookupFileglob,
	"first_found":         lookupFirstFound,
	"dict":                lookupDict,
	"password":            lookupPassword,
	"items":               lookupItems,
	"list":                lookupList,
	"indexed_items":       lookupIndexedItems,
	"flattened":           lookupFlattened,
	"nested":              lookupNested,
	"cartesian":           lookupNested,
	"together":            lookupTogether,
	"subelements":         lookupSubelements,
	"sequence":            lookupSequence,
	"random_choice":       lookupRandomChoice,
	"lines":               lookupLines,
	"pipe":                lookupPipe,
	"template":            lookupTemplate,
	"vars":                lookupVars,
	"varnames":            lookupVarnames,
	"ini":                 lookupIni,
	"csvfile":             lookupCsvfile,
	"inventory_hostnames": lookupInventoryHostnames,
	"unvault":             lookupUnvault,
}

// installLookups wires the control-side lookup plugins into the template
// engine (lookup(), query(), and with_<name> loops).
func (r *Runner) installLookups() {
	r.Engine.Lookup = func(ec *template.EvalCtx, name string, terms []any, kwargs map[string]any) (any, error) {
		name = strings.TrimPrefix(strings.TrimPrefix(name, "ansible.builtin."), "community.general.")
		plugin, ok := lookupPlugins[name]
		if !ok {
			return nil, fmt.Errorf("lookup plugin %q is not supported yet", name)
		}
		out, err := plugin(r, ec, terms, kwargs)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
}

// ---- helpers ----

func termStrings(terms []any) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		out = append(out, template.PyStr(t))
	}
	return out
}

// kvTerm splits "key opt=v opt2=v2" (Ansible's parse_kv on a lookup term),
// with kwargs overriding.
func kvTerm(term string, kw map[string]any) (string, map[string]string) {
	opts := map[string]string{}
	var key []string
	for _, f := range strings.Fields(term) {
		if k, v, ok := strings.Cut(f, "="); ok && k != "" {
			opts[k] = strings.Trim(v, `"'`)
		} else {
			key = append(key, f)
		}
	}
	for k, v := range kw {
		opts[k] = template.PyStr(v)
	}
	return strings.Join(key, " "), opts
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return []any{v}
}

func truthyArg(v any) bool {
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

// resolveLookupPath anchors relative lookup paths at the playbook dir.
func (r *Runner) resolveLookupPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(r.Opts.BaseDir, path)
}

// findLookupFile searches a relative file like Ansible's
// find_file_in_search_path: <subdir>/ then the playbook dir.
func (r *Runner) findLookupFile(name, subdir string) string {
	if filepath.IsAbs(name) {
		return name
	}
	for _, c := range []string{filepath.Join(r.Opts.BaseDir, subdir, name), filepath.Join(r.Opts.BaseDir, name), name} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return filepath.Join(r.Opts.BaseDir, name)
}

func shellOut(cmd string) (string, int, error) {
	c := exec.Command("/bin/sh", "-c", cmd)
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = os.Stderr
	err := c.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout.String(), ee.ExitCode(), nil
	}
	return stdout.String(), 0, err
}

// ---- plugins ----

func lookupList(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	return terms, nil
}

func lookupPassword(r *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	out := make([]any, 0, len(terms))
	for _, t := range termStrings(terms) {
		p, err := parsePasswordTerm(t, kw)
		if err != nil {
			return nil, err
		}
		pw, err := r.readOrCreatePassword(p)
		if err != nil {
			return nil, err
		}
		out = append(out, pw)
	}
	return out, nil
}

func lookupEnv(_ *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	out := make([]any, 0, len(terms))
	for _, t := range termStrings(terms) {
		if v, ok := os.LookupEnv(t); ok {
			out = append(out, v)
		} else if d, ok := kw["default"]; ok {
			out = append(out, d)
		} else {
			out = append(out, "")
		}
	}
	return out, nil
}

func lookupFile(r *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	out := make([]any, 0, len(terms))
	for _, t := range termStrings(terms) {
		data, err := os.ReadFile(r.findLookupFile(t, "files"))
		if err != nil {
			return nil, fmt.Errorf("The 'file' lookup had an issue accessing the file '%s'", t)
		}
		s := string(data)
		if rstrip, ok := kw["rstrip"]; !ok || truthyArg(rstrip) {
			s = strings.TrimRight(s, " \t\r\n")
		}
		if lstrip, ok := kw["lstrip"]; ok && truthyArg(lstrip) {
			s = strings.TrimLeft(s, " \t\r\n")
		}
		out = append(out, s)
	}
	return out, nil
}

func lookupFileglob(r *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, pattern := range termStrings(terms) {
		matches, err := r.globLookup(pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, matches...)
	}
	return out, nil
}

// globLookup expands a fileglob pattern (absolute, or playbook-relative,
// checking the conventional files/ subdirectory too), returning only files.
func (r *Runner) globLookup(pattern string) ([]any, error) {
	patterns := []string{pattern}
	if !filepath.IsAbs(pattern) {
		patterns = []string{
			filepath.Join(r.Opts.BaseDir, "files", pattern),
			filepath.Join(r.Opts.BaseDir, pattern),
		}
	}
	var out []any
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("fileglob: bad pattern %q: %v", pattern, err)
		}
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			break
		}
	}
	return out, nil
}

// lookupFirstFound: terms are file names, lists of names, or dicts with
// files/paths/skip (and kwargs of the same names).
func lookupFirstFound(r *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	var files, paths []string
	skip := false
	addOpts := func(m map[string]any) {
		if f, ok := m["files"]; ok {
			for _, x := range asList(f) {
				files = append(files, splitCommaList(template.PyStr(x))...)
			}
		}
		if p, ok := m["paths"]; ok {
			for _, x := range asList(p) {
				paths = append(paths, splitCommaList(template.PyStr(x))...)
			}
		}
		if s, ok := m["skip"]; ok {
			skip = truthyArg(s)
		}
	}
	add := func(x any) {
		if m, ok := template.Plain(x).(map[string]any); ok {
			addOpts(m)
		} else {
			files = append(files, template.PyStr(x))
		}
	}
	for _, t := range terms {
		if l, ok := t.([]any); ok {
			for _, x := range l {
				add(x)
			}
		} else {
			add(t)
		}
	}
	addOpts(kw)
	var candidates []string
	for _, f := range files {
		if len(paths) == 0 || filepath.IsAbs(f) {
			candidates = append(candidates, f)
			continue
		}
		for _, p := range paths {
			candidates = append(candidates, filepath.Join(p, f))
		}
	}
	for _, c := range candidates {
		path := r.findLookupFile(c, "files")
		if _, err := os.Stat(path); err == nil {
			abs, _ := filepath.Abs(path)
			return []any{abs}, nil
		}
	}
	if skip {
		return []any{}, nil
	}
	return nil, fmt.Errorf("No file was found when using first_found.")
}

func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func lookupDict(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, t := range terms {
		var keys []string
		var get func(string) any
		switch m := t.(type) {
		case template.Mapping:
			// Ordered mapping: keep insertion order, as with_dict does.
			keys = m.Keys()
			get = func(k string) any { v, _ := m.GetItem(k); return v }
		case map[string]any:
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			get = func(k string) any { return m[k] }
		default:
			return nil, fmt.Errorf("with_dict expects a dict")
		}
		for _, k := range keys {
			out = append(out, map[string]any{"key": k, "value": get(k)})
		}
	}
	return out, nil
}

// lookupItems flattens the terms one level (with_items).
func lookupItems(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	out := []any{}
	for _, t := range terms {
		if l, ok := t.([]any); ok {
			out = append(out, l...)
		} else {
			out = append(out, t)
		}
	}
	return out, nil
}

func lookupIndexedItems(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	out := make([]any, len(terms))
	for i, t := range terms {
		out[i] = []any{int64(i), t}
	}
	return out, nil
}

func lookupFlattened(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				walk(x)
			}
		case nil:
		default:
			if s, ok := t.(string); ok && (s == "None" || s == "null") {
				return
			}
			out = append(out, t)
		}
	}
	walk(terms)
	return out, nil
}

// lookupNested is the cartesian product of the term lists.
func lookupNested(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	if len(terms) == 0 {
		return nil, fmt.Errorf("with_nested requires at least one element in the nested list")
	}
	result := [][]any{{}}
	for _, t := range terms {
		var next [][]any
		for _, prefix := range result {
			for _, x := range asList(t) {
				next = append(next, append(append([]any{}, prefix...), x))
			}
		}
		result = next
	}
	out := make([]any, len(result))
	for i, row := range result {
		out[i] = row
	}
	return out, nil
}

// lookupTogether zips the lists, padding short ones with None.
func lookupTogether(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	if len(terms) == 0 {
		return nil, fmt.Errorf("with_together requires at least one element in each list")
	}
	longest := 0
	for _, t := range terms {
		longest = max(longest, len(asList(t)))
	}
	out := make([]any, longest)
	for i := range longest {
		row := make([]any, len(terms))
		for j, t := range terms {
			if l := asList(t); i < len(l) {
				row[j] = l[i]
			}
		}
		out[i] = row
	}
	return out, nil
}

// lookupSubelements pairs each element with each entry of its sub-list.
func lookupSubelements(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	if len(terms) < 2 || len(terms) > 3 {
		return nil, fmt.Errorf("subelements lookup expects a list of two or three items")
	}
	skipMissing := false
	if len(terms) == 3 {
		if m, ok := template.Plain(terms[2]).(map[string]any); ok {
			skipMissing = truthyArg(m["skip_missing"])
		}
	}
	var list []any
	if m, ok := template.Plain(terms[0]).(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			list = append(list, m[k])
		}
	} else {
		list = asList(terms[0])
	}
	path := strings.Split(template.PyStr(terms[1]), ".")
	var out []any
	for _, el := range list {
		// Like Ansible, the sub-list key is popped from a copy of the
		// element: item.0 is the parent without it.
		root, ok := template.Plain(el).(map[string]any)
		if !ok {
			return nil, fmt.Errorf("subelements lookup expects a dictionary, got '%s'", template.PyStr(el))
		}
		parent := copyMap(root)
		el = parent
		var cur any = parent
		missing := false
		for i, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("the key %s should point to a dictionary, got '%s'", key, template.PyStr(cur))
			}
			v, present := m[key]
			if !present {
				missing = true
				break
			}
			if i == len(path)-1 {
				delete(m, key)
			} else if sub, isMap := v.(map[string]any); isMap {
				v = copyMap(sub)
				m[key] = v
			}
			cur = v
		}
		if missing {
			if skipMissing {
				continue
			}
			return nil, fmt.Errorf("could not find '%s' key in iterated item '%s'", path[len(path)-1], template.PyStr(el))
		}
		subs, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("the key %s should point to a list, got '%s'", path[len(path)-1], template.PyStr(cur))
		}
		for _, s := range subs {
			out = append(out, []any{el, s})
		}
	}
	return out, nil
}

var seqShortcut = regexp.MustCompile(`^(?:(0[xX][0-9a-fA-F]+|0[0-7]*|[1-9][0-9]*|-[0-9]+)-)?(0[xX][0-9a-fA-F]+|0[0-7]*|[1-9][0-9]*|-[0-9]+)(?:/(0[xX][0-9a-fA-F]+|0[0-7]*|[1-9][0-9]*))?(?::(.+))?$`)

// lookupSequence generates formatted numbers: "start=1 end=5 stride=2
// format=host%02d", "count=3", or the shortcut "[start-]end[/stride][:fmt]".
func lookupSequence(_ *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	var out []any
	parseN := func(s string) (int64, error) { return strconv.ParseInt(s, 0, 64) }
	for _, term := range termStrings(terms) {
		start, end, stride, count := int64(1), int64(-1), int64(1), int64(-1)
		format := "%d"
		if m := seqShortcut.FindStringSubmatch(strings.TrimSpace(term)); m != nil && !strings.Contains(term, "=") {
			if m[1] != "" {
				start, _ = parseN(m[1])
			}
			end, _ = parseN(m[2])
			if m[3] != "" {
				stride, _ = parseN(m[3])
			}
			if m[4] != "" {
				format = m[4]
			}
		} else {
			_, opts := kvTerm(term, kw)
			for k, v := range opts {
				if k == "format" {
					format = v
					continue
				}
				n, err := parseN(v)
				if err != nil {
					return nil, fmt.Errorf("can't parse %s=%s as integer", k, v)
				}
				switch k {
				case "start":
					start = n
				case "end":
					end = n
				case "stride":
					stride = n
				case "count":
					count = n
				default:
					return nil, fmt.Errorf("unrecognized arguments to with_sequence: %s", k)
				}
			}
		}
		if count >= 0 {
			if end >= 0 {
				return nil, fmt.Errorf("can't specify both count and end in with_sequence")
			}
			if count == 0 {
				continue
			}
			end = start + (count-1)*stride
		}
		if end < 0 && count < 0 {
			return nil, fmt.Errorf("must specify count or end in with_sequence")
		}
		if stride == 0 {
			continue
		}
		if stride > 0 && end < start {
			return nil, fmt.Errorf("to count backwards make stride negative")
		}
		for i := start; (stride > 0 && i <= end) || (stride < 0 && i >= end); i += stride {
			out = append(out, fmt.Sprintf(format, i))
		}
	}
	return out, nil
}

func lookupRandomChoice(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	if len(terms) == 0 {
		return []any{}, nil
	}
	return []any{terms[rand.Intn(len(terms))]}, nil
}

func lookupLines(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, cmd := range termStrings(terms) {
		stdout, rc, err := shellOut(cmd)
		if err != nil || rc != 0 {
			return nil, fmt.Errorf("lookup_plugin.lines(%s) returned %d", cmd, rc)
		}
		sc := bufio.NewScanner(strings.NewReader(stdout))
		for sc.Scan() {
			out = append(out, sc.Text())
		}
	}
	return out, nil
}

func lookupPipe(_ *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, cmd := range termStrings(terms) {
		stdout, rc, err := shellOut(cmd)
		if err != nil || rc != 0 {
			return nil, fmt.Errorf("lookup_plugin.pipe(%s) returned %d", cmd, rc)
		}
		out = append(out, strings.TrimRight(stdout, " \t\r\n"))
	}
	return out, nil
}

// lookupTemplate renders template files with the current variables
// (search path: templates/ then the playbook dir).
func lookupTemplate(r *Runner, ec *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, name := range termStrings(terms) {
		path := r.findLookupFile(name, "templates")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("the template file %s could not be found for the lookup", name)
		}
		search := []string{filepath.Dir(path), filepath.Join(r.Opts.BaseDir, "templates"), r.Opts.BaseDir}
		s, err := r.Engine.RenderFile(string(data), ec.Vars(), template.Position{File: path, Line: 1, Col: 1}, search)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func lookupVars(_ *Runner, ec *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	var out []any
	for _, name := range termStrings(terms) {
		if v, ok := ec.Vars().Get(name); ok {
			out = append(out, v)
		} else if d, ok := kw["default"]; ok {
			out = append(out, d)
		} else {
			return nil, fmt.Errorf("No variable found with this name: %s", name)
		}
	}
	return out, nil
}

func lookupVarnames(_ *Runner, ec *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	namer, ok := ec.Vars().(interface{ Names() []string })
	if !ok {
		return nil, fmt.Errorf("varnames: variable names are not available here")
	}
	names := namer.Names()
	sort.Strings(names)
	var out []any
	for _, pat := range termStrings(terms) {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("Unable to use %q as a search parameter: %v", pat, err)
		}
		for _, n := range names {
			// Python re.search semantics.
			if re.MatchString(n) {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// lookupIni reads a key from an INI file section, or a Java properties
// file (type=properties).
func lookupIni(r *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	var out []any
	for _, term := range termStrings(terms) {
		key, opts := kvTerm(term, kw)
		file := opts["file"]
		if file == "" {
			file = "ansible.ini"
		}
		section := opts["section"]
		if section == "" {
			section = "global"
		}
		props := opts["type"] == "properties"
		data, err := os.ReadFile(r.findLookupFile(file, "files"))
		if err != nil {
			return nil, fmt.Errorf("The ini lookup had an issue reading %s: %v", file, err)
		}
		values := parseIni(string(data), props)
		sec := values[section]
		if props {
			sec = values[""]
		}
		if truthyArg(opts["re"]) {
			re, err := regexp.Compile(key)
			if err != nil {
				return nil, err
			}
			keys := make([]string, 0, len(sec))
			for k := range sec {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if re.MatchString(k) {
					out = append(out, sec[k])
				}
			}
			continue
		}
		lookupKey := key
		if !props && !truthyArg(opts["case_sensitive"]) {
			lookupKey = strings.ToLower(key)
		}
		if v, ok := sec[lookupKey]; ok {
			out = append(out, v)
		} else {
			out = append(out, opts["default"])
		}
	}
	return out, nil
}

// parseIni returns section -> key -> value ("" section for properties).
// INI keys are lower-cased (configparser's default optionxform).
func parseIni(data string, properties bool) map[string]map[string]string {
	out := map[string]map[string]string{"": {}}
	cur := ""
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || (properties && strings.HasPrefix(line, "!")) {
			continue
		}
		if !properties && strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			if out[cur] == nil {
				out[cur] = map[string]string{}
			}
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		if !properties {
			k = strings.ToLower(k)
		}
		out[cur][k] = strings.TrimSpace(line[i+1:])
	}
	return out
}

func lookupCsvfile(r *Runner, _ *template.EvalCtx, terms []any, kw map[string]any) ([]any, error) {
	var out []any
	for _, term := range termStrings(terms) {
		key, opts := kvTerm(term, kw)
		file := opts["file"]
		if file == "" {
			file = "ansible.csv"
		}
		delim := opts["delimiter"]
		switch delim {
		case "", "TAB", "\\t":
			delim = "\t"
		}
		col := 1
		if c := opts["col"]; c != "" {
			n, err := strconv.Atoi(c)
			if err != nil {
				return nil, fmt.Errorf("csvfile: col must be an integer")
			}
			col = n
		}
		f, err := os.Open(r.findLookupFile(file, "files"))
		if err != nil {
			return nil, fmt.Errorf("csvfile: %v", err)
		}
		rd := csv.NewReader(f)
		rd.Comma = []rune(delim)[0]
		rd.FieldsPerRecord = -1
		rows, err := rd.ReadAll()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("csvfile: %v", err)
		}
		var found any
		if d, ok := opts["default"]; ok {
			found = d
		}
		for _, row := range rows {
			if len(row) > 0 && row[0] == key {
				if col < len(row) {
					found = row[col]
				}
				break
			}
		}
		out = append(out, found)
	}
	return out, nil
}

func lookupInventoryHostnames(r *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, pattern := range termStrings(terms) {
		hosts, err := r.Inv.Match(pattern)
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			out = append(out, h.Name)
		}
	}
	return out, nil
}

func lookupUnvault(r *Runner, _ *template.EvalCtx, terms []any, _ map[string]any) ([]any, error) {
	var out []any
	for _, name := range termStrings(terms) {
		data, err := os.ReadFile(r.findLookupFile(name, "files"))
		if err != nil {
			return nil, fmt.Errorf("Unable to find file matching %q", name)
		}
		if r.Opts.Vault != nil {
			if data, err = r.Opts.Vault.MaybeDecryptFile(data); err != nil {
				return nil, err
			}
		}
		out = append(out, string(data))
	}
	return out, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
