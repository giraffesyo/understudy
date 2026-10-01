// Package inventory loads Ansible inventories (INI and YAML), applies
// group_vars/host_vars directories, and resolves host patterns.
package inventory

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// Host is one managed host.
type Host struct {
	Name string
	Vars map[string]any
	// VarOrigins are where its variables with reserved names were set.
	VarOrigins []template.KeyOrigin
	// FileVarOrigins are where its host_vars files named reserved
	// variables: [0] next to the inventory sources, [1] next to the
	// playbook.
	FileVarOrigins [2][]template.KeyOrigin
	groups         map[string]*Group
	// fileVars are its host_vars files' variables (next to the inventory
	// sources, then the playbook), over Vars: the vars plugins' layer,
	// which add_host does not see or change.
	fileVars map[string]any

	// implicit marks the implicit localhost: created on demand when a
	// pattern names localhost and the inventory has none. It belongs to no
	// group ("all" does not match it) but takes the all group's vars.
	implicit bool
}

// Implicit reports whether h is the implicit localhost.
func (h *Host) Implicit() bool { return h.implicit }

// Group is a named set of hosts with vars and child groups.
type Group struct {
	Name string
	Vars map[string]any
	// VarOrigins are where its variables with reserved names were set.
	VarOrigins []template.KeyOrigin
	// FileVarOrigins are where its group_vars files named reserved
	// variables: [0] next to the inventory sources, [1] next to the
	// playbook.
	FileVarOrigins [2][]template.KeyOrigin
	Hosts          map[string]*Host
	Children       map[string]*Group
	Parents        map[string]*Group
	depth          int

	// Insertion order, which Ansible's "inventory" host order follows.
	hostOrder  []*Host
	childOrder []*Group
}

// Inventory is the loaded host/group graph. "all" and "ungrouped" always
// exist.
type Inventory struct {
	Hosts  map[string]*Host
	Groups map[string]*Group

	hostOrder  []*Host  // first-seen order
	groupOrder []*Group // creation order (ansible-core's groups dict)

	// varsWarnings wait for the first lookup of host variables.
	varsWarnings []string

	// localhost is the host "localhost" patterns resolve to when the
	// inventory has no host of that name: the first localhost-like host
	// added, else the implicit localhost once created.
	localhost *Host

	// PatternMismatch is HOST_PATTERN_MISMATCH: what a pattern matching
	// nothing does ("warning", the default, "error" or "ignore").
	PatternMismatch string

	// currentSource is the source being parsed (hosts record it as
	// inventory_file).
	currentSource string

	// warn receives warnings raised while building the inventory
	// (group names with invalid characters, conflicting names).
	warn func(string)

	// TransformGroupChars is TRANSFORM_INVALID_GROUP_CHARS ("never", the
	// default, "always", "ignore" or "silently"), as add_host and
	// group_by name the groups they create.
	TransformGroupChars string

	// mu guards the inventory while a run reads it and add_host or
	// group_by change it (each public method takes it).
	mu sync.Mutex
	// dynHosts and dynGroups are the add_host and group_by changes made
	// so far, which a refreshed inventory replays.
	dynHosts  []dynamicHost
	dynGroups []dynamicGroup
	// varsDirs are where the host_group_vars plugin looks for the
	// group_vars/ and host_vars/ files of groups and hosts added later.
	varsDirs []varsDir
	// matchCache holds Match's results by pattern.
	matchCache map[string][]*Host
}

// localhostNames is ansible-core's C.LOCALHOST.
var localhostNames = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}

// New returns an empty inventory with the implicit groups.
func New() *Inventory {
	inv := &Inventory{Hosts: map[string]*Host{}, Groups: map[string]*Group{}}
	all := inv.ensureGroup("all")
	ungrouped := inv.ensureGroup("ungrouped")
	linkGroups(all, ungrouped)
	return inv
}

func (inv *Inventory) warning(msg string) {
	if inv.warn != nil {
		inv.warn(msg)
	}
}

// invalidGroupChars is ansible-core's C.INVALID_VARIABLE_NAMES.
var invalidGroupChars = regexp.MustCompile(`^[\d\W]|[^\w]`)

// ensureGroup is InventoryData.add_group: a new group's name goes
// through to_safe_group_name under TRANSFORM_INVALID_GROUP_CHARS (kept
// with a warning by default, replaced with one or silently, or kept).
func (inv *Inventory) ensureGroup(name string) *Group {
	if g, ok := inv.Groups[name]; ok {
		return g
	}
	name = inv.groupNameFor(name, false)
	if g, ok := inv.Groups[name]; ok {
		return g
	}
	return inv.newGroup(name)
}

