// Package omap is an insertion-ordered string-keyed map, shared by the YAML
// loader, module results and the template engine (stdlib only: the agent
// imports it).
package omap

import (
	"bytes"
	"encoding/json"
	"maps"
	"sort"
)

// OMap is an insertion-ordered string-keyed map. YAML mappings decode to
// *OMap so key order is preserved through templating and serialization,
// matching Python/Ansible (Go's built-in map is unordered). It structurally
// satisfies the template engine's Mapping interface (GetItem/Keys/Len), so
// the engine iterates it in source order.
type OMap struct {
	keys   []string
	values map[string]any
}

// NewOMap returns an empty ordered map.
func NewOMap() *OMap {
	return &OMap{values: map[string]any{}}
}

// Set inserts or updates a key, preserving first-insertion position.
func (m *OMap) Set(key string, val any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = val
}

// GetItem returns the value for a key.
func (m *OMap) GetItem(key string) (any, bool) {
	v, ok := m.values[key]
	return v, ok
}

// Get is a convenience for GetItem's value (nil if absent).
func (m *OMap) Get(key string) any { return m.values[key] }

// Has reports whether a key is present.
func (m *OMap) Has(key string) bool { _, ok := m.values[key]; return ok }

// Delete removes a key.
func (m *OMap) Delete(key string) {
	if _, ok := m.values[key]; !ok {
		return
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in insertion order (a copy is not made; do not
// mutate the result).
func (m *OMap) Keys() []string { return m.keys }

// Len returns the number of entries.
func (m *OMap) Len() int { return len(m.keys) }

// AsMap returns a plain unordered map copy — for consumers that only access
// by key and do not care about order (module args, task keywords).
func (m *OMap) AsMap() map[string]any {
	out := make(map[string]any, len(m.keys))
	maps.Copy(out, m.values)
	return out
}

// MarshalJSON serializes the map with keys in sorted order, so an *OMap
// encodes identically to the equivalent Go map under encoding/json. Display
// consumers (the default callback) rely on that sort_keys=True ordering;
// insertion order stays available via Keys() for consumers that want it (the
// to_json filter renders in Keys() order without going through encoding/json).
func (m *OMap) MarshalJSON() ([]byte, error) {
	keys := append([]string(nil), m.keys...)
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(m.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Clone returns a shallow copy preserving order.
func (m *OMap) Clone() *OMap {
	c := &OMap{keys: append([]string(nil), m.keys...), values: make(map[string]any, len(m.values))}
	maps.Copy(c.values, m.values)
	return c
}

// AsMap recursively converts OMaps to plain map[string]any within a value
// tree — used by consumers (playbook structure, inventory, agent args) that
// need ordinary maps. Lists are walked; scalars pass through.
func AsMap(v any) any {
	switch t := v.(type) {
	case *OMap:
		out := make(map[string]any, t.Len())
		for _, k := range t.keys {
			out[k] = AsMap(t.values[k])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = AsMap(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = AsMap(item)
		}
		return out
	}
	return v
}

// PlainMap converts a decoded value to map[string]any by flattening the
// top-level mapping only — nested *OMap values are left intact so their key
// order survives. Used by consumers whose struct fields are typed
// map[string]any (task args, vars, environment, inventory vars) but which
// still template nested dicts where order matters. Returns (nil,false) if v
// is neither a map nor an *OMap.
func PlainMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case *OMap:
		return t.AsMap(), true
	}
	return nil, false
}
