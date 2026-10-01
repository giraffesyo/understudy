package modules

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// The setup module reimplements ansible-core's fact collectors (Linux
// first) in pure Go: /proc, /sys and /etc parsing, plus the same handful of
// standard tools Ansible itself shells out to (ip, lsblk, capsh, ...). All
// filesystem access goes through factEnv so the collectors can be pointed
// at a fixture tree in tests.

func init() {
	Register(setupModule, "setup", "gather_facts", "ansible.builtin.setup")
	specs["setup"] = setupSpec
	specs["ansible.builtin.setup"] = setupSpec
}

var setupSpec = args.Spec{
	"gather_subset":  {Type: "list", Default: []any{"all"}},
	"gather_timeout": {Type: "int", Default: int64(10)},
	"filter":         {Type: "list", Default: []any{}},
	"fact_path":      {Default: "/etc/ansible/facts.d"},
}

// minimalGatherSubset is always collected unless explicitly negated
// (ansible-core's setup.py).
var minimalGatherSubset = []string{"apparmor", "caps", "cmdline", "date_time",
	"distribution", "dns", "env", "fips", "local", "lsb", "pkg_mgr", "platform",
	"python", "selinux", "service_mgr", "ssh_pub_keys", "user"}

// factCollector mirrors one ansible-core BaseFactCollector subclass.
type factCollector struct {
	name     string
	ids      []string // _fact_ids: extra gather_subset names selecting it
	requires []string // required_facts: collectors pulled in as dependencies
	// collect returns un-namespaced facts; prior holds the (un-namespaced)
	// facts collected so far, like ansible's collected_facts.
	collect func(e *factEnv, prior map[string]any) map[string]any
	// namespace prefix applied to keys ("" -> "ansible_").
	prefix string
}

// factCollectors in ansible-core's default_collectors order (_base,
// _restrictive, _general, _virtual, _hardware, _network, _extra_facts).
var factCollectors = []*factCollector{
	{name: "platform", ids: []string{"system", "kernel", "kernel_version", "machine",
		"python_version", "architecture", "machine_id"}, collect: collectPlatform},
	{name: "distribution", ids: []string{"distribution_version", "distribution_release",
		"distribution_major_version", "os_family"}, collect: collectDistribution},
	{name: "lsb", collect: collectLSB},
	{name: "selinux", collect: collectSelinux},
	{name: "apparmor", collect: collectApparmor},
	{name: "chroot", ids: []string{"is_chroot"}, collect: collectChroot},
	{name: "fips", collect: collectFips},
	{name: "python", ids: []string{"python_version", "python"}, collect: collectPython},
	{name: "caps", ids: []string{"system_capabilities", "system_capabilities_enforced"}, collect: collectCaps},
	{name: "pkg_mgr", requires: []string{"distribution"}, collect: collectPkgMgr},
	{name: "service_mgr", requires: []string{"platform", "distribution"}, collect: collectServiceMgr},
	{name: "cmdline", collect: collectCmdline},
	{name: "date_time", collect: collectDateTime},
	{name: "env", collect: collectEnv},
	{name: "loadavg", collect: collectLoadavg},
	{name: "ssh_pub_keys", ids: []string{"ssh_host_pub_keys", "ssh_host_key_dsa_public",
		"ssh_host_key_rsa_public", "ssh_host_key_ecdsa_public", "ssh_host_key_ed25519_public"},
		collect: collectSSHPubKeys},
	{name: "user", ids: []string{"user_id", "user_uid", "user_gid", "user_gecos", "user_dir",
		"user_shell", "real_user_id", "effective_user_id", "effective_group_ids"}, collect: collectUser},
	{name: "virtual", ids: []string{"virtualization_type", "virtualization_role",
		"virtualization_tech_guest", "virtualization_tech_host"}, collect: collectVirtual},
	{name: "hardware", ids: []string{"processor", "processor_cores", "processor_count",
		"mounts", "devices"}, requires: []string{"platform"}, collect: collectHardware},
	{name: "dns", collect: collectDNS},
	{name: "fibre_channel_wwn", collect: collectFcWwn},
	{name: "network", ids: []string{"interfaces", "default_ipv4", "default_ipv6",
		"all_ipv4_addresses", "all_ipv6_addresses"}, requires: []string{"platform", "distribution"},
		collect: collectNetwork},
	{name: "iscsi", collect: collectIscsi},
	{name: "nvme", collect: collectNvme},
	{name: "local", collect: collectLocal},
	{name: "facter", collect: collectFacter, prefix: "facter_"},
	{name: "ohai", collect: collectOhai, prefix: "ohai_"},
}

