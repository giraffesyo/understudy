package inventory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/giraffesyo/understudy/internal/template"
)

// LoadINI parses INI-format inventory text into inv, as ansible-core's
// ini inventory plugin does:
//
//	host1 ansible_host=10.0.0.1
//	[web]
//	web[01:03].example.com
//	[web:vars]
//	http_port=80
//	[site:children]
//	web
//
// Values go through Python's ast.literal_eval (so "yes" stays a string,
// 1.5 is a float and [1, 2] a list). Errors carry the plugin's messages.
func LoadINI(inv *Inventory, data []byte, filename string) error {
	if err := parseINI(inv, data); err != nil {
		return &chainError{msg: "Failed to parse inventory.", ctx: "Origin: " + filename, cause: asChain(err)}
	}
	return nil
}

var (
	iniSection   = regexp.MustCompile(`^\[([^:\]\s]+)(?::(\w+))?\]\s*(?:#.*)?$`)
	iniGroupName = regexp.MustCompile(`^([^:\]\s]+)\s*(?:#.*)?$`)
)

type pendingDecl struct {
	state, name string
	parents     []string
}

func parseINI(inv *Inventory, data []byte) error {
	pending := map[string]*pendingDecl{}
	var pendingOrder []string
	groupName, state := "ungrouped", "hosts"
	source := inv.currentSource
	for lineNo, raw := range pySplitLines(string(data)) {
		lineNo++
		line := strings.TrimFunc(raw, unicode.IsSpace)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if m := iniSection.FindStringSubmatch(line); m != nil {
			groupName, state = m[1], m[2]
			if state == "" {
				state = "hosts"
			}
			if state != "hosts" && state != "children" && state != "vars" {
				return fmt.Errorf("Section [%s:%s] has unknown type: %s", m[1], m[2], state)
			}
			if _, ok := inv.Groups[groupName]; !ok {
				if _, isPending := pending[groupName]; state == "vars" && !isPending {
					pending[groupName] = &pendingDecl{state: state, name: groupName}
					pendingOrder = append(pendingOrder, groupName)
				}
				inv.ensureGroup(groupName)
			}
			if d, ok := pending[groupName]; ok && state != "vars" {
				switch d.state {
				case "children":
					if err := inv.addPendingChildren(groupName, pending); err != nil {
						return err
					}
				case "vars":
					delete(pending, groupName)
				}
			}
			continue
		} else if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			return fmt.Errorf("Invalid section entry: '%s'. Please make sure that there are no spaces in the section entry, and that there are no other invalid characters", line)
		}

		switch state {
		case "hosts":
			tokens, err := shlexSplit(line)
			if err != nil {
				// Raised while handling shlex's error: not collapsed
				// into its parent when shown.
				return &chainError{msg: fmt.Sprintf("Error parsing host definition '%s': %s", line, err), hiddenCause: true}
			}
			if len(tokens) == 0 {
				return fmt.Errorf("list index out of range")
			}
			hosts, port, err := iniExpandHostPattern(tokens[0])
			if err != nil {
				return err
			}
			vars := yamlOrderedVars{}
			for _, t := range tokens[1:] {
				k, v, ok := strings.Cut(t, "=")
				if !ok {
					return fmt.Errorf("Expected key=value host variable assignment, got: %s", t)
				}
				vars.set(k, parseINIValue(v))
			}
			for _, name := range hosts {
				h := inv.addHost(name, inv.Groups[groupName], port)
				vars.apply(h.Vars)
				for _, t := range tokens[1:] {
					if k, _, _ := strings.Cut(t, "="); template.IsReservedName(k) {
						h.VarOrigins = append(h.VarOrigins, template.KeyOrigin{Name: k, File: source, Line: lineNo})
					}
				}
			}
		case "vars":
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return fmt.Errorf("Expected key=value, got: %s", line)
			}
			k = strings.TrimFunc(k, unicode.IsSpace)
			g := inv.Groups[groupName]
			g.Vars[k] = parseINIValue(strings.TrimFunc(v, unicode.IsSpace))
			if template.IsReservedName(k) {
				g.VarOrigins = append(g.VarOrigins, template.KeyOrigin{Name: k, File: source, Line: lineNo})
			}
		case "children":
			m := iniGroupName.FindStringSubmatch(line)
			if m == nil {
				return fmt.Errorf("Expected group name, got: %s", line)
			}
			child := m[1]
			if g, ok := inv.Groups[child]; !ok {
				if d, isPending := pending[child]; !isPending {
					pending[child] = &pendingDecl{state: state, name: child, parents: []string{groupName}}
					pendingOrder = append(pendingOrder, child)
				} else {
					d.parents = append(d.parents, groupName)
				}
			} else if err := inv.addChild(inv.Groups[groupName], g); err != nil {
				return err
			}
		}
	}
	// Report the first unresolved reference.
	for _, name := range pendingOrder {
		d, ok := pending[name]
		if !ok {
			continue
		}
		if d.state == "vars" {
			return fmt.Errorf("Section [%s:vars] not valid for undefined group '%s'.", d.name, d.name)
		}
		return fmt.Errorf("Section [%s:children] includes undefined group '%s'.", d.parents[len(d.parents)-1], d.name)
	}
	return nil
}

func (inv *Inventory) addPendingChildren(group string, pending map[string]*pendingDecl) error {
	for _, parent := range pending[group].parents {
		if err := inv.addChild(inv.Groups[parent], inv.Groups[group]); err != nil {
			return err
		}
		if d, ok := pending[parent]; ok && d.state == "children" {
			if err := inv.addPendingChildren(parent, pending); err != nil {
				return err
			}
		}
	}
	delete(pending, group)
	return nil
}

// iniExpandHostPattern adds the ini plugin's checks to the base
// expansion.
func iniExpandHostPattern(pattern string) ([]string, int, error) {
	hosts, port, err := expandHostPattern(pattern)
	if err != nil {
		return nil, -1, err
	}
	if strings.HasSuffix(strings.TrimFunc(pattern, unicode.IsSpace), ":") && port < 0 {
		return nil, -1, fmt.Errorf("Invalid host pattern '%s' supplied, ending in ':' is not allowed, this character is reserved to provide a port.", pattern)
	}
	for _, h := range hosts {
		if strings.TrimFunc(h, unicode.IsSpace) == "---" {
			return nil, -1, fmt.Errorf("Invalid host pattern '%s' supplied, '---' is normally a sign this is a YAML file.", pattern)
		}
	}
	return hosts, port, nil
}

// parseINIValue is the ini plugin's _parse_value: a Python literal when
// ast.literal_eval accepts it, else the text itself.
func parseINIValue(v string) any {
	if out, ok := literalEval(v); ok {
		return out
	}
	return v
}

// pySplitLines is Python's str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(rs[start:i]))
			start = i + 1
		case '\r':
			out = append(out, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// yamlOrderedVars keeps variable assignments in order (later wins).
type yamlOrderedVars struct {
	keys []string
	vals map[string]any
}

func (v *yamlOrderedVars) set(k string, val any) {
	if v.vals == nil {
		v.vals = map[string]any{}
	}
	if _, ok := v.vals[k]; !ok {
		v.keys = append(v.keys, k)
	}
	v.vals[k] = val
}

func (v *yamlOrderedVars) apply(into map[string]any) {
	for _, k := range v.keys {
		into[k] = v.vals[k]
	}
}
