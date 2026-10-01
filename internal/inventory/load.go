package inventory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Decrypt transparently decrypts vault-encrypted group_vars/host_vars
// files. Set before Load when a vault password is available; nil leaves
// files untouched (an encrypted file then fails to parse with a clear
// vault error).
var Decrypt func([]byte) ([]byte, error)

// Options are ansible-core's inventory settings.
type Options struct {
	// VarsDirs are applied after the inventory-adjacent group_vars/ and
	// host_vars/ (e.g. the playbook directory), later winning.
	VarsDirs []string
	// Enabled is INVENTORY_ENABLED: the plugins tried on each source, in
	// order. Empty means the default.
	Enabled []string
	// IgnoreExts and IgnorePatterns skip directory entries
	// (INVENTORY_IGNORE_EXTS, INVENTORY_IGNORE_PATTERNS); nil IgnoreExts
	// means the default.
	IgnoreExts     []string
	IgnorePatterns []string
	// UnparsedWarning (INVENTORY_UNPARSED_WARNING) warns when no source
	// parsed; UnparsedIsFailed and AnyUnparsedIsFailed
	// (INVENTORY_UNPARSED_IS_FAILED, INVENTORY_ANY_UNPARSED_IS_FAILED) make
	// that, or any source failing, an error.
	UnparsedWarning     bool
	UnparsedIsFailed    bool
	AnyUnparsedIsFailed bool
	// ExtraVarsErr is the error loading the extra vars failed with: every
	// plugin that takes on a source loads them first (load_extra_vars in
	// BaseInventoryPlugin.parse), so each fails with it.
	ExtraVarsErr error
	// Warn receives each warning as ansible-core's Display formats it
	// (without the "[WARNING]: " prefix; ending in a newline, two after a
	// multi-line message).
	Warn func(string)
}

// DefaultEnabled is ansible-core's INVENTORY_ENABLED default.
var DefaultEnabled = []string{"host_list", "script", "auto", "yaml", "ini", "toml"}

// DefaultIgnoreExts is ansible-core's INVENTORY_IGNORE_EXTS default.
var DefaultIgnoreExts = []string{".pyc", ".pyo", ".swp", ".bak", "~", ".rpm", ".md", ".txt", ".rst", ".orig", ".cfg", ".retry"}

// DefaultSource is ansible-core's DEFAULT_HOST_LIST: missing, it parses
// silently to nothing.
const DefaultSource = "/etc/ansible/hosts"

// Load builds an inventory from -i sources with the default settings and
// no warnings. See LoadWith.
func Load(sources []string, varsDirs []string) (*Inventory, error) {
	return LoadWith(sources, Options{VarsDirs: varsDirs})
}

// LoadWith builds an inventory as ansible-core's InventoryManager does:
// each source (a file, a directory of them, or a "h1,h2" host list) is
// offered to the enabled inventory plugins in turn until one parses it; a
// source none can parse is reported with each plugin's failure and
// skipped. With nothing parsed, only the implicit localhost exists.
// group_vars/ and host_vars/ next to sources are applied, then any
// VarsDirs in order — later wins.
func LoadWith(sources []string, o Options) (*Inventory, error) {
	inv := New()
	inv.warn = func(msg string) {
		if o.Warn != nil {
			o.Warn(msg + "\n")
		}
	}
	if len(o.Enabled) == 0 {
		o.Enabled = DefaultEnabled
	}
	if o.IgnoreExts == nil {
		o.IgnoreExts = DefaultIgnoreExts
	}
	l := &loader{inv: inv, o: o, ignored: ignoredRegexp(o)}
	parsed := false
	var adjacentDirs []string
	for _, src := range sources {
		if src == "" {
			continue
		}
		if !strings.Contains(src, ",") {
			src = unfrackPath(src)
		}
		ok, err := l.parseSource(src)
		if err != nil {
			return nil, err
		}
		parsed = parsed || ok
		// The vars plugins look next to every source, parsed or not.
		if info, err := os.Stat(src); err == nil && info.IsDir() {
			adjacentDirs = append(adjacentDirs, src)
		} else if err == nil || !strings.Contains(src, ",") {
			adjacentDirs = append(adjacentDirs, filepath.Dir(src))
		}
	}
	if parsed {
		inv.reconcile()
	} else if o.UnparsedIsFailed {
		return nil, errors.New("No inventory was parsed, please check your configuration and options.")
	} else if o.UnparsedWarning {
		inv.warning("No inventory was parsed, only implicit localhost is available")
	}

	if err := inv.finalize(); err != nil {
		return nil, err
	}

	// vars directories: inventory-adjacent first, then explicit (playbook)
	// dirs — later application wins on key conflicts.
	for _, dir := range append(adjacentDirs, o.VarsDirs...) {
		if err := applyVarsDirs(inv, dir); err != nil {
			return nil, err
		}
	}
	return inv, nil
}

