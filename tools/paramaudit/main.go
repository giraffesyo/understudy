// Command paramaudit diffs the parameters understudy's modules accept
// against the argument specs of the installed ansible-core (and its
// collections): for every implemented module it reports the options
// Ansible defines that understudy does not accept, missing aliases, and
// names understudy accepts that Ansible does not know.
//
// The Ansible side comes from argspec.py, run with the Python interpreter
// that ansible-playbook uses: each module is resolved through ansible's
// plugin loader (so routing/redirects apply) and its argument_spec is
// captured from the module source by running main() against a patched
// AnsibleModule; action-only plugins fall back to DOCUMENTATION options.
//
// Usage:
//
//	go run ./tools/paramaudit [-python PATH] [-json] [-module NAME]...
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/modules"
)

//go:embed argspec.py
var argspecPy []byte

// skipNames are handled by the executor, not a module or action.
var skipNames = map[string]bool{
	"include_tasks": true, "include_vars": true, "include_role": true,
	"import_role": true, "import_tasks": true, "meta": true, "gather_facts": true,
}

type ansibleModule struct {
	Resolved string              `json:"resolved"`
	Path     string              `json:"path"`
	Via      string              `json:"via"`
	Params   map[string][]string `json:"params"`
	Error    *string             `json:"error"`
}

