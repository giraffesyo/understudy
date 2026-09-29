package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
)

// installLookups wires the control-side lookup plugins into the template
// engine (lookup(), query(), and with_<name> loops).
func (r *Runner) installLookups() {
	r.Engine.Lookup = func(ec *template.EvalCtx, name string, terms []any, kwargs map[string]any) (any, error) {
		switch name {
		case "env":
			out := make([]any, 0, len(terms))
			for _, t := range terms {
				out = append(out, os.Getenv(fmt.Sprintf("%v", t)))
			}
			return singleOrList(out), nil

		case "file":
			out := make([]any, 0, len(terms))
			for _, t := range terms {
				path := r.resolveLookupPath(fmt.Sprintf("%v", t))
				data, err := os.ReadFile(path)
				if err != nil {
					return nil, fmt.Errorf("file lookup: %v", err)
				}
				out = append(out, strings.TrimRight(string(data), "\n"))
			}
			return singleOrList(out), nil

		case "fileglob":
			var out []any
			for _, t := range terms {
				pattern := fmt.Sprintf("%v", t)
				matches, err := r.globLookup(pattern)
				if err != nil {
					return nil, err
				}
				out = append(out, matches...)
			}
			if out == nil {
				out = []any{}
			}
			return out, nil

		case "first_found":
			// Terms: file names (or a list); returns the first that exists.
			var names []string
			for _, t := range terms {
				if list, ok := t.([]any); ok {
					for _, item := range list {
						names = append(names, fmt.Sprintf("%v", item))
					}
				} else {
					names = append(names, fmt.Sprintf("%v", t))
				}
			}
			for _, n := range names {
				path := r.resolveLookupPath(n)
				if _, err := os.Stat(path); err == nil {
					return path, nil
				}
			}
			return nil, fmt.Errorf("first_found: none of the files exist: %s", strings.Join(names, ", "))

		case "dict":
			// with_dict: terms[0] is a mapping; items are {key, value}.
			if len(terms) != 1 {
				return nil, fmt.Errorf("dict lookup requires exactly one mapping")
			}
			var keys []string
			var get func(string) any
			switch m := terms[0].(type) {
			case template.Mapping:
				// Ordered mapping (e.g. *yaml.OMap from a var): keep insertion
				// order, which with_dict preserves in real Ansible.
				keys = m.Keys()
				get = func(k string) any { v, _ := m.GetItem(k); return v }
			case map[string]any:
				keys = make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				// A plain Go map has no inherent order; sort for determinism.
				sortStrings(keys)
				get = func(k string) any { return m[k] }
			default:
				return nil, fmt.Errorf("with_dict requires a dictionary, got %T", terms[0])
			}
			out := make([]any, 0, len(keys))
			for _, k := range keys {
				out = append(out, map[string]any{"key": k, "value": get(k)})
			}
			return out, nil

		case "password":
			return r.passwordLookup(terms, kwargs)

		case "pipe":
			return nil, fmt.Errorf("the pipe lookup is not supported yet")
		}
		return nil, fmt.Errorf("lookup plugin %q is not supported yet", name)
	}
}

func singleOrList(items []any) any {
	if len(items) == 1 {
		return items[0]
	}
	return items
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// resolveLookupPath anchors relative lookup paths at the playbook dir.
func (r *Runner) resolveLookupPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(r.Opts.BaseDir, path)
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