// factEnv is everything the collectors observe about the host. The zero
// root "/" means the live system; tests substitute a fixture tree and fake
// commands.
type factEnv struct {
	root     string
	system   string // platform.system(): "Linux", "Darwin", ...
	uname    func() unameInfo
	run      func(argv ...string) (rc int, stdout, stderr string)
	lookPath func(name string) string // "" when absent (get_bin_path)
	getenv   func(string) string
	environ  func() []string
	now      func() time.Time
	ids      func() (uid, euid, gid, egid int)
	nproc    func() int // len(sched_getaffinity(0))
	statvfs  func(path string) (map[string]any, bool)
	statID   func(path string) (dev, ino uint64, ok bool)
	fsType   func(path string) int64 // statfs f_type, 0 when unknown
	fqdn     func(node string) string

	factPath string
	timeout  time.Duration
	warnings []string

	pyDone bool
	py     *pyInfo
	// python, when set, names the Python that runs the module: the path
	// to run and its sys.executable (else python3 on PATH).
	python func() (run, executable string)
}

type unameInfo struct {
	sysname, nodename, release, version, machine string
}

func newFactEnv(factPath string, timeout time.Duration) *factEnv {
	e := &factEnv{
		root:     "/",
		system:   platformSystem(),
		uname:    sysUname,
		lookPath: func(name string) string { p, _ := lookPath(name); return p },
		getenv:   os.Getenv,
		environ:  os.Environ,
		now:      time.Now,
		ids: func() (int, int, int, int) {
			return os.Getuid(), os.Geteuid(), os.Getgid(), os.Getegid()
		},
		nproc:    runtime.NumCPU,
		statvfs:  sysStatvfs,
		statID:   sysStatID,
		fsType:   sysFsType,
		fqdn:     getFQDN,
		factPath: factPath,
		timeout:  timeout,
	}
	e.run = e.execCommand
	return e
}

func platformSystem() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "Darwin"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "netbsd":
		return "NetBSD"
	}
	return titleCase(runtime.GOOS)
}

// execCommand runs a tool the way AnsibleModule.run_command does for fact
// gathering: no shell, C locale.
func (e *factEnv) execCommand(argv ...string) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cmdTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "LANG=C", "LC_ALL=C", "LC_MESSAGES=C", "LC_NUMERIC=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = agentproto.ExitCode(ee)
		} else {
			return -1, "", err.Error()
		}
	}
	return rc, stdout.String(), stderr.String()
}

func (e *factEnv) cmdTimeout() time.Duration {
	if e.timeout <= 0 {
		return 10 * time.Second
	}
	return e.timeout
}

func (e *factEnv) warn(msg string) { e.warnings = append(e.warnings, msg) }

// p maps an absolute target path into the (possibly fixture) root.
func (e *factEnv) p(abs string) string {
	if e.root == "" || e.root == "/" {
		return abs
	}
	return filepath.Join(e.root, abs)
}

// unroot strips the fixture root from a resolved path.
func (e *factEnv) unroot(p string) string {
	if e.root == "" || e.root == "/" {
		return p
	}
	rel, err := filepath.Rel(e.root, p)
	if err != nil {
		return p
	}
	return "/" + filepath.ToSlash(rel)
}

// fileContent is ansible's get_file_content(path): stripped content, or
// ok=false when the file is missing, unreadable or empty.
func (e *factEnv) fileContent(abs string) (string, bool) {
	data, err := os.ReadFile(e.p(abs))
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	return s, true
}

// fileContentOr returns the stripped content or def.
func (e *factEnv) fileContentOr(abs, def string) string {
	if s, ok := e.fileContent(abs); ok {
		return s
	}
	return def
}

// fileContentAny is get_file_content returning nil (JSON null) when absent.
func (e *factEnv) fileContentAny(abs string) any {
	if s, ok := e.fileContent(abs); ok {
		return s
	}
	return nil
}