// newGroup adds a group named name (which must be new).
func (inv *Inventory) newGroup(name string) *Group {
	g := &Group{
		Name:     name,
		Vars:     map[string]any{},
		Hosts:    map[string]*Host{},
		Children: map[string]*Group{},
		Parents:  map[string]*Group{},
	}
	inv.Groups[name] = g
	inv.groupOrder = append(inv.groupOrder, g)
	return g
}

func (inv *Inventory) ensureHost(name string) *Host {
	if h, ok := inv.Hosts[name]; ok {
		return h
	}
	h := &Host{Name: name, Vars: map[string]any{}, groups: map[string]*Group{}}
	inv.Hosts[name] = h
	inv.hostOrder = append(inv.hostOrder, h)
	if localhostNames[name] {
		// The first localhost-like entry stands in for the implicit one.
		if inv.localhost == nil {
			inv.localhost = h
		} else {
			inv.warning(fmt.Sprintf("A duplicate localhost-like entry was found (%s). First found localhost was %s", name, inv.localhost.Name))
		}
	}
	return h
}

// addChild is InventoryData.add_child for two groups.
func (inv *Inventory) addChild(parent, child *Group) error {
	if parent == child {
		return fmt.Errorf("can't add group to itself")
	}
	if _, ok := parent.Children[child.Name]; ok {
		return nil
	}
	if parent.hasAncestor(child) {
		return fmt.Errorf("Adding group '%s' as child to '%s' creates a recursive dependency loop.", child.Name, parent.Name)
	}
	linkGroups(parent, child)
	return nil
}

// hasAncestor reports whether a is g or one of g's ancestors.
func (g *Group) hasAncestor(a *Group) bool {
	seen := map[*Group]bool{}
	queue := []*Group{g}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == a {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, p := range cur.Parents {
			queue = append(queue, p)
		}
	}
	return false
}

func linkGroups(parent, child *Group) {
	if _, ok := parent.Children[child.Name]; !ok {
		parent.childOrder = append(parent.childOrder, child)
	}
	parent.Children[child.Name] = child
	child.Parents[parent.Name] = parent
}

func addHostToGroup(g *Group, h *Host) {
	if _, ok := g.Hosts[h.Name]; !ok {
		g.hostOrder = append(g.hostOrder, h)
	}
	g.Hosts[h.Name] = h
	h.groups[g.Name] = g
}

func removeHostFromGroup(g *Group, h *Host) {
	if _, ok := g.Hosts[h.Name]; !ok {
		return
	}
	delete(g.Hosts, h.Name)
	delete(h.groups, g.Name)
	for i, o := range g.hostOrder {
		if o == h {
			g.hostOrder = append(g.hostOrder[:i:i], g.hostOrder[i+1:]...)
			break
		}
	}
}

// reconcile is InventoryData.reconcile_inventory, run once after the
// sources when at least one parsed: groups without a parent join "all",
// hosts in no group join "ungrouped", and "ungrouped" loses hosts that
// have a group.
func (inv *Inventory) reconcile() {
	all, ungrouped := inv.Groups["all"], inv.Groups["ungrouped"]
	for _, g := range inv.groupOrder {
		if g != all && len(g.Parents) == 0 {
			linkGroups(all, g)
		}
	}
	inv.closeAncestors()
	for _, h := range inv.hostOrder {
		if _, in := h.groups["ungrouped"]; in {
			for name := range h.groups {
				if name != "all" && name != "ungrouped" {
					removeHostFromGroup(ungrouped, h)
					break
				}
			}
			continue
		}
		others := 0
		for name := range h.groups {
			if name != "all" {
				others++
			}
		}
		if others == 0 {
			addHostToGroup(ungrouped, h)
		}
	}
	var conflicts []string
	for name := range inv.Groups {
		if _, ok := inv.Hosts[name]; ok {
			conflicts = append(conflicts, name)
		}
	}
	sort.Strings(conflicts)
	for _, name := range conflicts {
		inv.warning("Found both group and host with same name: " + name)
	}
}

// closeAncestors makes every host a member of the ancestors of its
// groups, as Host.add_group/populate_ancestors keep it.
func (inv *Inventory) closeAncestors() {
	for _, h := range inv.hostOrder {
		queue := make([]*Group, 0, len(h.groups))
		for _, g := range h.groups {
			queue = append(queue, g)
		}
		for len(queue) > 0 {
			g := queue[0]
			queue = queue[1:]
			for _, parent := range g.Parents {
				if _, ok := h.groups[parent.Name]; !ok {
					h.groups[parent.Name] = parent
					queue = append(queue, parent)
				}
			}
		}
	}
}

