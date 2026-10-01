package inventory

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// The plugins configured by a YAML file of options ("plugin: <name>"):
// constructed and generator, with ansible-core's option validation.

// optionDef is one documented plugin option.
type optionDef struct {
	name     string
	typ      string // bool, dict, list, str; "" passes the value through
	required bool
	choices  []string
	def      any
	env      []string
}

var constructedOptions = []optionDef{
	{name: "plugin", required: true, choices: []string{"ansible.builtin.constructed", "constructed"}},
	{name: "use_vars_plugins", typ: "bool", def: false},
	{name: "strict", typ: "bool", def: false},
	{name: "compose", typ: "dict", def: map[string]any{}},
	{name: "groups", typ: "dict", def: map[string]any{}},
	{name: "keyed_groups", typ: "list", def: []any{}},
	{name: "use_extra_vars", typ: "bool", def: false, env: []string{"ANSIBLE_INVENTORY_USE_EXTRA_VARS"}},
	{name: "leading_separator", typ: "bool", def: true},
}

var generatorOptions = []optionDef{
	{name: "plugin", required: true, choices: []string{"ansible.builtin.generator", "generator"}},
	{name: "hosts"},
	{name: "layers"},
	{name: "use_extra_vars", typ: "bool", def: false, env: []string{"ANSIBLE_INVENTORY_USE_EXTRA_VARS", "ANSIBLE_GENERATOR_USE_EXTRA_VARS"}},
}

// pyBoolean is ansible-core's boolean(value, strict=False).
func pyBoolean(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "y", "yes", "on", "1", "true", "t":
			return true
		}
	case int64:
		return t == 1
	case float64:
		return t == 1
	}
	return false
}

// ensureType is the config manager's ensure_type for the option types
// plugin configs use; ok is false for a value of the wrong type.
func ensureType(v any, typ string) (any, bool) {
	if v == nil {
		return nil, true
	}
	switch typ {
	case "bool":
		return pyBoolean(v), true
	case "dict":
		_, ok := asMapping(v)
		return v, ok
	case "list":
		switch t := v.(type) {
		case []any:
			return t, true
		case string:
			var out []any
			for _, item := range strings.Split(t, ",") {
				out = append(out, unquote(strings.TrimSpace(item)))
			}
			return out, true
		}
		return nil, false
	case "str":
		switch v.(type) {
		case string:
			return v, true
		case bool, int64, float64, *big.Int:
			return template.PyStr(v), true
		}
		return nil, false
	}
	return v, true
}

// unquote is ansible-core's config unquote: one pair of matching
// surrounding quotes removed.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// pluginOptions is set_options(direct=config): every option's value from
// the config, the environment or its default, type-checked.
func pluginOptions(loadName string, defs []optionDef, config any) (map[string]any, error) {
	direct, _ := asMapping(config)
	out := map[string]any{}
	for _, d := range defs {
		label := fmt.Sprintf("%s for %s inventory plugin", pyQuote(d.name), pyQuote(loadName))
		value, origin := direct[d.name], "Direct"
		if value == nil {
			for _, name := range d.env {
				if s, ok := os.LookupEnv(name); ok {
					value, origin = s, "env: "+name
					break
				}
			}
		}
		if value == nil {
			if d.required {
				return nil, fmt.Errorf("Required config %s not provided.", label)
			}
			value, origin = d.def, "default"
		}
		typed, ok := ensureType(value, d.typ)
		if !ok {
			if strings.HasPrefix(origin, "env:") && value == "" {
				typed, _ = ensureType(d.def, d.typ)
			} else {
				return nil, &chainError{
					msg:   fmt.Sprintf("Config %s from %s has an invalid value.", label, pyQuote(origin)),
					cause: &chainError{msg: fmt.Sprintf("Invalid value provided for %s: %s", pyQuote(d.typ), template.PyRepr(value))},
				}
			}
		}
		if typed != nil && d.choices != nil {
			s, _ := typed.(string)
			valid := false
			for _, c := range d.choices {
				valid = valid || c == s
			}
			if !valid {
				return nil, &chainError{
					msg:  fmt.Sprintf("Invalid value %s for config %s.", template.PyRepr(typed), label),
					help: "Valid values are: " + strings.Join(d.choices, ", "),
				}
			}
		}
		out[d.name] = typed
	}
	return out, nil
}

