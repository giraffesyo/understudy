package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

func init() {
	Register(sysctlModule, "sysctl", "ansible.posix.sysctl")
}

var sysctlSpec = args.Spec{
	"name":         {Required: true, Aliases: []string{"key"}},
	"value":        {Type: "any", Aliases: []string{"val"}},
	"state":        {Default: "present", Choices: []string{"present", "absent"}},
	"sysctl_file":  {Default: "/etc/sysctl.conf"},
	"sysctl_set":   {Type: "bool", Default: false},
	"reload":       {Type: "bool", Default: true},
	"ignoreerrors": {Type: "bool", Default: false},
}

// sysctlLangEnv is SysctlModule.LANG_ENV: sysctl's stderr is parsed.
var sysctlLangEnv = map[string]string{"LANG": "C", "LC_ALL": "C", "LC_MESSAGES": "C"}

// sysctlStderrFailed is SysctlModule._stderr_failed: sysctl can exit 0
// after failing to set a key.
var sysctlStderrFailed = regexp.MustCompile(`(?m)^sysctl: setting key "[^"]+": (Invalid argument|Read-only file system)$`)

// sysctlModule is a port of ansible.posix.sysctl: the sysctl file is
// normalized and rewritten, and live values are read, set (sysctl -w) and
// reloaded (sysctl -p) with the sysctl binary, so failures carry its
// messages.
func sysctlModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := sysctlSpec.Parse(rawArgs)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	name := p.Str("name")
	state := p.Str("state")
	rawValue := p.Any("value")
	if state == "present" && rawValue == nil {
		return agentproto.Fail("state is present but all of the following are missing: value")
	}
	if name == "" {
		return agentproto.Fail("name cannot be blank")
	}
	if state == "present" && sysctlStr(rawValue) == "" {
		return agentproto.Fail("value cannot be blank")
	}
	s := &sysctl{
		env:          env,
		name:         strings.TrimSpace(name),
		value:        sysctlParseValue(rawValue),
		state:        state,
		file:         pyExpandPath(p.Str("sysctl_file")),
		reload:       p.Bool("reload"),
		sysctlSet:    p.Bool("sysctl_set"),
		ignoreErrors: p.Bool("ignoreerrors"),
		platform:     runtime.GOOS,
		fileValues:   map[string]*string{},
	}
	s.cmd, err = getBinPath("sysctl")
	if err != nil {
		return agentproto.Fail("%s", err.Error())
	}
	if fail := s.process(); fail != nil {
		return fail
	}
	return &agentproto.Result{Changed: s.changed}
}

type sysctl struct {
	env                             *RunEnv
	name, value, state, file, cmd   string
	reload, sysctlSet, ignoreErrors bool
	platform                        string

	procValue  *string
	fileLines  []string
	fileValues map[string]*string
	fileOrder  []string
	fixedLines []string

	changed, setProc, writeFile bool
}

func (s *sysctl) process() *agentproto.Result {
	if s.platform == "freebsd" && s.file != "/etc/sysctl.conf" && s.file != "/etc/sysctl.conf.local" && s.reload {
		return agentproto.Fail("%s can not be reloaded. Set reload=False.", s.file)
	}
	s.procValue = s.currentValue(s.name)
	if fail := s.readFile(); fail != nil {
		return fail
	}
	fv, inFile := s.fileValues[s.name]
	if !inFile {
		s.fileValues[s.name] = nil
	}
	s.fixLines()

	switch {
	case fv == nil && s.state == "present":
		s.changed, s.writeFile = true, true
	case fv == nil && s.state == "absent":
		s.changed = false
	case *fv != "" && s.state == "absent":
		s.changed, s.writeFile = true, true
	case *fv != s.value:
		s.changed, s.writeFile = true, true
	case s.reload:
		if s.procValue == nil || !sysctlValuesEqual(*s.procValue, s.value) {
			s.changed = true
		}
	}
	if s.sysctlSet && s.state == "present" {
		if s.procValue == nil {
			s.changed = true
		} else if !sysctlValuesEqual(*s.procValue, s.value) {
			s.changed, s.setProc = true, true
		}
	}

	if s.env.CheckMode {
		return nil
	}
	if s.setProc {
		if fail := s.setValue(s.name, s.value); fail != nil {
			return fail
		}
	}
	if s.writeFile {
		if fail := s.writeSysctl(); fail != nil {
			return fail
		}
	}
	if s.changed && s.reload {
		return s.reloadSysctl()
	}
	return nil
}