// finalize computes ancestor membership and group depths. Call once after
// loading all sources.
func (inv *Inventory) finalize() error {
	inv.closeAncestors()
	// Depth = longest path from "all" (children override parents, so deeper
	// groups must merge later). Iterative relaxation; cycle-guarded.
	for _, g := range inv.Groups {
		g.depth = 0
	}
	changed := true
	for iter := 0; changed; iter++ {
		if iter > len(inv.Groups)+1 {
			return fmt.Errorf("inventory group graph contains a cycle")
		}
		changed = false
		for _, g := range inv.Groups {
			for _, child := range g.Children {
				if child.depth < g.depth+1 {
					child.depth = g.depth + 1
					changed = true
				}
			}
		}
	}
	return nil
}

// GetHost is InventoryData.get_host: the named host, or for a
// localhost-like name absent from the inventory, the implicit localhost
// (created on first use).
func (inv *Inventory) GetHost(name string) *Host {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.getHost(name)
}

func (inv *Inventory) getHost(name string) *Host {
	if h, ok := inv.Hosts[name]; ok {
		return h
	}
	if !localhostNames[name] {
		return nil
	}
	if inv.localhost == nil {
		inv.localhost = &Host{
			Name:     name,
			Vars:     map[string]any{"ansible_connection": "local"},
			groups:   map[string]*Group{},
			implicit: true,
		}
	}
	return inv.localhost
}

// OrderedGroups returns a host's groups in Ansible's var-merge order:
// shallowest first (so deeper, more specific groups override), ties broken
// alphabetically. The implicit localhost takes the all group's vars.
func (inv *Inventory) OrderedGroups(h *Host) []*Group {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.orderedGroups(h)
}

func (inv *Inventory) orderedGroups(h *Host) []*Group {
	if h.implicit {
		return []*Group{inv.Groups["all"]}
	}
	out := make([]*Group, 0, len(h.groups))
	for _, g := range h.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].depth != out[j].depth {
			return out[i].depth < out[j].depth
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// GroupNames returns a host's group names (excluding "all"), sorted — the
// group_names magic variable.
func (inv *Inventory) GroupNames(h *Host) []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := []string{}
	for name := range h.groups {
		if name != "all" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// GroupsMap builds the `groups` magic variable: group name -> host names
// in inventory order, with implicit all/ungrouped included.
func (inv *Inventory) GroupsMap() *yaml.OMap {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := yaml.NewOMap()
	for _, g := range inv.groupOrder {
		if inv.Groups[g.Name] != g {
			continue // a group since removed
		}
		hosts := inv.groupHostNames(g)
		items := make([]any, len(hosts))
		for i, h := range hosts {
			items[i] = h
		}
		out.Set(g.Name, items)
	}
	return out
}

// groupHostNames is Ansible's Group.get_hosts(): the group's own hosts, then
// its descendants' level by level (children in insertion order), each host
// once.
func (inv *Inventory) groupHostNames(g *Group) []string {
	out := []string{}
	seenHost := map[string]bool{}
	seenGroup := map[string]bool{g.Name: true}
	level := []*Group{g}
	for len(level) > 0 {
		var next []*Group
		for _, gr := range level {
			for _, h := range gr.hostOrder {
				if !seenHost[h.Name] {
					seenHost[h.Name] = true
					if g.Name == "all" && h.implicit {
						continue // the all group never lists the implicit localhost
					}
					out = append(out, h.Name)
				}
			}
			for _, child := range gr.childOrder {
				if !seenGroup[child.Name] {
					seenGroup[child.Name] = true
					next = append(next, child)
				}
			}
		}
		level = next
	}
	return out
}

// EffectiveVars merges group vars (depth order) then host vars for one host.
// The caller layers these under play/task/extra vars.
func (inv *Inventory) EffectiveVars(h *Host) map[string]any {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := map[string]any{}
	origins := map[string]yaml.ChildPos{}
	for _, g := range inv.orderedGroups(h) {
		for k, v := range g.Vars {
			out[k] = v
			yaml.MergeChildOrigin(origins, k, g.Vars)
		}
	}
	for k, v := range h.Vars {
		out[k] = v
		yaml.MergeChildOrigin(origins, k, h.Vars)
	}
	for k, v := range h.fileVars {
		out[k] = v
		yaml.MergeChildOrigin(origins, k, h.fileVars)
	}
	// Where each value came from rides along (a broken conditional
	// names it).
	yaml.SetChildOrigins(out, origins)
	return out
}

// Host is the named host (nil when the inventory has none).
func (inv *Inventory) Host(name string) *Host {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.Hosts[name]
}

// HostNames returns all host names in inventory (first-seen) order.
func (inv *Inventory) HostNames() []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := make([]string, len(inv.hostOrder))
	for i, h := range inv.hostOrder {
		out[i] = h.Name
	}
	return out
}

// SortedHostNames returns all host names, sorted.
func (inv *Inventory) SortedHostNames() []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := make([]string, 0, len(inv.Hosts))
	for name := range inv.Hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ListHosts is InventoryManager.list_hosts("all"): the hosts "all"
// matches (never the implicit localhost).
func (inv *Inventory) ListHosts() []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.groupHostNames(inv.Groups["all"])
}