// pyClassName is the class name a loaded value's AttributeError names.
func pyClassName(v any) string {
	r := typeRepr(v)
	r = strings.TrimSuffix(strings.TrimPrefix(r, "<class '"), "'>")
	if i := strings.LastIndexByte(r, '.'); i >= 0 {
		r = r[i+1:]
	}
	return r
}

// readConfigData is BaseInventoryPlugin._read_config_data: the config
// file loaded and checked to name the plugin (as it was loaded), then its
// options.
func readConfigData(src, name, loadName string, defs []optionDef) (any, map[string]any, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, nil, err
	}
	config, err := yaml.Unmarshal(data, src)
	if err != nil {
		return nil, nil, &chainError{msg: asChain(err).brief(), hiddenCause: true}
	}
	m, isMap := asMapping(config)
	if !truthy(config) {
		return nil, nil, fmt.Errorf("%s is empty", src)
	}
	if !isMap {
		return nil, nil, fmt.Errorf("'%s' object has no attribute 'get'", pyClassName(config))
	}
	plugin, has := m["plugin"]
	if s, ok := plugin.(string); !ok || s != name {
		shown := "none found"
		if has {
			shown = template.PyStr(plugin)
		}
		return nil, nil, fmt.Errorf("Incorrect plugin name in file: %s", shown)
	}
	opts, err := pluginOptions(loadName, defs, config)
	if err != nil {
		return nil, nil, err
	}
	return config, opts, nil
}

var (
	inventoryEngineOnce sync.Once
	inventoryEngine     *template.Engine
)

// engine is the templar inventory plugins render with.
func engine() *template.Engine {
	inventoryEngineOnce.Do(func() { inventoryEngine = template.New() })
	return inventoryEngine
}

// originCtx is the "Origin: ..." context of a value loaded from YAML.
func originCtx(s string) (string, template.Position) {
	file, line, col, ok := yaml.Origin(s)
	if !ok {
		return "", template.Position{}
	}
	return fmt.Sprintf("Origin: %s:%d:%d\n\n%s", file, line, col, strings.TrimRight(template.SourceExcerpt(file, line, col), "\n")),
		template.Position{File: file, Line: line, Col: col}
}

// templateError is a template failure as ansible-core words it.
func templateError(err error) string {
	if msg, ok := template.Cause(err); ok {
		return msg
	}
	return err.Error()
}

// pyTypeError is TypeError's "Expressions must be <class 'str'>" for a
// non-string expression.
func expressionTypeError(v any) error {
	return fmt.Errorf("Expressions must be <class 'str'>, got %s.", typeRepr(v))
}

// compose is Constructable._compose: an expression evaluated over vars.
func compose(expr any, vars map[string]any) (any, error) {
	s, ok := expr.(string)
	if !ok {
		return nil, expressionTypeError(expr)
	}
	_, pos := originCtx(s)
	return engine().EvalExpression(s, template.MapVars(vars), pos)
}

