package modules

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

func init() {
	Register(modprobeModule, "modprobe", "community.general.modprobe")
}

var modprobeSpec = args.Spec{
	"name":       {Required: true},
	"state":      {Default: "present", Choices: []string{"absent", "present"}},
	"params":     {Default: ""},
	"persistent": {Default: "disabled", Choices: []string{"disabled", "present", "absent"}},
}

// Where modprobe persists modules and their options (overridable in
// tests).
var (
	modulesLoadLocation     = "/etc/modules-load.d"
	parametersFilesLocation = "/etc/modprobe.d"
	procModulesPath         = "/proc/modules"
)

// modprobe is community.general.modprobe's Modprobe class.
type modprobe struct {
	env        *RunEnv
	bin        string
	name       string
	params     string
	state      string
	changed    bool
	warnings   []any
	reModule   *pyre.Pattern
	reParams   *pyre.Pattern
	reParamVal *pyre.Pattern
}

func (m *modprobe) result() map[string]any {
	return map[string]any{"name": m.name, "params": m.params, "state": m.state}
}

func (m *modprobe) fail(msg string, extra map[string]any) *agentproto.Result {
	r := agentproto.Fail("%s", msg)
	r.Changed = m.changed
	r.Extra = m.result()
	for k, v := range extra {
		r.Extra[k] = v
	}
	if len(m.warnings) > 0 {
		r.Extra["warnings"] = m.warnings
	}
	return r
}

func (m *modprobe) run(argv []string) (int, string, string) {
	return runCommand(m.env, argv, cmdOpts{Env: map[string]string{"LANGUAGE": "C", "LC_ALL": "C"}})
}

// modprobeModule ports community.general.modprobe.
func modprobeModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := modprobeSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	bin, err := getBinPath("modprobe")
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	m := &modprobe{env: env, bin: bin, name: p.Str("name"), params: p.Str("params"), state: p.Str("state")}
	// The name goes into the patterns as it is (a re.error crashes).
	var fail *agentproto.Result
	if m.reModule, fail = pyCompile(`^ *` + m.name + ` *(?:[#;].*)?\n?\Z`); fail != nil {
		return fail
	}
	if m.reParams, fail = pyCompile(`^options ` + m.name + ` \w+=\S+ *(?:[#;].*)?\n?\Z`); fail != nil {
		return fail
	}
	if m.reParamVal, fail = pyCompile(`^options ` + m.name + ` (\w+=\S+) *(?:[#;].*)?\n?\Z`); fail != nil {
		return fail
	}

	loaded, fail := m.moduleLoaded()
	if fail != nil {
		return fail
	}
	check := env.CheckMode
	extra, _ := shlexSplit(m.params)
	if m.state == "present" && !loaded {
		argv := []string{bin}
		if check {
			argv = append(argv, "-n")
		}
		argv = append(append(argv, m.name), extra...)
		rc, out, errOut := m.run(argv)
		if rc != 0 {
			return m.fail(errOut, map[string]any{"rc": int64(rc), "stdout": out, "stderr": errOut})
		}
		now, fail := m.moduleLoaded()
		if fail != nil {
			return fail
		}
		if check || now {
			m.changed = true
		} else {
			rc, _, errOut := m.run(append([]string{bin, "-n", "--first-time", m.name}, extra...))
			if rc != 0 {
				m.warnings = append(m.warnings, errOut)
			}
		}
	} else if m.state == "absent" && loaded {
		argv := []string{bin, "-r", m.name}
		if check {
			argv = append(argv, "-n")
		}
		rc, out, errOut := m.run(argv)
		if rc != 0 {
			return m.fail(errOut, map[string]any{"rc": int64(rc), "stdout": out, "stderr": errOut})
		}
		m.changed = true
	}

	switch p.Str("persistent") {
	case "present":
		if !(m.loadedPersistently() && m.paramsIsSet()) {
			if !m.loadedPersistently() {
				if !check {
					os.WriteFile(filepath.Join(modulesLoadLocation, m.name+".conf"), []byte(m.name+"\n"), 0o644)
				}
				m.changed = true
			}
			if !m.paramsIsSet() {
				m.commentOut(m.modprobeFiles(), m.reParams, check)
				if !check {
					var lines []string
					for _, prm := range strings.Fields(m.params) {
						lines = append(lines, "options "+m.name+" "+prm)
					}
					os.WriteFile(filepath.Join(parametersFilesLocation, m.name+".conf"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
				}
				m.changed = true
			}
		}
	case "absent":
		if m.loadedPersistently() || len(m.permanentParams()) > 0 {
			if m.loadedPersistently() {
				m.commentOut(m.modulesFiles(), m.reModule, check)
				m.changed = true
			}
			if len(m.permanentParams()) > 0 {
				m.commentOut(m.modprobeFiles(), m.reParams, check)
				m.changed = true
			}
		}
	}
	res := &agentproto.Result{Changed: m.changed, Extra: m.result()}
	if len(m.warnings) > 0 {
		res.Extra["warnings"] = m.warnings
	}
	return res
}

// moduleLoaded checks /proc/modules, then the kernel's modules.builtin.
func (m *modprobe) moduleLoaded() (bool, *agentproto.Result) {
	data, err := os.ReadFile(procModulesPath)
	if err != nil {
		return false, m.fail(pyStrOSError(err, procModulesPath), nil)
	}
	prefix := strings.ReplaceAll(m.name, "-", "_") + " "
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true, nil
		}
	}
	// platform.release(): the kernel release uname reports.
	rel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	release := strings.TrimSpace(string(rel))
	builtin := filepath.Join("/lib/modules/", release, "modules.builtin")
	data, err = os.ReadFile(builtin)
	if err != nil {
		return false, m.fail(pyStrOSError(err, builtin), nil)
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasSuffix(strings.TrimRight(line, " \t\r\n\f\v"), "/"+m.name+".ko") {
			return true, nil
		}
	}
	return false, nil
}

func listFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

func (m *modprobe) modulesFiles() []string  { return listFiles(modulesLoadLocation) }
func (m *modprobe) modprobeFiles() []string { return listFiles(parametersFilesLocation) }

func readLinesKeepNL(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.SplitAfter(string(data), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (m *modprobe) loadedPersistently() bool {
	for _, f := range m.modulesFiles() {
		for _, line := range readLinesKeepNL(f) {
			if m.reModule.Match(line, 0, -1) != nil {
				return true
			}
		}
	}
	return false
}

func (m *modprobe) permanentParams() map[string]bool {
	out := map[string]bool{}
	for _, f := range m.modprobeFiles() {
		for _, line := range readLinesKeepNL(f) {
			if sm := m.reParamVal.Match(line, 0, -1); sm != nil {
				out[line[sm[2]:sm[3]]] = true
			}
		}
	}
	return out
}

func (m *modprobe) paramsIsSet() bool {
	want := map[string]bool{}
	for _, f := range strings.Fields(m.params) {
		want[f] = true
	}
	have := m.permanentParams()
	if len(want) != len(have) {
		return false
	}
	for k := range want {
		if !have[k] {
			return false
		}
	}
	return true
}

// commentOut prefixes matching lines with '#' (disable_old_params /
// disable_module_permanent), rewriting the file as the module does: the
// kept lines joined with "\n".
func (m *modprobe) commentOut(files []string, re *pyre.Pattern, check bool) {
	sort.Strings(files)
	for _, f := range files {
		lines := readLinesKeepNL(f)
		changed := false
		for i, line := range lines {
			if re.Match(line, 0, -1) != nil {
				lines[i] = "#" + line
				changed = true
			}
		}
		if !check && changed {
			os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)
		}
	}
}
