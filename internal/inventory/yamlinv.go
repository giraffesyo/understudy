package inventory

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/giraffesyo/understudy/internal/template"
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
	return loadYAMLInventory(inv, data, filename)
}

// loadYAMLInventory is ansible-core's yaml inventory plugin, its errors
// and warnings included.
func loadYAMLInventory(inv *Inventory, data []byte, filename string) error {
	v, err := yaml.Unmarshal(data, absPath(filename))
	if err != nil {
		// AnsibleParserError(e): the message alone, without its origin.
		var ye *yaml.Error
		if errors.As(err, &ye) {
			return errors.New(ye.Message())
		}
		return err
	}
	root, isMap := asMapping(v)
	switch {
	case v == nil || isMap && len(root) == 0 || isEmpty(v):
		return errors.New("Parsed empty YAML file")
	case !isMap:
		return fmt.Errorf("YAML inventory has invalid structure, it should be a dictionary, got: %s", typeRepr(v))
	case truthy(root["plugin"]):
		return errors.New("Plugin configuration YAML file, not YAML inventory")
	}
	for _, name := range mappingKeys(v) {
		if _, err := loadYAMLGroup(inv, name, root[name]); err != nil {
			return err
		}
	}
	return nil
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case bool:
		return !t
	case int64:
		return t == 0
	case float64:
		return t == 0
	}
	return false
}

func truthy(v any) bool {
	if v == nil {
		return false
	}
	if m, ok := asMapping(v); ok {
		return len(m) > 0
	}
	return !isEmpty(v)
}

// loadYAMLGroup is the yaml plugin's _parse_group; it returns the group
// name.
func loadYAMLGroup(inv *Inventory, name string, body any) (string, error) {
	m, isMap := asMapping(body)
	if !isMap && body != nil {
		inv.warning(fmt.Sprintf("Skipping '%s' as this is not a valid group definition", name))
		return name, nil
	}
	group := inv.ensureGroup(name)
	if body == nil {
		return name, nil
	}
	sections := map[string]any{}
	for _, section := range []string{"vars", "children", "hosts"} {
		val, ok := m[section]
		if !ok {
			continue
		}
		if s, isStr := val.(string); isStr {
			om := yaml.NewOMap()
			om.Set(s, nil)
			val = om
		}
		if _, isMap := asMapping(val); !isMap && val != nil {
			return "", fmt.Errorf(`Invalid "%s" entry for "%s" group, requires a dictionary, found "%s" instead.`, section, name, typeRepr(val))
		}
		sections[section] = val
	}
	for _, key := range mappingKeys(body) {
		val := m[key]
		if s, ok := sections[key]; ok {
			val = s
		}
		sub, isMap := asMapping(val)
		if !isMap && val != nil {
			inv.warning(fmt.Sprintf("Skipping key (%s) in group (%s) as it is not a mapping, it is a %s", key, name, typeRepr(val)))
			continue
		}
		if val == nil {
			continue
		}
		switch key {
		case "vars":
			origins := yaml.ChildOrigins(group.Vars)
			if origins == nil {
				origins = map[string]yaml.ChildPos{}
			}
			for _, k := range mappingKeys(val) {
				group.Vars[k] = sub[k]
				yaml.MergeChildOrigin(origins, k, val)
			}
			yaml.SetChildOrigins(group.Vars, origins)
		case "children":
			for _, childName := range mappingKeys(val) {
				child, err := loadYAMLGroup(inv, childName, sub[childName])
				if err != nil {
					return "", err
				}
				if g, ok := inv.Groups[child]; ok {
					if err := inv.addChild(group, g); err != nil {
						return "", err
					}
				} else if h, ok := inv.Hosts[child]; ok {
					addHostToGroup(group, h)
				} else {
					return "", fmt.Errorf("%s is not a known host nor group", child)
				}
			}
		case "hosts":
			for _, pattern := range mappingKeys(val) {
				names, port, err := expandHostPattern(pattern)
				if err != nil {
					return "", err
				}
				hostVars := sub[pattern]
				var vars map[string]any
				var keys []string
				if truthy(hostVars) {
					var ok bool
					if vars, ok = asMapping(hostVars); !ok {
						return "", fmt.Errorf("Invalid data from file, expected dictionary and got:\n\n%s", template.PyStr(hostVars))
					}
					keys = mappingKeys(hostVars)
				}
				for _, n := range names {
					h := inv.addHost(n, group, port)
					origins := yaml.ChildOrigins(h.Vars)
					if origins == nil {
						origins = map[string]yaml.ChildPos{}
					}
					for _, k := range keys {
						h.Vars[k] = vars[k]
						yaml.MergeChildOrigin(origins, k, hostVars)
					}
					yaml.SetChildOrigins(h.Vars, origins)
				}
			}
		default:
			inv.warning(fmt.Sprintf(`Skipping unexpected key (%s) in group (%s), only "vars", "children" and "hosts" are valid`, key, name))
		}
	}
	return name, nil
}

// absPath is the path ansible-core names an inventory file by in load
// warnings and errors (sources are made absolute).
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
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