// evaluateConditional is the templar's evaluate_conditional.
func evaluateConditional(cond any, vars map[string]any) (bool, error) {
	if s, ok := cond.(string); ok {
		cond = strings.TrimSpace(s)
	}
	if cond == nil || cond == "" {
		return false, errors.New("Empty conditional expressions are not allowed.")
	}
	if b, ok := cond.(bool); ok {
		return b, nil
	}
	expr, ok := cond.(string)
	if !ok {
		return false, errors.New("Conditional expressions must be strings.")
	}
	_, pos := originCtx(expr)
	result, err := engine().EvalExpression(expr, template.MapVars(vars), pos)
	if err != nil {
		var ue *template.UndefinedError
		if errors.As(err, &ue) {
			return false, fmt.Errorf("Error while evaluating conditional: %s", templateError(err))
		}
		return false, errors.New(templateError(err))
	}
	if b, ok := result.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("Conditional result (%s) was derived from value of type %s at '<unknown>'. Conditionals must have a boolean result.",
		pyBool(template.Truthy(result)), pyTypeName(result))
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// safeGroupChars is C.INVALID_VARIABLE_NAMES.
var safeGroupChars = regexp.MustCompile(`^[\d\W]|[^\w]`)

// safeGroupName is constructed's to_safe_group_name(force=True,
// silent=True).
func safeGroupName(name string) string {
	return safeGroupChars.ReplaceAllString(name, "_")
}

// groupAncestry returns a host's groups with their ancestors (Host.groups
// as populate_ancestors keeps it) and every group's current depth.
func (inv *Inventory) groupAncestry(h *Host) []*Group {
	seen := map[*Group]bool{}
	var out []*Group
	var visit func(g *Group)
	visit = func(g *Group) {
		if seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
		for _, p := range g.Parents {
			visit(p)
		}
	}
	for _, g := range h.groups {
		visit(g)
	}
	return out
}

// groupDepth is a group's depth as add_child_group keeps it: the longest
// path from a group with no parent.
func groupDepth(g *Group, memo map[*Group]int) int {
	if d, ok := memo[g]; ok {
		return d
	}
	memo[g] = 0
	d := 0
	for _, p := range g.Parents {
		d = max(d, groupDepth(p, memo)+1)
	}
	memo[g] = d
	return d
}

// hostVars is constructed's get_all_host_vars: the host's group vars
// (sort_groups order), then its own vars and magic variables, each with
// the vars plugins' when use_vars_plugins is set.
func (l *loader) hostVars(h *Host, useVarsPlugins bool) (map[string]any, error) {
	groups := l.inv.groupAncestry(h)
	memo := map[*Group]int{}
	sort.SliceStable(groups, func(i, j int) bool {
		di, dj := groupDepth(groups[i], memo), groupDepth(groups[j], memo)
		if di != dj {
			return di < dj
		}
		return groups[i].Name < groups[j].Name
	})
	out := map[string]any{}
	for _, g := range groups {
		for k, v := range g.Vars {
			out[k] = v
		}
	}
	if useVarsPlugins {
		for _, g := range groups {
			if err := l.sourceVars("group_vars", g.Name, out); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range h.Vars {
		out[k] = v
	}
	var names []string
	for _, g := range groups {
		if g.Name != "all" {
			names = append(names, g.Name)
		}
	}
	sort.Strings(names)
	groupNames := make([]any, len(names))
	for i, n := range names {
		groupNames[i] = n
	}
	out["inventory_hostname"] = h.Name
	out["inventory_hostname_short"] = shortHostname(h.Name)
	out["group_names"] = groupNames
	if useVarsPlugins {
		if err := l.sourceVars("host_vars", h.Name, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// shortHostname is inventory_hostname_short: up to the first dot, unless
// the name is an IP address.
func shortHostname(name string) string {
	if ipv4Address.MatchString(name) || ipv6Address.MatchString(name) {
		return name
	}
	short, _, _ := strings.Cut(name, ".")
	return short
}

// sourceVars is get_vars_from_inventory_sources with host_group_vars:
// the group_vars/ or host_vars/ files for name next to each processed
// source, merged into into.
func (l *loader) sourceVars(sub, name string, into map[string]any) error {
	for _, src := range l.processed {
		if strings.Contains(src, ",") && !exists(src) {
			continue
		}
		dir := src
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			dir = filepath.Dir(src)
		}
		target := map[string]any{}
		err := applyVarsDir(l.inv, filepath.Join(dir, sub), func(n string) map[string]any {
			if n == name {
				return target
			}
			return nil
		})
		if err != nil {
			return err
		}
		for k, v := range target {
			into[k] = v
		}
	}
	return nil
}

// withExtraVars layers the run's extra vars over vars (use_extra_vars).
func (l *loader) withExtraVars(vars map[string]any, use bool) map[string]any {
	if !use || len(l.o.ExtraVars) == 0 {
		return vars
	}
	out := make(map[string]any, len(vars)+len(l.o.ExtraVars))
	for k, v := range vars {
		out[k] = v
	}
	for k, v := range l.o.ExtraVars {
		out[k] = v
	}
	return out
}

// parseConstructed is the constructed plugin: variables and groups for
// the hosts already in the inventory, from Jinja2 expressions.
func parseConstructed(l *loader, src, name, loadName string) error {
	_, opts, err := readConfigData(src, name, loadName, constructedOptions)
	if err != nil {
		return err
	}
	if err := l.construct(opts); err != nil {
		return &chainError{msg: fmt.Sprintf("Failed to parse %s.", pyQuote(src)), cause: asChain(err)}
	}
	return nil
}

func (l *loader) construct(opts map[string]any) error {
	strict := opts["strict"].(bool)
	useVarsPlugins := opts["use_vars_plugins"].(bool)
	useExtra := opts["use_extra_vars"].(bool)
	for _, h := range append([]*Host(nil), l.inv.hostOrder...) {
		vars, err := l.hostVars(h, useVarsPlugins)
		if err != nil {
			return err
		}
		if c, ok := asMapping(opts["compose"]); ok {
			for _, varname := range mappingKeys(opts["compose"]) {
				v, err := compose(c[varname], l.withExtraVars(vars, useExtra))
				if err != nil {
					if strict {
						return &chainError{msg: fmt.Sprintf("Could not set %s for host %s: %s", varname, h.Name, templateError(err)), hiddenCause: true}
					}
					continue
				}
				h.Vars[varname] = v
			}
		}
		if vars, err = l.hostVars(h, useVarsPlugins); err != nil {
			return err
		}
		if err := l.composedGroups(opts["groups"], vars, h, strict); err != nil {
			return err
		}
		if err := l.keyedGroups(opts, l.withExtraVars(vars, useExtra), h, strict); err != nil {
			return err
		}
	}
	return nil
}

// composedGroups is _add_host_to_composed_groups.
func (l *loader) composedGroups(groups any, vars map[string]any, h *Host, strict bool) error {
	m, ok := asMapping(groups)
	if !ok {
		return nil
	}
	for _, raw := range mappingKeys(groups) {
		name := safeGroupName(raw)
		result, err := evaluateConditional(m[raw], vars)
		if err != nil {
			if strict {
				return &chainError{msg: fmt.Sprintf("Could not add host %s to group %s: %s", h.Name, name, err), hiddenCause: true}
			}
			continue
		}
		if result {
			addHostToGroup(l.inv.ensureGroup(name), h)
		}
	}
	return nil
}

// isDefaultable reports whether a value is None or the empty string
// (what keyed groups replace with default_value).
func isDefaultable(v any) bool { return v == nil || v == "" }

// keyedGroups is _add_host_to_keyed_groups.
func (l *loader) keyedGroups(opts map[string]any, vars map[string]any, h *Host, strict bool) error {
	keys, ok := opts["keyed_groups"].([]any)
	if !ok {
		return nil
	}
	leading := opts["leading_separator"].(bool)
	for _, entry := range keys {
		keyed, isMap := asMapping(entry)
		if !isMap || len(keyed) == 0 {
			return fmt.Errorf("Invalid keyed group entry, it must be a dictionary: %s", template.PyStr(entry))
		}
		key, err := compose(keyed["key"], vars)
		if err != nil {
			if strict {
				return &chainError{msg: fmt.Sprintf("Could not generate group for host %s from %s entry: %s", h.Name, template.PyStr(keyed["key"]), templateError(err)), hiddenCause: true}
			}
			continue
		}
		defaultValue, hasDefault := keyed["default_value"]
		hasDefault = hasDefault && defaultValue != nil
		trailing, hasTrailing := keyed["trailing_separator"]
		hasTrailing = hasTrailing && trailing != nil
		if hasTrailing && hasDefault {
			return errors.New("parameters are mutually exclusive for keyed groups: default_value|trailing_separator")
		}
		useDefault := isDefaultable(key) && hasDefault
		if !template.Truthy(key) && !useDefault {
			if strict && !isEmptyContainer(key) {
				return fmt.Errorf("No key or key resulted empty for %s in host %s, invalid entry", template.PyStr(keyed["key"]), h.Name)
			}
			continue
		}
		prefix, sep := any(""), any("_")
		if v, ok := keyed["prefix"]; ok {
			prefix = v
		}
		if v, ok := keyed["separator"]; ok {
			sep = v
		}
		parent := keyed["parent_group"]
		if s, ok := parent.(string); ok {
			_, pos := originCtx(s)
			rendered, err := engine().RenderTemplate(s, template.MapVars(vars), pos)
			if err != nil {
				if strict {
					return &chainError{msg: fmt.Sprintf("Could not generate parent group %s for group %s: %s", template.PyRepr(s), template.PyRepr(key), templateError(err)), hiddenCause: true}
				}
				continue
			}
			if _, omitted := rendered.(template.Omit); omitted {
				rendered = nil
			}
			parent = rendered
		}
		str := template.PyStr
		var names []string
		switch k := key.(type) {
		case string:
			if useDefault {
				names = append(names, str(defaultValue))
			} else {
				names = append(names, k)
			}
		case []any:
			for _, item := range k {
				if isDefaultable(item) && hasDefault {
					names = append(names, str(defaultValue))
				} else {
					names = append(names, str(item))
				}
			}
		default:
			km, isMapping := asMapping(key)
			switch {
			case useDefault:
				names = append(names, str(defaultValue))
			case isMapping:
				for _, gname := range mappingKeys(key) {
					gval := km[gname]
					bare := gname + str(sep) + str(gval)
					if isDefaultable(gval) {
						if hasDefault {
							bare = gname + str(sep) + str(defaultValue)
						} else if trailing == false {
							bare = gname
						}
					}
					names = append(names, bare)
				}
			default:
				return fmt.Errorf("Invalid group name format, expected a string or a list of them or dictionary, got: %s", typeRepr(key))
			}
		}
		for _, bare := range names {
			if prefix == "" && !leading {
				sep = ""
			}
			g := l.inv.ensureGroup(safeGroupName(str(prefix) + str(sep) + bare))
			addHostToGroup(g, h)
			if template.Truthy(parent) {
				pg := l.inv.ensureGroup(safeGroupName(str(parent)))
				if err := l.inv.addChild(pg, g); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// isEmptyContainer is `key in ([], {})`.
func isEmptyContainer(v any) bool {
	if l, ok := v.([]any); ok {
		return len(l) == 0
	}
	if m, ok := asMapping(v); ok {
		return len(m) == 0
	}
	return false
}

// parseGenerator is the generator plugin: a host for every combination
// of the layers' values, with templated names and parent groups.
func parseGenerator(l *loader, src, name, loadName string) error {
	config, opts, err := readConfigData(src, name, loadName, generatorOptions)
	if err != nil {
		return err
	}
	cfg, _ := asMapping(config)
	extra := l.withExtraVars(map[string]any{}, opts["use_extra_vars"].(bool))
	layersVal, ok := cfg["layers"]
	if !ok {
		return errors.New("'layers'")
	}
	if _, ok := asMapping(layersVal); !ok {
		return fmt.Errorf("'%s' object has no attribute 'values'", pyClassName(layersVal))
	}
	layerNames := mappingKeys(layersVal)
	layers, _ := asMapping(layersVal)
	values := make([][]any, len(layerNames))
	for i, n := range layerNames {
		items, err := pyIterate(layers[n])
		if err != nil {
			return err
		}
		values[i] = items
	}
	for _, combo := range product(values) {
		vars := map[string]any{}
		for k, v := range extra {
			vars[k] = v
		}
		for i, n := range layerNames {
			vars[n] = combo[i]
		}
		hostsVal, ok := cfg["hosts"]
		if !ok {
			return errors.New("'hosts'")
		}
		hosts, ok := asMapping(hostsVal)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyClassName(hostsVal))
		}
		if _, ok := hosts["name"]; !ok {
			return errors.New("'name'")
		}
		host, err := generatorTemplate(hosts["name"], vars)
		if err != nil {
			return err
		}
		hostName, err := checkName(host, "host")
		if err != nil {
			return err
		}
		if _, ok := l.inv.Hosts[hostName]; !ok {
			l.inv.addHost(hostName, nil, -1)
		}
		parents := hosts["parents"]
		if parents == nil {
			parents = []any{}
		}
		if err := l.addGeneratorParents(hostName, parents, vars); err != nil {
			return err
		}
	}
	return nil
}

// checkName is InventoryData's check that a host or group name is a
// non-empty string.
func checkName(v any, kind string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("Invalid %s name supplied, expected a string but got %s for %s", kind, typeRepr(v), template.PyStr(v))
	}
	if s == "" {
		if kind == "host" {
			return "", errors.New("Invalid empty host name provided: ")
		}
		return "", errors.New("Invalid empty/false group name provided: ")
	}
	return s, nil
}

// generatorTemplate is the generator's template(): strings render,
// anything else passes through.
func generatorTemplate(v any, vars map[string]any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	ctx, pos := originCtx(s)
	out, err := engine().RenderTemplate(s, template.MapVars(vars), pos)
	if err != nil {
		return nil, &chainError{msg: templateError(err), ctx: ctx}
	}
	return out, nil
}

// addGeneratorParents is the generator's add_parents.
func (l *loader) addGeneratorParents(child string, parents any, vars map[string]any) error {
	items, err := pyIterate(parents)
	if err != nil {
		return err
	}
	for _, p := range items {
		pm, ok := asMapping(p)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyClassName(p))
		}
		gname, err := generatorTemplate(pm["name"], vars)
		if err != nil {
			return err
		}
		if !template.Truthy(gname) {
			return fmt.Errorf("Element %s has a parent with no name.", child)
		}
		groupName, err := checkName(gname, "group")
		if err != nil {
			return err
		}
		g := l.inv.ensureGroup(groupName)
		if gv := pm["vars"]; gv != nil {
			vm, ok := asMapping(gv)
			if !ok {
				return fmt.Errorf("'%s' object has no attribute 'items'", pyClassName(gv))
			}
			for _, k := range mappingKeys(gv) {
				v, err := generatorTemplate(vm[k], vars)
				if err != nil {
					return err
				}
				g.Vars[k] = v
			}
		}
		if cg, ok := l.inv.Groups[child]; ok {
			if err := l.inv.addChild(g, cg); err != nil {
				return err
			}
		} else {
			addHostToGroup(g, l.inv.Hosts[child])
		}
		grand := pm["parents"]
		if grand == nil {
			grand = []any{}
		}
		if err := l.addGeneratorParents(groupName, grand, vars); err != nil {
			return err
		}
	}
	return nil
}

// pyIterate is iter() over a loaded value: a list's items, a mapping's
// keys, a string's characters.
func pyIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	}
	if _, ok := asMapping(v); ok {
		var out []any
		for _, k := range mappingKeys(v) {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyClassName(v))
}

// product is itertools.product.
func product(lists [][]any) [][]any {
	out := [][]any{{}}
	for _, l := range lists {
		var next [][]any
		for _, prefix := range out {
			for _, item := range l {
				combo := append(append([]any(nil), prefix...), item)
				next = append(next, combo)
			}
		}
		out = next
	}
	return out
}
