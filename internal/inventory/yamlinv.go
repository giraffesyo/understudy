package inventory

import (
	"fmt"

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
	v, err := yaml.Unmarshal(data, filename)
	if err != nil {
		return err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: YAML inventory must be a mapping of group names", filename)
	}
	for name, body := range root {
		if err := loadYAMLGroup(inv, name, body, filename); err != nil {
			return err
		}
	}
	return nil
}

func loadYAMLGroup(inv *Inventory, name string, body any, filename string) error {
	group := inv.ensureGroup(name)
	if body == nil {
		return nil
	}
	m, ok := body.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: group %q must map to a mapping (hosts/children/vars)", filename, name)
	}
	for key, val := range m {
		switch key {
		case "hosts":
			hosts, ok := val.(map[string]any)
			if !ok {
				if val == nil {
					continue
				}
				return fmt.Errorf("%s: %s.hosts must be a mapping", filename, name)
			}
			for hostName, hostVars := range hosts {
				names, err := ExpandRange(hostName)
				if err != nil {
					return fmt.Errorf("%s: %v", filename, err)
				}
				var vars map[string]any
				if hostVars != nil {
					vars, ok = hostVars.(map[string]any)
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
			children, ok := val.(map[string]any)
			if !ok {
				if val == nil {
					continue
				}
				return fmt.Errorf("%s: %s.children must be a mapping", filename, name)
			}
			for childName, childBody := range children {
				if err := loadYAMLGroup(inv, childName, childBody, filename); err != nil {
					return err
				}
				linkGroups(group, inv.Groups[childName])
			}
		case "vars":
			vars, ok := val.(map[string]any)
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
