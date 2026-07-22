package inventory

import (
	"fmt"
	"path"
	"strings"
)

// Match resolves a host pattern against the inventory. Terms are separated
// by ':' or ',' and evaluated in order against an accumulating set:
// plain = union, &term = intersect, !term = subtract. Each term matches an
// exact host, exact group, or glob against host then group names.
func (inv *Inventory) Match(pattern string) ([]*Host, error) {
	terms := splitPattern(pattern)
	if len(terms) == 0 {
		return nil, fmt.Errorf("empty host pattern")
	}
	selected := map[string]bool{}
	order := []string{}
	add := func(name string) {
		if !selected[name] {
			selected[name] = true
			order = append(order, name)
		}
	}

	for _, term := range terms {
		op := byte(0)
		if term[0] == '&' || term[0] == '!' {
			op = term[0]
			term = term[1:]
		}
		names, err := inv.matchTerm(term)
		if err != nil {
			return nil, err
		}
		switch op {
		case 0:
			for _, n := range names {
				add(n)
			}
		case '&':
			keep := map[string]bool{}
			for _, n := range names {
				keep[n] = true
			}
			for name := range selected {
				if !keep[name] {
					delete(selected, name)
				}
			}
		case '!':
			for _, n := range names {
				delete(selected, n)
			}
		}
	}

	var out []*Host
	for _, name := range order {
		if selected[name] {
			out = append(out, inv.Hosts[name])
		}
	}
	return out, nil
}

// matchTerm resolves one pattern term to host names (inventory order:
// sorted for determinism).
func (inv *Inventory) matchTerm(term string) ([]string, error) {
	if term == "all" || term == "*" {
		return inv.SortedHostNames(), nil
	}
	if strings.HasPrefix(term, "~") {
		return nil, fmt.Errorf("regex host patterns (~) are not supported yet")
	}
	if h, ok := inv.Hosts[term]; ok {
		return []string{h.Name}, nil
	}
	if g, ok := inv.Groups[term]; ok {
		return inv.groupHostNames(g), nil
	}
	if strings.ContainsAny(term, "*?[") {
		var out []string
		for _, name := range inv.SortedHostNames() {
			if ok, _ := path.Match(term, name); ok {
				out = append(out, name)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
		// Fall back to matching group names.
		for gName, g := range inv.Groups {
			if ok, _ := path.Match(term, gName); ok {
				out = append(out, inv.groupHostNames(g)...)
			}
		}
		return dedupe(out), nil
	}
	// Unknown name: Ansible warns and matches nothing.
	return nil, nil
}

// splitPattern splits on ',' (preferred) or ':', avoiding splits inside
// [] ranges (IPv6 addresses, host ranges).
func splitPattern(pattern string) []string {
	sep := byte(':')
	if strings.ContainsRune(pattern, ',') {
		sep = ','
	}
	var terms []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '[':
			depth++
			cur.WriteByte(c)
		case c == ']':
			depth--
			cur.WriteByte(c)
		case c == sep && depth == 0:
			if t := strings.TrimSpace(cur.String()); t != "" {
				terms = append(terms, t)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if t := strings.TrimSpace(cur.String()); t != "" {
		terms = append(terms, t)
	}
	return terms
}

func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
