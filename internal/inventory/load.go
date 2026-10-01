package inventory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/template"
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
	// Verbose receives the messages ansible-core's Display shows at a
	// verbosity (-v is 1): the plugins tried on each source, and why.
	Verbose func(level int, msg string)
	// TransformGroupChars is TRANSFORM_INVALID_GROUP_CHARS ("never", the
	// default, "always", "ignore" or "silently"): how the plugins name
	// groups with invalid characters.
	TransformGroupChars string
	// ExtraVars are the run's extra vars (-e), which the constructed and
	// generator plugins can template with (use_extra_vars).
	ExtraVars map[string]any
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
	inv.TransformGroupChars = o.TransformGroupChars
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
	for _, dir := range adjacentDirs {
		inv.varsDirs = append(inv.varsDirs, varsDir{realDir(dir), 0})
	}
	for _, dir := range o.VarsDirs {
		inv.varsDirs = append(inv.varsDirs, varsDir{realDir(dir), 1})
	}
	for _, d := range inv.varsDirs {
		if err := applyVarsDirs(inv, d.dir, d.layer); err != nil {
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

func ignoredRegexp(o Options) *pyre.Pattern {
	// IGNORED: one bytes pattern, searched in each entry's name.
	parts := []string{`^\.`, `^host_vars$`, `^group_vars$`, `^vars_plugins$`}
	parts = append(parts, o.IgnorePatterns...)
	for _, ext := range o.IgnoreExts {
		parts = append(parts, pyre.Escape(ext)+`$`)
	}
	re, err := pyre.CompileBytes([]byte(strings.Join(parts, "|")), 0)
	if err != nil {
		return pyre.MustCompile(`^\.|^host_vars$|^group_vars$|^vars_plugins$`, 0)
	}
	return re
}

type loader struct {
	inv     *Inventory
	o       Options
	ignored *pyre.Pattern
	// processed are the sources parsed so far (InventoryData's
	// processed_sources).
	processed []string
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
			if l.ignored.Search(pyre.Latin1([]byte(name)), 0, -1) != nil {
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
	enabled, err := l.fetchPlugins()
	if err != nil {
		return false, err
	}
	var failures []pluginFailure
	parsed := false
	for _, p := range enabled {
		if !p.plugin.verify(l, src) {
			l.verbose(3, fmt.Sprintf("%s declined parsing %s as it did not pass its verify_file() method", p.loadName, src))
			continue
		}
		var err error
		if ce, ok := l.o.ExtraVarsErr.(*chainError); ok {
			c := *ce // each failure gets its own origin
			err = &c
		} else if err = l.o.ExtraVarsErr; err == nil {
			err = p.plugin.parse(l, src, p.name, p.loadName)
		}
		if err == nil {
			parsed = true
			l.verbose(3, fmt.Sprintf("Parsed %s inventory source with %s plugin", src, p.loadName))
			break
		}
		ce := asChain(err)
		if ce.ctx == "" {
			ce.ctx = fmt.Sprintf("Origin: <inventory plugin %s with source %s>", pyQuote(p.loadName), pyQuote(src))
		}
		failures = append(failures, pluginFailure{p.loadName, ce})
	}
	if parsed {
		l.processed = append(l.processed, src)
	} else if src != DefaultSource || exists(src) {
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

// loadedPlugin is an inventory plugin as the plugin loader returns it:
// the name it was requested by and its _load_name (which messages show).
type loadedPlugin struct {
	plugin         *invPlugin
	name, loadName string
}

// loadPlugin is inventory_loader.get for ansible-core's own plugins: a
// short name, its ansible.builtin FQCN (loaded as the collection's
// Python module) or ansible.legacy name.
func loadPlugin(name string) (loadedPlugin, bool) {
	short, loadName := name, name
	switch {
	case strings.HasPrefix(name, "ansible.builtin."):
		short = strings.TrimPrefix(name, "ansible.builtin.")
		loadName = "ansible_collections.ansible.builtin.plugins.inventory." + short
	case strings.HasPrefix(name, "ansible.legacy."):
		short = strings.TrimPrefix(name, "ansible.legacy.")
		loadName = short
	}
	p, ok := plugins[short]
	if !ok || strings.Contains(short, ".") {
		return loadedPlugin{}, false
	}
	return loadedPlugin{plugin: p, name: name, loadName: loadName}, true
}

// fetchPlugins is InventoryManager._fetch_inventory_plugins.
func (l *loader) fetchPlugins() ([]loadedPlugin, error) {
	var out []loadedPlugin
	for _, name := range l.o.Enabled {
		p, ok := loadPlugin(name)
		if !ok {
			l.inv.warning("Failed to load inventory plugin, skipping " + name)
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("No inventory plugins available to generate inventory, make sure you have at least one enabled.")
	}
	return out, nil
}

// verbose is Display.verbose at a verbosity.
func (l *loader) verbose(level int, msg string) {
	if l.o.Verbose != nil {
		l.o.Verbose(level, msg)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// readable is BaseInventoryPlugin.verify_file: the path exists and can
// be read (said at -vvv when not).
func (l *loader) readable(p string) bool {
	if exists(p) && unix.Access(p, unix.R_OK) == nil {
		return true
	}
	l.verbose(3, "Skipping due to inventory source not existing or not being readable by the current user")
	return false
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

// yamlExtensions is C.YAML_FILENAME_EXTENSIONS.
var yamlExtensions = []string{".yml", ".yaml", ".json"}

type invPlugin struct {
	// verify is the plugin's verify_file.
	verify func(l *loader, src string) bool
	// parse parses src; name is what the plugin was loaded by (a plugin
	// config must name it so) and loadName its _load_name.
	parse func(l *loader, src, name, loadName string) error
}

// isHostList is host_list's and advanced_host_list's verify_file: not a
// path, with a comma.
func isHostList(_ *loader, src string) bool { return !exists(src) && strings.Contains(src, ",") }

// configVerify is the verify_file of the plugins configured by a YAML
// file (constructed, generator).
func configVerify(l *loader, src string) bool {
	if !l.readable(src) {
		return false
	}
	ext := pySplitExt(src)
	return ext == "" || ext == ".config" || slices.Contains(yamlExtensions, ext)
}

var plugins map[string]*invPlugin

func init() {
	plugins = map[string]*invPlugin{
		"host_list":          {verify: isHostList, parse: parseHostList},
		"advanced_host_list": {verify: isHostList, parse: parseAdvancedHostList},
		"script": {
			verify: func(l *loader, src string) bool {
				return l.readable(src) && unix.Access(src, unix.X_OK) == nil
			},
			parse: func(l *loader, src, _, _ string) error { return parseScript(l.inv, src) },
		},
		"auto": {
			verify: func(l *loader, src string) bool {
				return (strings.HasSuffix(src, ".yml") || strings.HasSuffix(src, ".yaml")) && l.readable(src)
			},
			parse: parseAuto,
		},
		"yaml": {
			verify: func(l *loader, src string) bool {
				if !l.readable(src) {
					return false
				}
				ext := pySplitExt(src)
				return ext == "" || slices.Contains(yamlExtensions, ext)
			},
			parse: func(l *loader, src, _, _ string) error {
				data, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				return loadYAMLInventory(l.inv, data, src)
			},
		},
		"ini": {
			verify: func(l *loader, src string) bool { return l.readable(src) && pySplitExt(src) != ".toml" },
			parse: func(l *loader, src, _, _ string) error {
				data, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				return LoadINI(l.inv, data, src)
			},
		},
		"toml": {
			verify: func(l *loader, src string) bool { return l.readable(src) && pySplitExt(src) == ".toml" },
			parse:  parseTOMLInventory,
		},
		"constructed": {verify: configVerify, parse: parseConstructed},
		"generator":   {verify: configVerify, parse: parseGenerator},
	}
}

// parseHostList is the host_list plugin: "h1,h2:2222,".
func parseHostList(l *loader, src, _, _ string) error {
	for _, h := range strings.Split(src, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		host, port, err := parseAddress(h, false)
		if err != nil {
			l.verbose(3, "Unable to parse address from hostname, leaving unchanged: "+err.Error())
			host, port = h, -1
		}
		if _, ok := l.inv.Hosts[host]; !ok {
			l.inv.addHost(host, l.inv.Groups["ungrouped"], port)
		}
	}
	return nil
}

// parseAdvancedHostList is the advanced_host_list plugin: host_list
// with host ranges ("web[1:3],db").
func parseAdvancedHostList(l *loader, src, _, _ string) error {
	for _, h := range strings.Split(src, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		names, port, err := expandHostPattern(h)
		if err != nil {
			if !strings.HasPrefix(err.Error(), "host range must") {
				// A Python error (not an AnsibleError) ends the parse.
				return fmt.Errorf("Invalid data from string, could not parse: %s", err)
			}
			l.verbose(3, "Unable to parse address from hostname, leaving unchanged: "+err.Error())
			names, port = []string{h}, -1
		}
		for _, name := range names {
			if _, ok := l.inv.Hosts[name]; !ok {
				l.inv.addHost(name, l.inv.Groups["ungrouped"], port)
			}
		}
	}
	return nil
}

// parseAuto is the auto plugin: a YAML file naming the inventory plugin
// to run ("plugin: ...").
func parseAuto(l *loader, src, _, _ string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	v, err := yaml.Unmarshal(data, src)
	if err != nil {
		return err
	}
	var name any
	if m, ok := asMapping(v); ok {
		name = m["plugin"]
	}
	if !truthy(name) {
		return fmt.Errorf("no root 'plugin' key found, '%s' is not a valid YAML inventory plugin config file", src)
	}
	s, isStr := name.(string)
	if !isStr {
		return fmt.Errorf("unsupported type %s", typeRepr(name))
	}
	p, ok := loadPlugin(s)
	if !ok {
		return fmt.Errorf("inventory config '%s' specifies unknown plugin '%s'", src, s)
	}
	if p.plugin == plugins["auto"] {
		// auto runs itself until Python's recursion limit.
		return &chainError{msg: "YAML parsing failed: maximum recursion depth exceeded", ctx: "Origin: " + src}
	}
	if !p.plugin.verify(l, src) {
		return fmt.Errorf("inventory source '%s' could not be verified by inventory plugin '%s'", src, s)
	}
	l.verbose(1, fmt.Sprintf("Using inventory plugin '%s' to process inventory source '%s'", p.loadName, src))
	return p.plugin.parse(l, src, p.name, p.loadName)
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

// applyVarsDirs is the host_group_vars vars plugin for one directory:
// each group's and host's files under its group_vars/ and host_vars/
// (layer 0 next to the inventory sources, 1 next to the playbook), found
// as DataLoader.find_vars_files finds them.
func applyVarsDirs(inv *Inventory, dir string, layer int) error {
	groups := make([]string, 0, len(inv.Groups))
	for name := range inv.Groups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	for _, name := range groups {
		if err := applyGroupVarsFiles(inv, inv.Groups[name], dir, layer); err != nil {
			return err
		}
	}
	hosts := make([]string, 0, len(inv.Hosts))
	for name := range inv.Hosts {
		hosts = append(hosts, name)
	}
	sort.Strings(hosts)
	for _, name := range hosts {
		if err := applyHostVarsFiles(inv, inv.Hosts[name], dir, layer); err != nil {
			return err
		}
	}
	return nil
}

// varsDir is a directory the host_group_vars plugin reads group_vars/
// and host_vars/ files from: layer 0 next to the inventory sources, 1
// next to the playbook.
type varsDir struct {
	dir   string
	layer int
}

// realDir is a vars directory's real path, which the plugin works from.
func realDir(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// applyHostVarsFiles merges a host's host_vars files under dir.
func applyHostVarsFiles(inv *Inventory, h *Host, dir string, layer int) error {
	if h.fileVars == nil {
		h.fileVars = map[string]any{}
	}
	return applyVarsFiles(inv.deferWarning, filepath.Join(dir, "host_vars"), h.Name, h.fileVars, &h.FileVarOrigins[layer])
}

// applyGroupVarsFiles merges a group's group_vars files under dir.
func applyGroupVarsFiles(inv *Inventory, g *Group, dir string, layer int) error {
	return applyVarsFiles(inv.deferWarning, filepath.Join(dir, "group_vars"), g.Name, g.Vars, &g.FileVarOrigins[layer])
}

// varsExtensions are YAML_FILENAME_EXTENSIONS, after the bare name.
var varsExtensions = []string{"", ".yml", ".yaml", ".json"}

// applyVarsFiles merges an entity's vars files under dir into vars,
// recording where they name reserved variables; warn reports a
// group_vars or host_vars that is not a directory.
func applyVarsFiles(warn func(string), dir, name string, vars map[string]any, origins *[]template.KeyOrigin) error {
	if strings.HasPrefix(name, "/") {
		return nil // a chroot-like host name
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		warn(fmt.Sprintf("Found %s that is not a directory, skipping: %s", filepath.Base(dir), dir))
		return nil
	}
	for _, path := range findVarsFiles(dir, name) {
		found, err := mergeVarsFile(path, vars)
		if err != nil {
			return err
		}
		for _, o := range found {
			*origins = addOrigin(*origins, o)
		}
	}
	return nil
}

// findVarsFiles is DataLoader.find_vars_files: the first of <name>,
// <name>.yml, <name>.yaml and <name>.json that exists; a directory gives
// its files (recursively, sorted; hidden files, backups and other
// extensions skipped).
func findVarsFiles(dir, name string) []string {
	for _, ext := range varsExtensions {
		full := filepath.Join(dir, name+ext)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			return dirVarsFiles(full)
		}
		return []string{full}
	}
	return nil
}

func dirVarsFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~") {
			continue
		}
		full := filepath.Join(dir, n)
		ext := pySplitExt(n)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		switch {
		case info.IsDir() && ext == "":
			out = append(out, dirVarsFiles(full)...)
		case info.Mode().IsRegular() && (ext == "" || slices.Contains(varsExtensions, ext)):
			out = append(out, full)
		}
	}
	return out
}

// mergeVarsFile merges a vars file into into, returning where it names
// reserved variables (no position for a file that parses as JSON:
// ansible-core loads those as JSON, without origins).
func mergeVarsFile(path string, into map[string]any) ([]template.KeyOrigin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if Decrypt != nil {
		if data, err = Decrypt(data); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	v, err := yaml.Unmarshal(data, absPath(path))
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return nil, fmt.Errorf("%s: vars file must contain a mapping", path)
	}
	if len(m) == 0 {
		return nil, nil // an empty file is ignored
	}
	childOrigins := yaml.ChildOrigins(into)
	if childOrigins == nil {
		childOrigins = map[string]yaml.ChildPos{}
	}
	for k, val := range m {
		into[k] = val
		yaml.MergeChildOrigin(childOrigins, k, m)
	}
	yaml.SetChildOrigins(into, childOrigins)
	var origins []template.KeyOrigin
	if _, jsonErr := omap.UnmarshalJSON(data); jsonErr == nil {
		for _, k := range orderedKeys(v) {
			if template.IsReservedName(k) {
				origins = append(origins, template.KeyOrigin{Name: k})
			}
		}
	} else if node, err := yaml.ParseSingle(data, absPath(path)); err == nil {
		origins = keyOrigins(node, absPath(path))
	}
	return origins, nil
}

// orderedKeys are a mapping's keys in order.
func orderedKeys(v any) []string {
	keys, _, _ := orderedMap(v)
	return keys
}
