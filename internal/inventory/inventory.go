// Package inventory loads Ansible inventories (INI and YAML), applies
// group_vars/host_vars directories, and resolves host patterns.
package inventory

import (
	"fmt"
	"sort"
)

// Host is one managed host.
type Host struct {
	Name   string
	Vars   map[string]any
	groups map[string]*Group
}

// Group is a named set of hosts with vars and child groups.
type Group struct {
	Name     string
	Vars     map[string]any
	Hosts    map[string]*Host
	Children map[string]*Group
	Parents  map[string]*Group
	depth    int

	// Insertion order, which Ansible's "inventory" host order follows.
	hostOrder  []*Host
	childOrder []*Group
}

// Inventory is the loaded host/group graph. "all" and "ungrouped" always
// exist.
type Inventory struct {
	Hosts  map[string]*Host
	Groups map[string]*Group

	hostOrder []*Host // first-seen order
}

// New returns an empty inventory with the implicit groups.
func New() *Inventory {
	inv := &Inventory{Hosts: map[string]*Host{}, Groups: map[string]*Group{}}
	all := inv.ensureGroup("all")
	ungrouped := inv.ensureGroup("ungrouped")
	linkGroups(all, ungrouped)
	return inv
}

func (inv *Inventory) ensureGroup(name string) *Group {
	if g, ok := inv.Groups[name]; ok {
		return g
	}
	g := &Group{
		Name:     name,
		Vars:     map[string]any{},
		Hosts:    map[string]*Host{},
		Children: map[string]*Group{},
		Parents:  map[string]*Group{},
	}
	inv.Groups[name] = g
	if name != "all" && name != "ungrouped" {
		linkGroups(inv.Groups["all"], g)
	}
	return g
}

func (inv *Inventory) ensureHost(name string) *Host {
	if h, ok := inv.Hosts[name]; ok {
		return h
	}
	h := &Host{Name: name, Vars: map[string]any{}, groups: map[string]*Group{}}
	inv.Hosts[name] = h
	inv.hostOrder = append(inv.hostOrder, h)
	return h
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

// finalize computes group depths and moves parentless hosts to ungrouped.
// Call once after loading all sources.
func (inv *Inventory) finalize() error {
	// Any host only in "all" belongs to ungrouped.
	for _, h := range inv.hostOrder {
		inGroup := false
		for name := range h.groups {
			if name != "all" && name != "ungrouped" {
				inGroup = true
				break
			}
		}
		if !inGroup {
			addHostToGroup(inv.Groups["ungrouped"], h)
		}
	}
	// A host is a member of every ancestor of its direct groups (var
	// inheritance and group_names both need the closure).
	for _, h := range inv.Hosts {
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

// OrderedGroups returns a host's groups in Ansible's var-merge order:
// shallowest first (so deeper, more specific groups override), ties broken
// alphabetically.
func (inv *Inventory) OrderedGroups(h *Host) []*Group {
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

// GroupNames returns a host's group names (excluding "all"), ordered like
// OrderedGroups — the group_names magic variable.
func (inv *Inventory) GroupNames(h *Host) []string {
	var out []string
	for _, g := range inv.OrderedGroups(h) {
		if g.Name != "all" {
			out = append(out, g.Name)
		}
	}
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
	var out []string
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