func (s *sysctl) run(cmdline string) (int, string, string) {
	argv, err := shlexSplit(cmdline)
	if err != nil || len(argv) == 0 {
		return 1, "", fmt.Sprintf("%v", err)
	}
	return s.runArgv(argv)
}

func (s *sysctl) runArgv(argv []string) (int, string, string) {
	envC := *s.env
	envC.Env = map[string]string{}
	for k, v := range s.env.Env {
		envC.Env[k] = v
	}
	for k, v := range sysctlLangEnv {
		envC.Env[k] = v
	}
	out, errOut, rc := runCapture(&envC, "", argv...)
	return rc, out, errOut
}

// currentValue is get_token_curr_value: sysctl -e -n <token> (nil on a
// non-zero exit).
func (s *sysctl) currentValue(token string) *string {
	cmd := fmt.Sprintf("%s -e -n %s", s.cmd, token)
	if s.platform == "openbsd" {
		cmd = fmt.Sprintf("%s -n %s", s.cmd, token)
	}
	rc, out, _ := s.run(cmd)
	if rc != 0 {
		return nil
	}
	return &out
}

// setValue is set_token_value: sysctl -w token="value".
func (s *sysctl) setValue(token, value string) *agentproto.Result {
	if len(strings.Fields(value)) > 0 {
		value = `"` + value + `"`
	}
	var cmd string
	switch s.platform {
	case "openbsd":
		cmd = fmt.Sprintf("%s %s=%s", s.cmd, token, value)
	case "freebsd":
		ignore := ""
		if s.ignoreErrors {
			ignore = "-i"
		}
		cmd = fmt.Sprintf("%s %s %s=%s", s.cmd, ignore, token, value)
	default:
		ignore := ""
		if s.ignoreErrors {
			ignore = "-e"
		}
		cmd = fmt.Sprintf("%s %s -w %s=%s", s.cmd, ignore, token, value)
	}
	rc, out, errOut := s.run(cmd)
	if rc != 0 || sysctlStderrFailed.MatchString(errOut) {
		return agentproto.Fail("setting %s failed: %s", token, out+errOut)
	}
	return nil
}

// reloadSysctl is reload_sysctl: sysctl -p <file> (per-key sets on
// OpenBSD, the rc.d service on FreeBSD).
func (s *sysctl) reloadSysctl() *agentproto.Result {
	var rc int
	var out, errOut string
	switch s.platform {
	case "freebsd":
		rc, out, errOut = s.run("/etc/rc.d/sysctl reload")
	case "openbsd":
		for _, k := range s.fileOrder {
			if v := s.fileValues[k]; k != s.name && v != nil {
				if fail := s.setValue(k, *v); fail != nil {
					return fail
				}
			}
		}
		if s.state == "present" {
			return s.setValue(s.name, s.value)
		}
		return nil
	default:
		argv := []string{s.cmd, "-p", s.file}
		if s.ignoreErrors {
			argv = []string{s.cmd, "-e", "-p", s.file}
		}
		rc, out, errOut = s.runArgv(argv)
	}
	if rc != 0 || sysctlStderrFailed.MatchString(errOut) {
		return agentproto.Fail("Failed to reload sysctl: %s", out+errOut)
	}
	return nil
}