// unfrackPath is ansible-core's unfrackpath(follow=False): ~ and $VARS
// expanded, made absolute and normalized, symlinks kept.
func unfrackPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	p = os.ExpandEnv(p)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func ignoredRegexp(o Options) *regexp.Regexp {
	parts := []string{`^\.`, `^host_vars$`, `^group_vars$`, `^vars_plugins$`}
	parts = append(parts, o.IgnorePatterns...)
	for _, ext := range o.IgnoreExts {
		parts = append(parts, regexp.QuoteMeta(ext)+`$`)
	}
	re, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return regexp.MustCompile(`^\.|^host_vars$|^group_vars$|^vars_plugins$`)
	}
	return re
}

type loader struct {
	inv     *Inventory
	o       Options
	ignored *regexp.Regexp
}

type pluginFailure struct {
	plugin string
	err    *chainError
}

// parseSource is InventoryManager.parse_source.
func (l *loader) parseSource(src string) (bool, error) {
	if info, err := os.Stat(src); err == nil && info.IsDir() {
		entries, err := os.ReadDir(src)
		if err != nil {
			return false, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		parsed := false
		for _, name := range names {
			if l.ignored.MatchString(name) {
				continue
			}
			ok, err := l.parseSource(filepath.Join(src, name))
			if err != nil {
				return false, err
			}
			parsed = parsed || ok
		}
		return parsed, nil
	}

	l.inv.currentSource = src
	defer func() { l.inv.currentSource = "" }()
	var failures []pluginFailure
	parsed := false
	for _, name := range l.o.Enabled {
		p, ok := plugins[name]
		if !ok {
			l.inv.warning("Failed to load inventory plugin, skipping " + name)
			continue
		}
		if !p.verify(src) {
			continue
		}
		var err error
		if ce, ok := l.o.ExtraVarsErr.(*chainError); ok {
			c := *ce // each failure gets its own origin
			err = &c
		} else if err = l.o.ExtraVarsErr; err == nil {
			err = p.parse(l.inv, src)
		}
		if err == nil {
			parsed = true
			break
		}
		ce := asChain(err)
		if ce.ctx == "" {
			ce.ctx = fmt.Sprintf("Origin: <inventory plugin %s with source %s>", pyQuote(name), pyQuote(src))
		}
		failures = append(failures, pluginFailure{name, ce})
	}
	if !parsed && (src != DefaultSource || exists(src)) {
		for _, f := range failures {
			l.inv.warnEvent(&chainError{msg: fmt.Sprintf("Failed to parse inventory with %s plugin.", pyQuote(f.plugin)), cause: f.err})
		}
		if l.o.AnyUnparsedIsFailed {
			return false, fmt.Errorf("Completely failed to parse inventory source %s", src)
		}
		l.inv.warning(fmt.Sprintf("Unable to parse %s as an inventory source", src))
	}
	return parsed, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// readable is BaseInventoryPlugin.verify_file: the path exists and can
// be read.
func readable(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// pySplitExt is Python's os.path.splitext extension (leading dots of the
// base name do not start one).
func pySplitExt(p string) string {
	base := filepath.Base(p)
	trimmed := strings.TrimLeft(base, ".")
	i := strings.LastIndexByte(trimmed, '.')
	if i < 0 {
		return ""
	}
	return trimmed[i:]
}

type invPlugin struct {
	verify func(src string) bool
	parse  func(inv *Inventory, src string) error
}

var plugins = map[string]invPlugin{
	"host_list": {
		verify: func(src string) bool { return !exists(src) && strings.Contains(src, ",") },
		parse:  parseHostList,
	},
	"script": {
		verify: func(src string) bool {
			info, err := os.Stat(src)
			return err == nil && readable(src) && info.Mode()&0o111 != 0 && !info.IsDir()
		},
		parse: parseScript,
	},
	"auto": {
		verify: func(src string) bool {
			return (strings.HasSuffix(src, ".yml") || strings.HasSuffix(src, ".yaml")) && readable(src)
		},
		// parse is set in init (it dispatches through plugins).
	},
	"yaml": {
		verify: func(src string) bool {
			switch pySplitExt(src) {
			case "", ".yaml", ".yml", ".json":
				return readable(src)
			}
			return false
		},
		parse: func(inv *Inventory, src string) error {
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return loadYAMLInventory(inv, data, src)
		},
	},
	"ini": {
		verify: func(src string) bool { return readable(src) && pySplitExt(src) != ".toml" },
		parse: func(inv *Inventory, src string) error {
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return LoadINI(inv, data, src)
		},
	},
	"toml": {
		verify: func(src string) bool { return readable(src) && pySplitExt(src) == ".toml" },
		parse: func(inv *Inventory, src string) error {
			return errors.New("TOML inventory sources are not supported by understudy")
		},
	},
}

func init() {
	auto := plugins["auto"]
	auto.parse = parseAuto
	plugins["auto"] = auto
}

// parseHostList is the host_list plugin: "h1,h2:2222,".
func parseHostList(inv *Inventory, src string) error {
	for _, h := range strings.Split(src, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		host, port, err := parseAddress(h, false)
		if err != nil {
			host, port = h, -1
		}
		if _, ok := inv.Hosts[host]; !ok {
			inv.addHost(host, inv.Groups["ungrouped"], port)
		}
	}
	return nil
}

// builtinPlugins are ansible-core's own inventory plugins; the auto
// plugin hands a config naming one of these to it.
var builtinPlugins = map[string]bool{"advanced_host_list": true, "auto": true, "constructed": true,
	"generator": true, "host_list": true, "ini": true, "script": true, "toml": true, "yaml": true}

// parseAuto is the auto plugin: a YAML file naming the inventory plugin
// to run ("plugin: ...").
func parseAuto(inv *Inventory, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	v, err := yaml.Unmarshal(data, src)
	if err != nil {
		return err
	}
	name := ""
	if m, ok := asMapping(v); ok {
		if s, ok := m["plugin"].(string); ok {
			name = s
		} else if m["plugin"] != nil && m["plugin"] != false {
			name = fmt.Sprint(m["plugin"])
		}
	}
	if name == "" {
		return fmt.Errorf("no root 'plugin' key found, '%s' is not a valid YAML inventory plugin config file", src)
	}
	short := strings.TrimPrefix(strings.TrimPrefix(name, "ansible.builtin."), "ansible.legacy.")
	if p, ok := plugins[short]; ok && short != "auto" {
		if !p.verify(src) {
			return fmt.Errorf("inventory source '%s' could not be verified by inventory plugin '%s'", src, name)
		}
		return p.parse(inv, src)
	}
	if builtinPlugins[short] {
		return fmt.Errorf("inventory config '%s' specifies the '%s' plugin, which understudy does not implement", src, name)
	}
	return fmt.Errorf("inventory config '%s' specifies unknown plugin '%s'", src, name)
}

// parseScript is the script plugin: an executable printing the inventory
// as JSON for --list.
func parseScript(inv *Inventory, src string) error {
	out, err := runInventoryScript(src, "--list")
	if err != nil {
		return err
	}
	data, err := yaml.Unmarshal(out, src)
	if err != nil {
		return &chainError{msg: "Inventory script result could not be parsed as JSON.", cause: asChain(err)}
	}
	top, ok := asMapping(data)
	if !ok {
		return errors.New("Inventory script result could not be parsed as JSON.")
	}
	keys := mappingKeys(data)
	var meta map[string]any
	var hosts []string
	seen := map[string]bool{}
	for _, group := range keys {
		gdata := top[group]
		if group == "_meta" {
			m, _ := asMapping(gdata)
			hv, ok := asMapping(m["hostvars"])
			if !ok {
				return fmt.Errorf("Value contains '_meta.hostvars' which is %s instead of 'dict'.", pyTypeName(m["hostvars"]))
			}
			meta = hv
			continue
		}
		g := inv.ensureGroup(group)
		gm, isMap := asMapping(gdata)
		if !isMap {
			gm = map[string]any{"hosts": gdata}
		} else if gm["hosts"] == nil && gm["vars"] == nil && gm["children"] == nil {
			gm = map[string]any{"hosts": []any{group}, "vars": gdata}
		}
		if hs, ok := gm["hosts"]; ok {
			list, ok := hs.([]any)
			if !ok {
				return fmt.Errorf("Value contains '%s.hosts' which is %s instead of 'list'.", group, pyTypeName(hs))
			}
			for _, h := range list {
				name := fmt.Sprint(h)
				if !seen[name] {
					seen[name] = true
					hosts = append(hosts, name)
				}
				inv.addHost(name, g, -1)
			}
		}
		if vs, ok := gm["vars"]; ok {
			vm, ok := asMapping(vs)
			if !ok {
				return fmt.Errorf("Value contains '%s.vars' which is %s instead of 'dict'.", group, pyTypeName(vs))
			}
			for _, k := range mappingKeys(vs) {
				g.Vars[k] = vm[k]
			}
		}
		if cs, ok := gm["children"].([]any); ok {
			for _, c := range cs {
				if err := inv.addChild(g, inv.ensureGroup(fmt.Sprint(c))); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range hosts {
		var vars any
		if meta != nil {
			vars = meta[name]
		} else {
			out, err := runInventoryScript(src, "--host", name)
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(out)) > 0 {
				if vars, err = yaml.Unmarshal(out, src); err != nil {
					return &chainError{msg: fmt.Sprintf("Inventory script result for host %s could not be parsed as JSON.", pyQuote(name)), cause: asChain(err)}
				}
			}
		}
		if vars == nil {
			continue
		}
		vm, ok := asMapping(vars)
		if !ok {
			return fmt.Errorf("Invalid data from file, expected dictionary and got:\n\n%v", vars)
		}
		h := inv.Hosts[name]
		for _, k := range mappingKeys(vars) {
			h.Vars[k] = vm[k]
		}
	}
	return nil
}

func runInventoryScript(src string, args ...string) ([]byte, error) {
	cmd := exec.Command(src, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	help := ""
	if s := stderr.String(); strings.TrimSpace(s) != "" {
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		help = "Standard error from inventory script:\n" + s
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return nil, &chainError{msg: fmt.Sprintf("Inventory script returned non-zero exit code %d.", exitErr.ExitCode()), help: help}
	case err != nil:
		return nil, &chainError{
			msg:   fmt.Sprintf("Failed to execute inventory script command %s.", pyQuote(shellJoin(append([]string{src}, args...)))),
			cause: &chainError{msg: osErrorStr(err, src)},
		}
	}
	return stdout.Bytes(), nil
}

// osErrorStr is str() of the OSError subprocess raises for a failed exec.
func osErrorStr(err error, path string) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "exec format error"):
		return fmt.Sprintf("[Errno 8] Exec format error: %s", pyQuote(path))
	case errors.Is(err, os.ErrPermission):
		return fmt.Sprintf("[Errno 13] Permission denied: %s", pyQuote(path))
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("[Errno 2] No such file or directory: %s", pyQuote(path))
	}
	return msg
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// shellJoin is Python's shlex.join.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && shellSafe.MatchString(a) {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// applyVarsDirs loads group_vars/ and host_vars/ under dir. Both file
// (group_vars/web.yml, bare group_vars/web) and directory
// (group_vars/web/*.yml) forms are supported.
func applyVarsDirs(inv *Inventory, dir string) error {
	if err := applyVarsDir(inv, filepath.Join(dir, "group_vars"), func(name string) map[string]any {
		if g, ok := inv.Groups[name]; ok {
			return g.Vars
		}
		return nil
	}); err != nil {
		return err
	}
	return applyVarsDir(inv, filepath.Join(dir, "host_vars"), func(name string) map[string]any {
		if h, ok := inv.Hosts[name]; ok {
			return h.Vars
		}
		return nil
	})
}

func applyVarsDir(inv *Inventory, dir string, lookup func(string) map[string]any) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml")
		target := lookup(base)
		if target == nil {
			continue // vars for a group/host not in this inventory
		}
		if info.IsDir() {
			subEntries, err := os.ReadDir(full)
			if err != nil {
				return err
			}
			var subNames []string
			for _, se := range subEntries {
				if !se.IsDir() && (strings.HasSuffix(se.Name(), ".yml") || strings.HasSuffix(se.Name(), ".yaml")) {
					subNames = append(subNames, se.Name())
				}
			}
			sort.Strings(subNames)
			for _, sn := range subNames {
				if err := mergeVarsFile(filepath.Join(full, sn), target); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if err := mergeVarsFile(full, target); err != nil {
			return err
		}
	}
	return nil
}

func mergeVarsFile(path string, into map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if Decrypt != nil {
		if data, err = Decrypt(data); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	v, err := yaml.Unmarshal(data, absPath(path))
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return fmt.Errorf("%s: vars file must contain a mapping", path)
	}
	for k, val := range m {
		into[k] = val
	}
	return nil
}
