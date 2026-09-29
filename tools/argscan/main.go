// Command argscan statically checks Ansible content against understudy's
// module argument specs: it parses every playbook and task file under a
// directory and reports each module parameter understudy would reject at
// run time, so gaps surface all at once instead of one failed run at a
// time.
//
//	go run ./tools/argscan <content-dir>
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/modules"
	"github.com/giraffesyo/understudy/internal/playbook"
)

type finding struct {
	module, param string
	sites         []string
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: argscan <content-dir>")
		os.Exit(2)
	}
	root := os.Args[1]
	playbook.ModuleKnown = actions.Known
	findings := map[string]*finding{}
	unchecked := map[string]int{}
	var parseErrs []string

	check := func(tasks []*playbook.Task) {
		for _, t := range tasks {
			spec, ok := modules.SpecOf(t.Module)
			if !ok {
				unchecked[t.Module]++
				continue
			}
			known := map[string]bool{}
			for name, def := range spec {
				known[name] = true
				for _, a := range def.Aliases {
					known[a] = true
				}
			}
			for param := range t.Args {
				if known[param] {
					continue
				}
				key := t.Module + "\x00" + param
				f := findings[key]
				if f == nil {
					f = &finding{module: t.Module, param: param}
					findings[key] = f
				}
				rel, _ := filepath.Rel(root, t.Src.File)
				f.sites = append(f.sites, fmt.Sprintf("%s:%d", rel, t.Src.Line))
			}
		}
	}

	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml")) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case len(parts) == 1: // top-level playbook
			plays, err := playbook.LoadFile(path)
			if err != nil {
				parseErrs = append(parseErrs, err.Error())
				return nil
			}
			for _, p := range plays {
				check(p.PreTasks)
				check(p.Tasks)
				check(p.PostTasks)
				check(p.Handlers)
			}
		case contains(parts, "tasks") || contains(parts, "handlers"):
			tasks, err := playbook.LoadTaskFile(path, "")
			if err != nil {
				parseErrs = append(parseErrs, err.Error())
				return nil
			}
			check(tasks)
		}
		return nil
	})

	keys := make([]string, 0, len(findings))
	for k := range findings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("Unsupported module parameters (%d):\n", len(keys))
	for _, k := range keys {
		f := findings[k]
		fmt.Printf("  %-22s %-24s x%-3d %s\n", f.module, f.param, len(f.sites), strings.Join(first(f.sites, 3), ", "))
	}
	if len(unchecked) > 0 {
		var names []string
		for n, c := range unchecked {
			names = append(names, fmt.Sprintf("%s(%d)", n, c))
		}
		sort.Strings(names)
		fmt.Printf("\nNot statically checked (no declared spec): %s\n", strings.Join(names, " "))
	}
	if len(parseErrs) > 0 {
		fmt.Printf("\nParse errors (%d):\n", len(parseErrs))
		for _, e := range parseErrs {
			fmt.Println("  " + e)
		}
	}
	if len(keys) > 0 || len(parseErrs) > 0 {
		os.Exit(1)
	}
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
