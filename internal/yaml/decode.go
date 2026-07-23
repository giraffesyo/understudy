package yaml

import (
	"fmt"
	"strconv"
	"strings"
)

// OnWarning, if set, receives non-fatal decode warnings (duplicate mapping
// keys). The executor points this at its display layer.
var OnWarning func(msg string)

func warnf(format string, args ...any) {
	if OnWarning != nil {
		OnWarning(fmt.Sprintf(format, args...))
	}
}

// Decode converts a node tree to Go values: nil, bool, int64, float64,
// string, []any, map[string]any, VaultedString, or UnsafeString. YAML 1.1
// implicit typing applies to plain scalars; merge keys (<<) are applied;
// aliases are followed (cycles were rejected at parse time).
func (n *Node) Decode() (any, error) {
	switch n.Kind {
	case AliasNode:
		return n.Target.Decode()
	case ScalarNode:
		return n.decodeScalar()
	case SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := item.Decode()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case MappingNode:
		return n.decodeMapping()
	}
	return nil, fmt.Errorf("yaml: unknown node kind %d", n.Kind)
}

func (n *Node) decodeScalar() (any, error) {
	switch n.Tag {
	case "":
		if n.Style == Plain {
			return resolveScalar(n.Value), nil
		}
		return n.Value, nil
	case "!", "!!str":
		return n.Value, nil
	case "!vault":
		return VaultedString{Ciphertext: n.Value}, nil
	case "!unsafe":
		return UnsafeString(n.Value), nil
	case "!!int":
		if v, ok := parseInt11(strings.TrimSpace(n.Value)); ok {
			return v, nil
		}
		return nil, decodeErrf(n, "cannot parse %q as an integer", n.Value)
	case "!!float":
		if v, err := strconv.ParseFloat(strings.TrimSpace(n.Value), 64); err == nil {
			return v, nil
		}
		return parseFloat11(strings.TrimSpace(n.Value)), nil
	case "!!bool":
		if b, ok := boolWords[strings.TrimSpace(n.Value)]; ok {
			return b, nil
		}
		return nil, decodeErrf(n, "cannot parse %q as a boolean", n.Value)
	case "!!null":
		return nil, nil
	}
	return nil, decodeErrf(n, "unsupported YAML tag %q", n.Tag)
}

func (n *Node) decodeMapping() (any, error) {
	out := NewOMap()
	var merges []*Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := n.Content[i].resolveAlias()
		valNode := n.Content[i+1]
		if keyNode.Kind != ScalarNode {
			return nil, decodeErrf(keyNode, "mapping keys must be scalars")
		}
		key := keyNode.Value
		if key == "<<" && keyNode.Style == Plain && keyNode.Tag == "" {
			merges = append(merges, valNode)
			continue
		}
		if out.Has(key) {
			warnf("%s:%d: duplicate mapping key %q (last value wins)", fileOf(n), keyNode.Line, key)
		}
		v, err := valNode.Decode()
		if err != nil {
			return nil, err
		}
		out.Set(key, v)
	}
	// Merge keys: the mapping's own keys win; among multiple merge sources,
	// earlier sources win.
	for _, m := range merges {
		if err := applyMerge(m, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func applyMerge(m *Node, out *OMap) error {
	r := m.resolveAlias()
	switch r.Kind {
	case MappingNode:
		v, err := r.Decode()
		if err != nil {
			return err
		}
		src, ok := v.(*OMap)
		if !ok {
			return decodeErrf(m, "merge key ('<<') value must be a mapping")
		}
		for _, k := range src.Keys() {
			if !out.Has(k) {
				out.Set(k, src.Get(k))
			}
		}
		return nil
	case SequenceNode:
		for _, item := range r.Content {
			if it := item.resolveAlias(); it.Kind != MappingNode {
				return decodeErrf(item, "merge key ('<<') sequence items must be mappings")
			}
			if err := applyMerge(item, out); err != nil {
				return err
			}
		}
		return nil
	}
	return decodeErrf(m, "merge key ('<<') value must be a mapping or a list of mappings")
}

// fileOf is a placeholder until nodes carry their file name; decode warnings
// currently only have line info.
func fileOf(*Node) string { return "yaml" }

func decodeErrf(n *Node, format string, args ...any) error {
	return &Error{Line: n.Line, Col: n.Column, Msg: fmt.Sprintf(format, args...)}
}