// fileLines is get_file_lines: the raw file split into lines.
func (e *factEnv) fileLines(abs string) []string {
	data, err := os.ReadFile(e.p(abs))
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (e *factEnv) exists(abs string) bool {
	_, err := os.Stat(e.p(abs))
	return err == nil
}

func (e *factEnv) isDir(abs string) bool {
	st, err := os.Stat(e.p(abs))
	return err == nil && st.IsDir()
}

func (e *factEnv) isFile(abs string) bool {
	st, err := os.Stat(e.p(abs))
	return err == nil && st.Mode().IsRegular()
}

func (e *factEnv) isLink(abs string) bool {
	st, err := os.Lstat(e.p(abs))
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// listDir is os.listdir: names in directory order (unsorted, like Python).
func (e *factEnv) listDir(abs string) ([]string, error) {
	f, err := os.Open(e.p(abs))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// glob is Python's glob.glob for one trailing "*" component: directory
// order, dotfiles excluded, results as absolute target paths.
func (e *factEnv) glob(dir, pattern string) []string {
	names, err := e.listDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(pattern, ".") {
			continue
		}
		if ok, _ := path.Match(pattern, n); ok {
			out = append(out, path.Join(dir, n))
		}
	}
	return out
}

func (e *factEnv) readlink(abs string) (string, error) {
	return os.Readlink(e.p(abs))
}

// realpath resolves symlinks inside the root and returns a target path.
// realpath is os.path.realpath inside the root: symlinks are resolved
// component by component, and missing components are kept as-is.
func (e *factEnv) realpath(abs string) string {
	pending := strings.Split(strings.TrimPrefix(path.Clean("/"+abs), "/"), "/")
	resolved := "/"
	for hops := 0; len(pending) > 0 && hops < 64; {
		comp := pending[0]
		pending = pending[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, comp)
		target, err := os.Readlink(e.p(next))
		if err != nil {
			resolved = next
			continue
		}
		hops++
		if strings.HasPrefix(target, "/") {
			resolved = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return resolved
}

func (e *factEnv) binPath(name string) string {
	if e.lookPath == nil {
		return ""
	}
	return e.lookPath(name)
}

// setupModule is ansible.builtin.setup.
func setupModule(env *RunEnv, rawArgs map[string]any) *agentproto.Result {
	p, err := setupSpec.Parse(rawArgs)
	if err != nil {
		if strings.HasPrefix(err.Error(), "Unsupported parameters: ") {
			return agentproto.Fail("Unsupported parameters for (setup) module: %s. Supported parameters include: fact_path, filter, gather_subset, gather_timeout.",
				strings.TrimPrefix(err.Error(), "Unsupported parameters: "))
		}
		return agentproto.Fail("%v", err)
	}
	subset := listOfStrings(p.List("gather_subset"))
	filter := listOfStrings(p.List("filter"))
	timeout := time.Duration(p.Int("gather_timeout")) * time.Second

	e := newFactEnv(p.Str("fact_path"), timeout)
	if targetHasPython(env) {
		// The python facts describe the interpreter running the module.
		e.python = func() (string, string) { return targetPython(env).paths[0], targetPythonExecutable(env) }
	}
	facts, err := gatherFacts(e, subset, filter)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	res := &agentproto.Result{AnsibleFacts: facts}
	if len(e.warnings) > 0 {
		w := make([]any, len(e.warnings))
		for i, s := range e.warnings {
			w[i] = s
		}
		res.Extra = map[string]any{"warnings": w}
	}
	return res
}

func listOfStrings(l []any) []string {
	out := make([]string, 0, len(l))
	for _, v := range l {
		switch t := v.(type) {
		case string:
			out = append(out, strings.TrimSpace(t))
		case nil:
		default:
			out = append(out, strings.TrimSpace(toString(t)))
		}
	}
	return out
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return ""
}

// gatherFacts runs the selected collectors and applies namespacing and the
// filter, returning the ansible_facts dict.
func gatherFacts(e *factEnv, subset, filter []string) (map[string]any, error) {
	selected, err := selectCollectors(subset)
	if err != nil {
		return nil, err
	}
	prior := map[string]any{}
	facts := map[string]any{}
	for _, c := range factCollectors {
		if !selected[c.name] {
			continue
		}
		got := c.collect(e, prior)
		prefix := c.prefix
		if prefix == "" {
			prefix = "ansible_"
		}
		for k, v := range got {
			prior[k] = v
			key := prefix + strings.ReplaceAll(k, "-", "_")
			if factFilterMatch(key, filter) {
				facts[key] = v
			}
		}
	}
	// CollectorMetaDataCollector: un-namespaced, but still filtered.
	gs := make([]any, len(subset))
	for i, s := range subset {
		gs[i] = s
	}
	meta := map[string]any{"gather_subset": gs, "module_setup": true}
	for k, v := range meta {
		if factFilterMatch(k, filter) {
			facts[k] = v
		}
	}
	return facts, nil
}

// factFilterMatch is AnsibleFactCollector._filter for one key.
func factFilterMatch(key string, filter []string) bool {
	if len(filter) == 0 || (len(filter) == 1 && filter[0] == "*") {
		return true
	}
	for _, f := range filter {
		if f == "" || pyre.Fnmatch(key, f) {
			return true
		}
		if !strings.HasPrefix(f, "ansible_") && !strings.HasPrefix(f, "facter") {
			if pyre.Fnmatch(key, "ansible_"+f) {
				return true
			}
		}
	}
	return false
}

// selectCollectors implements collector.get_collector_names plus
// dependency resolution: the set of collector names to run.
func selectCollectors(gatherSubset []string) (map[string]bool, error) {
	valid := map[string]bool{}
	aliases := map[string][]string{}
	idToCollectors := map[string][]string{}
	for _, c := range factCollectors {
		valid[c.name] = true
		idToCollectors[c.name] = append(idToCollectors[c.name], c.name)
		for _, id := range c.ids {
			valid[id] = true
			idToCollectors[id] = append(idToCollectors[id], c.name)
		}
		aliases[c.name] = c.ids
	}
	minimal := map[string]bool{}
	for _, m := range minimalGatherSubset {
		minimal[m] = true
	}

	additional := map[string]bool{}
	exclude := map[string]bool{}
	explicit := map[string]bool{}
	for _, subset := range append([]string{"min"}, gatherSubset...) {
		switch subset {
		case "min":
			for m := range minimal {
				additional[m] = true
			}
			continue
		case "all":
			for v := range valid {
				additional[v] = true
			}
			continue
		}
		if name, neg := strings.CutPrefix(subset, "!"); neg {
			switch name {
			case "min":
				for m := range minimal {
					exclude[m] = true
				}
			case "all":
				for v := range valid {
					if !minimal[v] {
						exclude[v] = true
					}
				}
			default:
				for _, a := range aliases[name] {
					exclude[a] = true
				}
				exclude[name] = true
			}
			continue
		}
		if !valid[subset] {
			names := make([]string, 0, len(valid))
			for v := range valid {
				names = append(names, v)
			}
			sort.Strings(names)
			return nil, &subsetError{subset: subset, allowed: names}
		}
		explicit[subset] = true
		additional[subset] = true
	}
	if len(additional) == 0 {
		for v := range valid {
			additional[v] = true
		}
	}
	for x := range exclude {
		if !explicit[x] {
			delete(additional, x)
		}
	}

	selected := map[string]bool{}
	for name := range additional {
		for _, c := range idToCollectors[name] {
			selected[c] = true
		}
	}
	// Pull in required collectors until stable.
	for changed := true; changed; {
		changed = false
		for _, c := range factCollectors {
			if !selected[c.name] {
				continue
			}
			for _, r := range c.requires {
				if !selected[r] {
					selected[r] = true
					changed = true
				}
			}
		}
	}
	return selected, nil
}

type subsetError struct {
	subset  string
	allowed []string
}

func (s *subsetError) Error() string {
	return "Bad subset '" + s.subset + "' given to Ansible. gather_subset options allowed: all, " +
		strings.Join(s.allowed, ", ")
}

// titleCase capitalizes the first letter (strings.Title is deprecated).
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// pyCapitalize is Python's str.capitalize().
func pyCapitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func normalizeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i386"
	}
	return goarch
}
