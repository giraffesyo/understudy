package inventory

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Match resolves a host pattern against the inventory. Terms are separated
// by ':' or ',' and evaluated in order against an accumulating set:
// plain = union, &term = intersect, !term = subtract. Each term matches an
// exact host, exact group, or glob against host then group names.
//
// Results are cached by pattern until add_host or group_by reconcile the
// inventory (InventoryManager's _hosts_patterns_cache).
func (inv *Inventory) Match(pattern string) ([]*Host, error) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if hosts, ok := inv.matchCache[pattern]; ok {
		return slices.Clone(hosts), nil
	}
	hosts, err := inv.match(pattern)
	if err != nil {
		return nil, err
	}
	if inv.matchCache == nil {
		inv.matchCache = map[string][]*Host{}
	}
	inv.matchCache[pattern] = hosts
	return slices.Clone(hosts), nil
}

func (inv *Inventory) match(pattern string) ([]*Host, error) {
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
			out = append(out, inv.getHost(name))
		}
	}
	return out, nil
}

// matchTerm resolves one pattern term the way Ansible's _enumerate_matches
// does: hosts of every group whose name matches, then (for globs, or when no
// group matched) hosts whose name matches, in inventory order, deduplicated.
func (inv *Inventory) matchTerm(term string) ([]string, error) {
	if term == "all" || term == "*" {
		return inv.groupHostNames(inv.Groups["all"]), nil
	}
	if _, ok := inv.Hosts[term]; ok {
		return []string{term}, nil
	}
	if strings.HasPrefix(term, "~") {
		return nil, fmt.Errorf("regex host patterns (~) are not supported yet")
	}
	isGlob := strings.ContainsAny(term, ".?*[")
	match := func(name string) bool {
		if name == term {
			return true
		}
		ok, _ := path.Match(term, name)
		return ok
	}
	var out []string
	matchedGroup := false
	for _, gName := range inv.groupNamesInOrder() {
		if match(gName) {
			matchedGroup = true
			out = append(out, inv.groupHostNames(inv.Groups[gName])...)
		}
	}
	if !matchedGroup || isGlob {
		for _, h := range inv.hostOrder {
			if match(h.Name) {
				out = append(out, h.Name)
			}
		}
	}
	if len(out) == 0 && localhostNames[term] {
		// The implicit localhost, created on first use.
		return []string{inv.getHost(term).Name}, nil
	}
	if len(out) == 0 && !matchedGroup {
		msg := "Could not match supplied host pattern, ignoring: " + term
		switch inv.PatternMismatch {
		case "error":
			return nil, errors.New(msg)
		case "ignore":
		default:
			inv.warning(msg)
		}
	}
	return dedupe(out), nil
}

// groupNamesInOrder lists groups in creation order ("all", "ungrouped",
// then as loaded), the order Ansible's groups dict iterates in.
func (inv *Inventory) groupNamesInOrder() []string {
	out := make([]string, len(inv.groupOrder))
	for i, g := range inv.groupOrder {
		out[i] = g.Name
	}
	return out
}

// splitPattern splits on ',' (preferred) or ':', avoiding splits inside
// [] ranges (IPv6 addresses, host ranges).
func splitPattern(pattern string) []string {
	var parts []string
	switch {
	case strings.Contains(pattern, ","):
		parts = strings.Split(pattern, ",")
	default:
		if _, _, err := parseAddress(pattern, true); err == nil {
			// One address: IPv6 colons and [x:y] ranges do not split.
			parts = []string{pattern}
		} else {
			parts = colonTerms.FindAllString(pattern, -1)
		}
	}
	var terms []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			terms = append(terms, t)
		}
	}
	return terms
}

// colonTerms splits a ':'-separated pattern list, keeping [..]
// expressions whole.
var colonTerms = regexp.MustCompile(`(?:[^\s:\[\]]|\[[^\]]*\])+`)

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
