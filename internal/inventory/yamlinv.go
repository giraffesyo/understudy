package inventory

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// LoadYAML parses YAML-format inventory:
//
//	all:
//	  hosts:
//	    web1: {ansible_host: 10.0.0.1}
//	  children:
//	    dbservers:
//	      hosts:
//	        db1:
//	      vars:
//	        pg_port: 5432
func LoadYAML(inv *Inventory, data []byte, filename string) error {
	v, err := yaml.Unmarshal(data, absPath(filename))
	if err != nil {
		return err
	}
	rootKeys, root, ok := orderedMap(v)
	if !ok {
		return fmt.Errorf("%s: YAML inventory must be a mapping of group names", filename)
	}
	for _, name := range rootKeys {
		body := root[name]
		if err := loadYAMLGroup(inv, name, body, filename); err != nil {
			return err
		}
	}
	return nil
}

// absPath is the path ansible-core names an inventory file by in load
// warnings and errors (sources are made absolute).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func loadYAMLGroup(inv *Inventory, name string, body any, filename string) error {
	group := inv.ensureGroup(name)
	if body == nil {
		return nil
	}
	keys, m, ok := orderedMap(body)
	if !ok {
		return fmt.Errorf("%s: group %q must map to a mapping (hosts/children/vars)", filename, name)
	}
	for _, key := range keys {
		val := m[key]
		switch key {
		case "hosts":
			hostNames, hosts, ok := orderedMap(val)
			if !ok {
				if val == nil {
					continue
				}
				return fmt.Errorf("%s: %s.hosts must be a mapping", filename, name)
			}
			for _, hostName := range hostNames {
				hostVars := hosts[hostName]
				names, err := ExpandRange(hostName)
				if err != nil {
					return fmt.Errorf("%s: %v", filename, err)
				}
				var vars map[string]any
				if hostVars != nil {
					vars, ok = yaml.PlainMap(hostVars)
					if !ok {
						return fmt.Errorf("%s: vars for host %q must be a mapping", filename, hostName)
					}
				}
				for _, n := range names {
					h := inv.ensureHost(n)
					for k, v := range vars {
						h.Vars[k] = v
					}
					addHostToGroup(group, h)
				}
			}
		case "children":
			childNames, children, ok := orderedMap(val)
			if !ok {
				if val == nil {
					continue
				}
				return fmt.Errorf("%s: %s.children must be a mapping", filename, name)
			}
			for _, childName := range childNames {
				childBody := children[childName]
				if err := loadYAMLGroup(inv, childName, childBody, filename); err != nil {
					return err
				}
				linkGroups(group, inv.Groups[childName])
			}
		case "vars":
			vars, ok := yaml.PlainMap(val)
			if !ok {
				if val == nil {
					continue
				}
				return fmt.Errorf("%s: %s.vars must be a mapping", filename, name)
			}
			for k, v := range vars {
				group.Vars[k] = v
			}
		default:
			return fmt.Errorf("%s: unexpected key %q under group %q (expected hosts/children/vars)", filename, key, name)
		}
	}
	return nil
}

// orderedMap returns a mapping's keys in source order (YAML order decides
// Ansible's inventory host order) alongside its plain-map form.
func orderedMap(v any) ([]string, map[string]any, bool) {
	m, ok := yaml.PlainMap(v)
	if !ok {
		return nil, nil, false
	}
	if om, isOM := v.(*yaml.OMap); isOM {
		return om.Keys(), m, true
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, m, true
}