// typeRepr names a loaded value's type as Python's type() repr does for
// ansible-core's tagged values, for messages that print it.
func typeRepr(v any) string {
	switch v.(type) {
	case nil:
		return "<class 'NoneType'>"
	case bool:
		return "<class 'bool'>"
	case int, int64:
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTaggedInt'>"
	case float64:
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTaggedFloat'>"
	case string:
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTaggedStr'>"
	case []any:
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTaggedList'>"
	}
	if _, ok := asMapping(v); ok {
		return "<class 'ansible.module_utils._internal._datatag._AnsibleTaggedDict'>"
	}
	return fmt.Sprintf("<class '%s'>", strings.TrimPrefix(fmt.Sprintf("%T", v), "*"))
}

// addOrigin records where a variable with a reserved name was set; a
// name already set keeps its first origin (a dict keeps its first key).
func addOrigin(origins []template.KeyOrigin, o template.KeyOrigin) []template.KeyOrigin {
	for _, prev := range origins {
		if prev.Name == o.Name {
			return origins
		}
	}
	return append(origins, o)
}

// keyOrigins are where a mapping node names reserved variables.
func keyOrigins(node *yaml.Node, file string) []template.KeyOrigin {
	if node == nil {
		return nil
	}
	var out []template.KeyOrigin
	for _, k := range node.MapKeys() {
		if !template.IsReservedName(k) {
			continue
		}
		o := template.KeyOrigin{Name: k}
		if kn := node.MapKeyNode(k); kn != nil {
			o.File, o.Line, o.Col = file, kn.Line, kn.Column
		}
		out = append(out, o)
	}
	return out
}

// deferWarning keeps a vars plugin's warning for when the variables are
// first looked up (VarsWarnings).
func (inv *Inventory) deferWarning(msg string) {
	if !slices.Contains(inv.varsWarnings, msg) {
		inv.varsWarnings = append(inv.varsWarnings, msg)
	}
}

// VarsWarnings are the warnings the host_group_vars plugin shows when a
// host's variables are first looked up (a group_vars or host_vars that is
// not a directory), each returned once.
func (inv *Inventory) VarsWarnings() []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := inv.varsWarnings
	inv.varsWarnings = nil
	return out
}

// EffectiveVarOrder is the order of EffectiveVars' names as ansible-core's
// get_vars combines a host's inventory variables: the groups' (all first,
// then by depth and name) from the inventory sources, then from their
// group_vars files; the host's own from its source (inventory_file and
// inventory_dir first, as the source added the host), then from its
// host_vars files. Within one source, variables are in written order.
func (inv *Inventory) EffectiveVarOrder(h *Host) []string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	var out []string
	groups := inv.orderedGroups(h)
	for _, fromFiles := range []bool{false, true} {
		for _, g := range groups {
			out = append(out, writtenOrder(g.Vars, fromFiles)...)
		}
	}
	out = append(out, "inventory_file", "inventory_dir")
	out = append(out, writtenOrder(h.Vars, false)...)
	out = append(out, writtenOrder(h.Vars, true)...)
	out = append(out, writtenOrder(h.fileVars, false)...)
	out = append(out, writtenOrder(h.fileVars, true)...)
	return out
}

// writtenOrder is the names of vars from inventory sources (fromFiles:
// from group_vars and host_vars files) in the order they were written;
// those with no known origin count as a source's, after the others, by
// name.
func writtenOrder(vars map[string]any, fromFiles bool) []string {
	origins := yaml.ChildOrigins(vars)
	var known, unknown []string
	for k := range vars {
		o, ok := origins[k]
		isFile := ok && (strings.Contains(o.File, "/group_vars/") || strings.Contains(o.File, "/host_vars/"))
		switch {
		case isFile != fromFiles:
		case ok:
			known = append(known, k)
		default:
			unknown = append(unknown, k)
		}
	}
	sort.Slice(known, func(i, j int) bool {
		a, b := origins[known[i]], origins[known[j]]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		return keyRank(vars, known[i]) < keyRank(vars, known[j])
	})
	sort.Strings(unknown)
	return append(known, unknown...)
}
