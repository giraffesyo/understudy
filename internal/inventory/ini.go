package inventory

import (
	"fmt"
	"strconv"
	"strings"
)

// LoadINI parses INI-format inventory text into inv.
//
//	host1 ansible_host=10.0.0.1
//	[web]
//	web[01:03].example.com
//	[web:vars]
//	http_port=80
//	[site:children]
//	web
func LoadINI(inv *Inventory, data []byte, filename string) error {
	section := "ungrouped" // bare hosts before any [group]
	kind := "hosts"        // hosts | vars | children

	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return fmt.Errorf("%s:%d: unterminated section header", filename, lineNo+1)
			}
			name := line[1:end]
			kind = "hosts"
			if i := strings.IndexByte(name, ':'); i >= 0 {
				kind = name[i+1:]
				name = name[:i]
				if kind != "vars" && kind != "children" {
					return fmt.Errorf("%s:%d: unknown section suffix %q", filename, lineNo+1, kind)
				}
			}
			section = name
			inv.ensureGroup(section)
			continue
		}

		switch kind {
		case "vars":
			eq := strings.IndexByte(line, '=')
			if eq <= 0 {
				return fmt.Errorf("%s:%d: expected key=value in vars section", filename, lineNo+1)
			}
			key := strings.TrimSpace(line[:eq])
			inv.Groups[section].Vars[key] = coerceINIValue(strings.TrimSpace(line[eq+1:]))
		case "children":
			child := inv.ensureGroup(line)
			linkGroups(inv.Groups[section], child)
		default: // hosts
			if err := parseHostLine(inv, section, line, filename, lineNo+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseHostLine(inv *Inventory, groupName, line, filename string, lineNo int) error {
	words := splitHostLine(line)
	if len(words) == 0 {
		return nil
	}
	names, err := ExpandRange(words[0])
	if err != nil {
		return fmt.Errorf("%s:%d: %v", filename, lineNo, err)
	}
	vars := map[string]any{}
	for _, w := range words[1:] {
		eq := strings.IndexByte(w, '=')
		if eq <= 0 {
			return fmt.Errorf("%s:%d: expected key=value after host name, got %q", filename, lineNo, w)
		}
		vars[w[:eq]] = coerceINIValue(stripQuotes(w[eq+1:]))
	}
	group := inv.Groups[groupName]
	for _, name := range names {
		h := inv.ensureHost(name)
		for k, v := range vars {
			h.Vars[k] = v
		}
		addHostToGroup(group, h)
	}
	return nil
}

// splitHostLine tokenizes a host line on whitespace, keeping quoted values
// (ansible_ssh_common_args="-o Foo=bar") intact.
func splitHostLine(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			inWord = true
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '#' && !inWord:
			// trailing comment
			if inWord {
				out = append(out, cur.String())
			}
			return out
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func stripQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// coerceINIValue applies Ansible's INI literal coercion: ints, floats,
// booleans, and null; everything else stays a string.
func coerceINIValue(s string) any {
	switch s {
	case "True", "true", "yes":
		return true
	case "False", "false", "no":
		return false
	case "None", "null", "~":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strings.ContainsAny(s, ".eE") {
		return f
	}
	return s
}
