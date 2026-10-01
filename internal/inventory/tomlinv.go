package inventory

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// parseTOMLInventory is the toml inventory plugin:
//
//	[web]
//	children = ["apache"]
//	vars = { http_port = 8080 }
//
//	[web.hosts]
//	host1 = {}
//	host2 = { ansible_port = 222 }
func parseTOMLInventory(l *loader, src, _, _ string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("An error occurred while parsing the file %s.", pyQuote(src))
	}
	data, err := parseTOML(string(raw))
	if err != nil {
		var te *tomlError
		if errors.As(err, &te) {
			msg := fmt.Sprintf("TOML file %s is invalid: %s", pyQuote(src), te.msg)
			if te.cause != "" {
				msg = concatMessage(msg, te.cause)
			}
			return errors.New(msg)
		}
		return err
	}
	if data.Len() == 0 {
		return errors.New("Parsed empty TOML file")
	}
	if truthy(data.Get("plugin")) {
		return errors.New("Plugin configuration TOML file, not TOML inventory")
	}
	for _, name := range data.Keys() {
		if err := parseTOMLGroup(l.inv, name, data.Get(name)); err != nil {
			return err
		}
	}
	return nil
}

// tomlTypeRepr is type() of a value tomllib decoded.
func tomlTypeRepr(v any) string {
	name := "str"
	switch v.(type) {
	case yaml.Datetime:
		name = "datetime.datetime"
	case yaml.Date:
		name = "datetime.date"
	case yaml.Time:
		name = "datetime.time"
	case bool:
		name = "bool"
	case int64, *big.Int:
		name = "int"
	case float64:
		name = "float"
	case []any:
		name = "list"
	case *yaml.OMap:
		name = "dict"
	}
	return "<class '" + name + "'>"
}

// parseTOMLGroup is the toml plugin's _parse_group.
func parseTOMLGroup(inv *Inventory, name string, body any) error {
	m, isMap := body.(*yaml.OMap)
	if body != nil && !isMap {
		inv.warning(fmt.Sprintf("Skipping '%s' as this is not a valid group definition", name))
		return nil
	}
	group := inv.ensureGroup(name)
	if m == nil {
		return nil
	}
	for _, key := range m.Keys() {
		data := m.Get(key)
		switch key {
		case "vars":
			vars, ok := data.(*yaml.OMap)
			if !ok {
				return fmt.Errorf(`Invalid "vars" entry for "%s" group, requires a dict, found "%s" instead.`, name, tomlTypeRepr(data))
			}
			for _, k := range vars.Keys() {
				group.Vars[k] = vars.Get(k)
			}
		case "children":
			children, ok := data.([]any)
			if !ok {
				return fmt.Errorf(`Invalid "children" entry for "%s" group, requires a list, found "%s" instead.`, name, tomlTypeRepr(data))
			}
			for _, c := range children {
				child, ok := c.(string)
				if !ok {
					return fmt.Errorf("Invalid group name supplied, expected a string but got %s for %s", tomlTypeRepr(c), template.PyStr(c))
				}
				if err := parseTOMLGroup(inv, child, yaml.NewOMap()); err != nil {
					return err
				}
				// The child is named as written, which add_group may
				// have changed (TRANSFORM_INVALID_GROUP_CHARS).
				if g, ok := inv.Groups[child]; ok {
					if err := inv.addChild(group, g); err != nil {
						return err
					}
				} else if h, ok := inv.Hosts[child]; ok {
					addHostToGroup(group, h)
				} else {
					return fmt.Errorf("%s is not a known host nor group", child)
				}
			}
		case "hosts":
			hosts, ok := data.(*yaml.OMap)
			if !ok {
				return fmt.Errorf(`Invalid "hosts" entry for "%s" group, requires a dict, found "%s" instead.`, name, tomlTypeRepr(data))
			}
			for _, pattern := range hosts.Keys() {
				names, port, err := expandHostPattern(pattern)
				if err != nil {
					return err
				}
				value := hosts.Get(pattern)
				vars, ok := value.(*yaml.OMap)
				if !ok {
					return fmt.Errorf("Invalid data from file, expected dictionary and got:\n\n%s", template.PyStr(value))
				}
				for _, n := range names {
					h := inv.addHost(n, group, port)
					for _, k := range vars.Keys() {
						h.Vars[k] = vars.Get(k)
					}
				}
			}
		default:
			inv.warning(fmt.Sprintf(`Skipping unexpected key "%s" in group "%s", only "vars", "children" and "hosts" are valid`, key, name))
		}
	}
	return nil
}
