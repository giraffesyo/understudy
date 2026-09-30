// Command argscan statically checks Ansible content against what understudy
// implements: it parses every playbook, task file, vars file and Jinja
// template under a directory and reports, all at once instead of one failed
// run at a time:
//
//   - modules understudy cannot resolve
//   - module parameters understudy would reject
//   - filters, tests and lookup plugins (lookup()/query()/with_<name>) the
//     template engine does not implement
//   - template syntax the engine cannot parse (unsupported statements)
//   - task/playbook parse errors (unknown keywords, malformed tasks)
//
// Usage:
//
//	go run ./tools/argscan [-json] [-all] <content-dir>
//
// -json prints one finding per line as JSON (for aggregating over many
// roles); -all also scans molecule/ and tests/ scaffolding.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/executor"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// Finding kinds.
const (
	kUnknownModule = "unknown-module"
	kParam         = "unsupported-param"
	kFilter        = "unknown-filter"
	kTest          = "unknown-test"
	kLookup        = "unknown-lookup"
	kTemplate      = "template-error"
	kParse         = "parse-error"
)

var kindTitles = []struct{ kind, title string }{
	{kUnknownModule, "Unknown modules"},
	{kParam, "Unsupported module parameters"},
	{kFilter, "Unknown filters"},
	{kTest, "Unknown tests"},
	{kLookup, "Unknown lookup plugins"},
	{kTemplate, "Template parse errors"},
	{kParse, "Parse errors"},
}