// readFile is read_sysctl_file: stripped lines and the key=value map.
func (s *sysctl) readFile() *agentproto.Result {
	if !isFile(s.file) {
		return nil
	}
	data, err := os.ReadFile(s.file)
	if err != nil {
		return agentproto.Fail("Failed to open %s: %s", s.file, pyOSError(err))
	}
	for _, line := range pyReadlines(string(data)) {
		line = strings.TrimSpace(line)
		s.fileLines = append(s.fileLines, line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, seen := s.fileValues[k]; !seen {
			s.fileOrder = append(s.fileOrder, k)
		}
		s.fileValues[k] = &v
	}
	return nil
}

// fixLines is fix_lines: the first occurrence of each key survives,
// normalized to "k=v", with the managed key updated, dropped or appended.
func (s *sysctl) fixLines() {
	var checked []string
	seen := func(k string) bool {
		for _, c := range checked {
			if c == k {
				return true
			}
		}
		return false
	}
	for _, line := range s.fileLines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") || !strings.Contains(line, "=") {
			s.fixedLines = append(s.fixedLines, line)
			continue
		}
		k, v, _ := strings.Cut(t, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if seen(k) {
			continue
		}
		checked = append(checked, k)
		if k == s.name {
			if s.state == "present" {
				s.fixedLines = append(s.fixedLines, k+"="+s.value)
			}
		} else {
			s.fixedLines = append(s.fixedLines, k+"="+v)
		}
	}
	if !seen(s.name) && s.state == "present" {
		s.fixedLines = append(s.fixedLines, s.name+"="+s.value)
	}
}

// writeSysctl is write_sysctl: a temp file next to the real file, then
// atomic_move over it.
func (s *sysctl) writeSysctl() *agentproto.Result {
	real := pyRealpath(s.file)
	tmp, err := os.CreateTemp(filepath.Dir(real), ".ansible_m_sysctl_*.conf")
	if err != nil {
		return moduleCrash(err)
	}
	var b strings.Builder
	for _, l := range s.fixedLines {
		b.WriteString(strings.TrimSpace(l) + "\n")
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return agentproto.Fail("Failed to write to file %s: %s", tmp.Name(), pyOSError(err))
	}
	tmp.Close()
	if err := fsutil.AtomicMove(tmp.Name(), real, true); err != nil {
		os.Remove(tmp.Name())
		return agentproto.Fail("Unable to make %s into to %s, failed final rename from %s: %s", tmp.Name(), real, tmp.Name(), pyOSError(err))
	}
	return nil
}

// sysctlValuesEqual is _values_is_equal: whitespace-split tokens match.
func sysctlValuesEqual(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	if len(fa) != len(fb) {
		return false
	}
	for i := range fa {
		if fa[i] != fb[i] {
			return false
		}
	}
	return true
}

// sysctlStr is the value option's type='str' conversion.
func sysctlStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return pyFloat(t)
	}
	return fmt.Sprintf("%v", v)
}

// sysctlParseValue is _parse_value on the str-converted value: booleans
// become 1/0, anything else is stripped.
func sysctlParseValue(v any) string {
	if v == nil {
		return ""
	}
	s := sysctlStr(v)
	switch strings.ToLower(s) {
	case "y", "yes", "on", "1", "true", "t":
		return "1"
	case "n", "no", "off", "0", "false", "f":
		return "0"
	}
	return strings.TrimSpace(s)
}

// getBinPath is module_utils' get_bin_path: PATH, then the sbin
// directories that exist, first executable file wins.
func getBinPath(arg string) (string, error) {
	return getBinPathIn(os.Getenv("PATH"), arg)
}

// envPATH is the PATH a module process sees: the task environment's, else
// the agent's own.
func envPATH(env *RunEnv) string {
	if p, ok := env.Env["PATH"]; ok {
		return p
	}
	return os.Getenv("PATH")
}

// getBinPathIn is getBinPath over an explicit PATH value.
func getBinPathIn(pathVar, arg string) (string, error) {
	paths := strings.Split(pathVar, ":")
	for _, d := range []string{"/sbin", "/usr/sbin", "/usr/local/sbin"} {
		present := false
		for _, p := range paths {
			if p == d {
				present = true
			}
		}
		if !present && pathExists(d) {
			paths = append(paths, d)
		}
	}
	for _, d := range paths {
		if d == "" {
			continue
		}
		p := pyJoin(d, arg)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("Failed to find required executable %q in paths: %s", arg, strings.Join(paths, ":"))
}

// pyReadlines is file.readlines() in text mode (universal newlines).
func pyReadlines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
