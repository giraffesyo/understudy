// Package inventory loads Ansible inventories (INI and YAML), applies
// group_vars/host_vars directories, and resolves host patterns.
package inventory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/template"
)

// Host is one managed host.
type Host struct {
	Name string
	Vars map[string]any
	// VarOrigins are where its variables with reserved names were set.
	VarOrigins []template.KeyOrigin
	groups     map[string]*Group

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
	Hosts      map[string]*Host
	Children   map[string]*Group
	Parents    map[string]*Group
	depth      int

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

func (inv *Inventory) ensureGroup(name string) *Group {
	if g, ok := inv.Groups[name]; ok {
		return g
	}
	// to_safe_group_name with TRANSFORM_INVALID_GROUP_CHARS at its
	// default ("never"): the name is kept, with a warning.
	if invalidGroupChars.MatchString(name) {
		inv.warning("Invalid characters were found in group names but not replaced, use -vvvv to see details")
	}
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
func (inv *Inventory) GroupsMap() map[string]any {
	out := make(map[string]any, len(inv.Groups))
	for name, g := range inv.Groups {
		hosts := inv.groupHostNames(g)
		items := make([]any, len(hosts))
		for i, h := range hosts {
			items[i] = h
		}
		out[name] = items
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
	out := map[string]any{}
	for _, g := range inv.OrderedGroups(h) {
		for k, v := range g.Vars {
			out[k] = v
		}
	}
	for k, v := range h.Vars {
		out[k] = v
	}
	return out
}

// HostNames returns all host names in inventory (first-seen) order.
func (inv *Inventory) HostNames() []string {
	out := make([]string, len(inv.hostOrder))
	for i, h := range inv.hostOrder {
		out[i] = h.Name
	}
	return out
}

// SortedHostNames returns all host names, sorted.
func (inv *Inventory) SortedHostNames() []string {
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