type Finding struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`             // module, module:param, filter, ...
	Detail string `json:"detail,omitempty"` // error text
	Site   string `json:"site"`             // file[:line], relative to root
}

// executorStatements are handled by the executor, not a module or action.
var executorStatements = map[string]bool{
	"include_tasks": true, "include_vars": true, "include_role": true,
	"import_role": true, "meta": true,
}

type scanner struct {
	root     string
	engine   *template.Engine
	findings []Finding
	seen     map[string]bool
}

func (s *scanner) add(f Finding) {
	key := f.Kind + "\x00" + f.Name + "\x00" + f.Site
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.findings = append(s.findings, f)
}

func (s *scanner) rel(path string) string {
	r, err := filepath.Rel(s.root, path)
	if err != nil {
		return path
	}
	return r
}

func main() {
	jsonOut := flag.Bool("json", false, "print findings as JSON lines")
	all := flag.Bool("all", false, "also scan molecule/ and tests/ directories")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: argscan [-json] [-all] <content-dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	root := flag.Arg(0)
	// Parse everything; unknown modules are reported, not fatal, so one
	// missing module does not hide the rest of its file.
	playbook.ModuleKnown = func(string) bool { return true }
	s := &scanner{root: root, engine: template.New(), seen: map[string]bool{}}

	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") ||
				(!*all && (name == "molecule" || name == "tests"))) {
				return filepath.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(s.rel(path))
		parts := strings.Split(rel, "/")
		dirs := parts[:len(parts)-1]
		isYAML := strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
		switch {
		case contains(dirs, "templates"):
			s.scanTemplateFile(path)
		case !isYAML:
		case contains(dirs, "tasks") || contains(dirs, "handlers"):
			load := func(p string) ([]*playbook.Task, error) { return playbook.LoadTaskFile(p, "") }
			if contains(dirs, "handlers") {
				load = playbook.LoadHandlerFile
			}
			tasks, err := load(path)
			if err != nil {
				s.add(Finding{Kind: kParse, Detail: err.Error(), Site: rel})
				return nil
			}
			s.checkTasks(tasks)
		case contains(dirs, "vars") || contains(dirs, "defaults") ||
			contains(dirs, "group_vars") || contains(dirs, "host_vars"):
			s.scanVarsFile(path)
		case contains(dirs, "meta") || strings.HasPrefix(name, "."):
		case looksLikePlaybook(path):
			plays, err := playbook.LoadFile(path)
			if err != nil {
				s.add(Finding{Kind: kParse, Detail: err.Error(), Site: rel})
				return nil
			}
			for _, p := range plays {
				for _, ts := range [][]*playbook.Task{p.PreTasks, p.Tasks, p.PostTasks, p.Handlers} {
					s.checkTasks(ts)
				}
			}
		}
		return nil
	})

	sort.SliceStable(s.findings, func(i, j int) bool {
		a, b := s.findings[i], s.findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Site < b.Site
	})
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, f := range s.findings {
			enc.Encode(f)
		}
	} else {
		printText(s.findings)
	}
	if len(s.findings) > 0 {
		os.Exit(1)
	}
}

func printText(fs []Finding) {
	for _, kt := range kindTitles {
		byName := map[string][]Finding{}
		var names []string
		for _, f := range fs {
			if f.Kind != kt.kind {
				continue
			}
			if _, ok := byName[f.Name]; !ok {
				names = append(names, f.Name)
			}
			byName[f.Name] = append(byName[f.Name], f)
		}
		if len(names) == 0 {
			continue
		}
		count := len(names)
		if names[0] == "" {
			count = len(byName[""])
		}
		fmt.Printf("%s (%d):\n", kt.title, count)
		for _, n := range names {
			group := byName[n]
			if n == "" { // parse/template errors: one line each
				for _, f := range group {
					fmt.Printf("  %s: %s\n", f.Site, oneLine(f.Detail))
				}
				continue
			}
			var sites []string
			for _, f := range group {
				sites = append(sites, f.Site)
			}
			fmt.Printf("  %-40s x%-3d %s\n", n, len(group), strings.Join(first(sites, 3), ", "))
		}
		fmt.Println()
	}
}

// looksLikePlaybook reports whether a top-level YAML file is a play list.
func looksLikePlaybook(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	v, err := yaml.Unmarshal(data, path)
	if err != nil {
		return false
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	m, ok := yaml.PlainMap(list[0])
	if !ok {
		return false
	}
	_, hosts := m["hosts"]
	_, imp := m["import_playbook"]
	_, imp2 := m["ansible.builtin.import_playbook"]
	return hosts || imp || imp2
}

func (s *scanner) checkTasks(tasks []*playbook.Task) {
	for _, t := range tasks {
		site := fmt.Sprintf("%s:%d", s.rel(t.Src.File), t.Src.Line)
		s.checkModule(t, site)
		// Templates anywhere in the task.
		s.scanValue(t.Name, site)
		s.scanValue(t.Args, site)
		s.scanValue(t.FreeForm, site)
		s.scanValue(t.Vars, site)
		s.scanValue(t.Environment, site)
		s.scanValue(t.LoopLabel, site)
		s.scanValue(t.Loop, site)
		for _, list := range [][]string{t.When, t.ChangedWhen, t.FailedWhen, {t.Until}} {
			for _, e := range list {
				s.scanExpr(e, site)
			}
		}
		if t.LoopWith != "" && !executor.LookupKnown(t.LoopWith) {
			s.add(Finding{Kind: kLookup, Name: t.LoopWith, Detail: "with_" + t.LoopWith, Site: site})
		}
	}
}

func (s *scanner) checkModule(t *playbook.Task, site string) {
	if executorStatements[t.Module] {
		return
	}
	if !actions.Known(t.Module) {
		s.add(Finding{Kind: kUnknownModule, Name: t.Module, Site: site})
		return
	}
	spec, ok := modules.SpecOf(t.Module)
	if !ok {
		return
	}
	known := map[string]bool{}
	for name, def := range spec {
		known[name] = true
		for _, a := range def.Aliases {
			known[a] = true
		}
	}
	for param := range t.Args {
		if !known[param] {
			s.add(Finding{Kind: kParam, Name: t.Module + ":" + param, Site: site})
		}
	}
}

// scanValue inspects every templated string inside a (nested) value.
func (s *scanner) scanValue(v any, site string) {
	switch t := v.(type) {
	case string:
		if template.HasTemplate(t) {
			s.inspect(t, false, site)
		}
	case yaml.VaultedString:
	case []any:
		for _, e := range t {
			s.scanValue(e, site)
		}
	case map[string]any:
		for k, e := range t {
			s.scanValue(k, site)
			s.scanValue(e, site)
		}
	default:
		if m, ok := yaml.PlainMap(v); ok {
			for k, e := range m {
				s.scanValue(k, site)
				s.scanValue(e, site)
			}
		}
	}
}

// scanExpr inspects a bare conditional (when:, changed_when:, ...).
func (s *scanner) scanExpr(e, site string) {
	if strings.TrimSpace(e) == "" {
		return
	}
	s.inspect(e, !template.HasTemplate(e), site)
}

func (s *scanner) inspect(src string, expr bool, site string) {
	var refs template.Refs
	var err error
	if expr {
		refs, err = s.engine.InspectExpr(src)
	} else {
		refs, err = s.engine.Inspect(src)
	}
	if err != nil {
		s.add(Finding{Kind: kTemplate, Detail: err.Error(), Site: site})
		return
	}
	for _, f := range refs.Filters {
		if _, ok := s.engine.Filters[f]; !ok {
			s.add(Finding{Kind: kFilter, Name: f, Site: site})
		}
	}
	for _, t := range refs.Tests {
		if _, ok := s.engine.Tests[t]; !ok {
			s.add(Finding{Kind: kTest, Name: t, Site: site})
		}
	}
	for _, l := range refs.Lookups {
		if !executor.LookupKnown(l) {
			s.add(Finding{Kind: kLookup, Name: l, Site: site})
		}
	}
}

func (s *scanner) scanTemplateFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	s.inspect(string(data), false, s.rel(path))
}

func (s *scanner) scanVarsFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	v, err := yaml.Unmarshal(data, path)
	if err != nil {
		s.add(Finding{Kind: kParse, Detail: err.Error(), Site: s.rel(path)})
		return
	}
	s.scanValue(v, s.rel(path))
}

func contains(parts []string, s string) bool {
	for _, p := range parts {
		if p == s {
			return true
		}
	}
	return false
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], "...")
	}
	return s
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	return s
}
