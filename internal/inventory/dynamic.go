package inventory

import (
	"fmt"
	"maps"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// dynamicHost is one add_host change (ansible-core's AddHost).
type dynamicHost struct {
	name   string
	keys   []string // host vars, in order
	vars   map[string]any
	groups []string
}

// dynamicGroup is one group_by change (AddGroup, for a host).
type dynamicGroup struct {
	host, group string
	parents     []string
}

// AddDynamicHost is InventoryManager.add_dynamic_host, add_host's change:
// the host joins the inventory (in the all group) when it is new, its
// variables are combined with vars (keys in order; the values are final,
// never templated again) and it joins groups, each created when new. It
// reports whether anything changed and the hosts whose variables may
// have.
func (inv *Inventory) AddDynamicHost(name string, keys []string, vars map[string]any, groups []string) (bool, []string, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	d := dynamicHost{name: name, keys: keys, vars: vars, groups: groups}
	inv.dynHosts = append(inv.dynHosts, d)
	return inv.addDynamicHost(d)
}

func (inv *Inventory) addDynamicHost(d dynamicHost) (bool, []string, error) {
	changed := false
	h, ok := inv.Hosts[d.name]
	if !ok {
		h = inv.ensureHost(d.name)
		h.Vars["inventory_file"] = nil
		h.Vars["inventory_dir"] = nil
		addHostToGroup(inv.Groups["all"], h)
		for _, vd := range inv.varsDirs {
			if err := applyHostVarsFiles(inv, h, vd.dir, vd.layer); err != nil {
				return true, nil, err
			}
		}
		changed = true
	}
	// combine_vars(host.get_vars(), host_vars): the host's own variables
	// (not its vars files'), replaced key by key.
	combined := maps.Clone(h.Vars)
	varsChanged := false
	for _, k := range d.keys {
		v := d.vars[k]
		old, had := h.Vars[k]
		if !had || !template.Equal(unfinal(old), v) {
			varsChanged = true
		}
		combined[k] = finalValue(v)
	}
	if varsChanged {
		// The host's other variables keep their origins; add_host's
		// values have none.
		origins := yaml.ChildOrigins(h.Vars)
		for _, k := range d.keys {
			delete(origins, k)
		}
		yaml.SetChildOrigins(combined, origins)
		h.Vars = combined
		changed = true
	}
	for _, name := range d.groups {
		g, ok := inv.Groups[name]
		if !ok {
			var err error
			if g, err = inv.addDynamicGroupNamed(name); err != nil {
				return changed, nil, err
			}
			changed = true
		}
		if _, in := g.Hosts[h.Name]; !in {
			addHostToGroup(g, h)
			changed = true
		}
	}
	if changed {
		if err := inv.reconcileDynamic(); err != nil {
			return changed, nil, err
		}
	}
	return changed, []string{h.Name}, nil
}

// AddDynamicGroup is InventoryManager.add_dynamic_group, group_by's
// change for host: the group (and each parent) is created when new, made
// a child of the parents, and given the host. It reports whether
// anything changed and the hosts whose variables may have.
func (inv *Inventory) AddDynamicGroup(host, group string, parents []string) (bool, []string, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	d := dynamicGroup{host: host, group: group, parents: parents}
	inv.dynGroups = append(inv.dynGroups, d)
	return inv.addDynamicGroup(d, false)
}

func (inv *Inventory) addDynamicGroup(d dynamicGroup, replay bool) (bool, []string, error) {
	changed := false
	h := inv.Hosts[d.host]
	if h == nil {
		switch {
		case inv.localhost != nil && d.host == inv.localhost.Name:
			h = inv.localhost
		case !replay:
			return false, nil, fmt.Errorf("%s cannot be matched in inventory", d.host)
		default:
			// The host left the inventory when it was refreshed.
			return false, nil, nil
		}
	}
	name := d.group
	if _, ok := inv.Groups[name]; !ok {
		g, err := inv.addDynamicGroupNamed(name)
		if err != nil {
			return false, nil, err
		}
		name = g.Name
	}
	for _, p := range d.parents {
		if _, ok := inv.Groups[p]; !ok {
			if _, err := inv.addDynamicGroupNamed(p); err != nil {
				return false, nil, err
			}
			changed = true
		}
	}
	g := inv.Groups[name]
	for _, p := range d.parents {
		parent := inv.Groups[p]
		if parent == nil {
			// The group was made under its sanitized name: ansible-core
			// looks it up as written (a KeyError).
			return changed, nil, fmt.Errorf("%s", template.PyRepr(p))
		}
		if parent == g {
			return changed, nil, fmt.Errorf("can't add group to itself")
		}
		if _, linked := parent.Children[g.Name]; !linked {
			if err := inv.addChild(parent, g); err != nil {
				return changed, nil, err
			}
			changed = true
		}
	}
	if !inv.groupHasHost(g, h) {
		addHostToGroup(g, h)
		changed = true
	}
	if changed {
		if err := inv.reconcileDynamic(); err != nil {
			return changed, nil, err
		}
	}
	return changed, inv.groupHostNames(g), nil
}

// groupHasHost reports whether h is among g.get_hosts() (its own hosts
// and its descendants').
func (inv *Inventory) groupHasHost(g *Group, h *Host) bool {
	for _, name := range inv.groupHostNames(g) {
		if name == h.Name {
			return true
		}
	}
	return false
}

// addDynamicGroupNamed is InventoryData.add_group for a group add_host or
// group_by names: the name is made safe as TRANSFORM_INVALID_GROUP_CHARS
// says (warning as ansible-core does), and a group new under that name
// takes its group_vars files.
func (inv *Inventory) addDynamicGroupNamed(name string) (*Group, error) {
	if name == "" {
		return nil, fmt.Errorf("Invalid empty/false group name provided: %s", name)
	}
	name = inv.groupNameFor(name, false)
	if g, ok := inv.Groups[name]; ok {
		return g, nil
	}
	g := inv.newGroup(name)
	for _, vd := range inv.varsDirs {
		if err := applyGroupVarsFiles(inv, g, vd.dir, vd.layer); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// groupNameFor is to_safe_group_name under TRANSFORM_INVALID_GROUP_CHARS
// (silent: no warning).
func (inv *Inventory) groupNameFor(name string, silent bool) string {
	if invalidGroupChars.Search(name, 0, -1) == nil {
		return name
	}
	switch inv.TransformGroupChars {
	case "always", "silently":
		if inv.TransformGroupChars == "always" && !silent {
			inv.warning("Invalid characters were found in group names and automatically replaced, use -vvvv to see details")
		}
		return replaceInvalidGroupChars(name)
	case "ignore":
	default:
		if !silent {
			inv.warning("Invalid characters were found in group names but not replaced, use -vvvv to see details")
		}
	}
	return name
}

// reconcileDynamic is reconcile_inventory after a change: groups without
// a parent join "all", hosts take their groups' ancestors and the
// ungrouped group is kept right; group depths follow the new edges.
func (inv *Inventory) reconcileDynamic() error {
	inv.matchCache = nil
	inv.reconcile()
	return inv.finalize()
}

// ReplayDynamic is refresh_inventory's second half: the add_host and
// group_by changes made to the inventory the run had so far are applied
// to this freshly parsed one (hosts first, then groups), and kept for
// the next refresh.
func (inv *Inventory) ReplayDynamic(from *Inventory) error {
	from.mu.Lock()
	hosts := append([]dynamicHost(nil), from.dynHosts...)
	groups := append([]dynamicGroup(nil), from.dynGroups...)
	from.mu.Unlock()
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.dynHosts, inv.dynGroups = hosts, groups
	for _, d := range hosts {
		if _, _, err := inv.addDynamicHost(d); err != nil {
			return err
		}
	}
	for _, d := range groups {
		if _, _, err := inv.addDynamicGroup(d, true); err != nil {
			return err
		}
	}
	return nil
}

// finalValue marks a value add_host set as final: already templated,
// never templated again.
func finalValue(v any) any {
	if _, ok := v.(vars.Final); ok {
		return v
	}
	return vars.Final{V: v}
}

func unfinal(v any) any {
	if f, ok := v.(vars.Final); ok {
		return f.V
	}
	return v
}