// Report is one module's audit result.
type Report struct {
	Module         string   `json:"module"`
	Names          []string `json:"names"`
	Resolved       string   `json:"resolved,omitempty"`
	Via            string   `json:"via,omitempty"`
	Missing        []string `json:"missing,omitempty"`
	MissingAliases []string `json:"missing_aliases,omitempty"`
	Extra          []string `json:"extra,omitempty"`
	Note           string   `json:"note,omitempty"`
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	python := flag.String("python", "", "Python interpreter with ansible importable (default: the one `ansible --version` reports)")
	asJSON := flag.Bool("json", false, "print JSON")
	var only multiFlag
	flag.Var(&only, "module", "audit only this module (short name; repeatable)")
	flag.Parse()

	groups := groupNames(only)
	shorts := make([]string, 0, len(groups))
	for s := range groups {
		shorts = append(shorts, s)
	}
	sort.Strings(shorts)

	queries := make([]string, len(shorts))
	for i, s := range shorts {
		queries[i] = queryName(s, groups[s])
	}
	py := *python
	if py == "" {
		py = ansiblePython()
	}
	ans, version, err := runArgspec(py, queries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paramaudit: %v\n", err)
		os.Exit(1)
	}

	var reports []Report
	for i, s := range shorts {
		r := Report{Module: s, Names: groups[s]}
		am := ans[queries[i]]
		accepted, declared := acceptedParams(groups[s])
		switch {
		case am == nil || am.Error != nil && am.Params == nil:
			msg := "not found"
			if am != nil && am.Error != nil {
				msg = *am.Error
			}
			r.Note = "ansible: " + msg
		case !declared:
			r.Resolved, r.Via = am.Resolved, am.Via
			r.Note = "understudy declares no parameter list (free-form)"
		default:
			r.Resolved, r.Via = am.Resolved, am.Via
			r.Missing, r.MissingAliases, r.Extra = diff(am.Params, accepted)
		}
		reports = append(reports, r)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"ansible_version": version, "modules": reports})
		return
	}
	fmt.Printf("ansible-core %s — %d modules audited\n\n", version, len(reports))
	var clean []string
	for _, r := range reports {
		if r.Note == "" && len(r.Missing)+len(r.MissingAliases)+len(r.Extra) == 0 {
			clean = append(clean, r.Module)
			continue
		}
		fmt.Printf("%s (%s)\n", r.Module, orDash(r.Resolved))
		if r.Note != "" {
			fmt.Printf("  note:            %s\n", r.Note)
		}
		if len(r.Missing) > 0 {
			fmt.Printf("  missing:         %s\n", strings.Join(r.Missing, ", "))
		}
		if len(r.MissingAliases) > 0 {
			fmt.Printf("  missing aliases: %s\n", strings.Join(r.MissingAliases, ", "))
		}
		if len(r.Extra) > 0 {
			fmt.Printf("  not in ansible:  %s\n", strings.Join(r.Extra, ", "))
		}
	}
	fmt.Printf("\ncomplete: %s\n", strings.Join(clean, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// groupNames maps each implemented module's short name to the names it
// is registered under.
func groupNames(only []string) map[string][]string {
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	groups := map[string][]string{}
	seen := map[string]bool{}
	for _, n := range append(modules.Names(), actions.Names()...) {
		if seen[n] {
			continue
		}
		seen[n] = true
		short := n[strings.LastIndexByte(n, '.')+1:]
		if skipNames[short] || len(want) > 0 && !want[short] {
			continue
		}
		groups[short] = append(groups[short], n)
	}
	for _, g := range groups {
		sort.Strings(g)
	}
	return groups
}

// queryName picks the name to resolve in Ansible: a registered FQCN
// (ansible.builtin first), else the short name.
func queryName(short string, names []string) string {
	var fq []string
	for _, n := range names {
		if strings.Contains(n, ".") {
			fq = append(fq, n)
		}
	}
	sort.Slice(fq, func(i, j int) bool {
		bi, bj := strings.HasPrefix(fq[i], "ansible.builtin."), strings.HasPrefix(fq[j], "ansible.builtin.")
		if bi != bj {
			return bi
		}
		return fq[i] < fq[j]
	})
	if len(fq) > 0 {
		return fq[0]
	}
	return short
}

func acceptedParams(names []string) (map[string]bool, bool) {
	out := map[string]bool{}
	declared := false
	for _, n := range names {
		p, ok := actions.AcceptedParams(n)
		if !ok {
			continue
		}
		declared = true
		for _, x := range p {
			out[x] = true
		}
	}
	return out, declared
}

// diff compares Ansible's {param: aliases} with understudy's accepted
// names.
func diff(ans map[string][]string, accepted map[string]bool) (missing, aliases, extra []string) {
	known := map[string]bool{}
	for name, al := range ans {
		known[name] = true
		ok := accepted[name] || name == "free_form" && accepted["_raw_params"]
		for _, a := range al {
			known[a] = true
			ok = ok || accepted[a]
		}
		if !ok {
			missing = append(missing, name)
			continue
		}
		for _, a := range append([]string{name}, al...) {
			if !accepted[a] {
				aliases = append(aliases, a)
			}
		}
	}
	for a := range accepted {
		if !known[a] && !(a == "_raw_params" && known["free_form"]) {
			extra = append(extra, a)
		}
	}
	sort.Strings(missing)
	sort.Strings(aliases)
	sort.Strings(extra)
	return
}

var pythonRe = regexp.MustCompile(`python version = .*\((/[^)]+)\)`)

// ansiblePython finds the interpreter ansible runs on.
func ansiblePython() string {
	cmd := exec.Command("ansible", "--version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err == nil {
		if m := pythonRe.FindSubmatch(out); m != nil {
			return string(m[1])
		}
	}
	return "python3"
}

func runArgspec(python string, names []string) (map[string]*ansibleModule, string, error) {
	dir, err := os.MkdirTemp("", "paramaudit")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	script := filepath.Join(dir, "argspec.py")
	if err := os.WriteFile(script, argspecPy, 0o644); err != nil {
		return nil, "", err
	}
	cmd := exec.Command(python, append([]string{script}, names...)...)
	cmd.Env = append(os.Environ(), "ANSIBLE_DEPRECATION_WARNINGS=False")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("%s argspec.py: %v", python, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, "", fmt.Errorf("argspec.py output: %v", err)
	}
	var version string
	json.Unmarshal(raw["_ansible_version"], &version)
	res := map[string]*ansibleModule{}
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue
		}
		var m ansibleModule
		if err := json.Unmarshal(v, &m); err == nil {
			res[k] = &m
		}
	}
	return res, version, nil
}
