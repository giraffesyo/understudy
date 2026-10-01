package factcache

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// The cache persistence profile serializes a tagged value as
// {"value": ..., "tags": [...], "__ansible_type": "_AnsibleTagged<T>"}:
// its Origin (where it was loaded or templated) and, for a string loaded
// from a trusted source, TrustedAsTemplate. bool and None carry no tags.

const typeKey = "__ansible_type"

// Untag is the plain value of a serialized one: tagged values unwrapped,
// dates and times as their isoformat(), an encrypted string as its vault
// payload. A type the profile does not deserialize is an error.
func Untag(v any) (any, error) {
	switch t := v.(type) {
	case *omap.OMap:
		if typ, ok := t.Get(typeKey).(string); ok && t.Has(typeKey) {
			switch typ {
			case "_AnsibleTaggedStr":
				// A string keeps its origin (a broken conditional names
				// it), as the tagged value does.
				s, ok := t.Get("value").(string)
				if !ok {
					return Untag(t.Get("value"))
				}
				s = strings.Clone(s)
				if tags, ok := t.Get("tags").([]any); ok {
					for _, tag := range tags {
						if o, ok := tag.(*omap.OMap); ok && o.Get(typeKey) == "Origin" {
							path, _ := o.Get("path").(string)
							line, _ := o.Get("line_num").(int64)
							col, _ := o.Get("col_num").(int64)
							if path != "" {
								yaml.RecordOrigin(s, path, int(line), int(col))
							}
						}
					}
				}
				return s, nil
			case "_AnsibleTaggedInt", "_AnsibleTaggedFloat", "_AnsibleTaggedBytes",
				"_AnsibleTaggedList", "_AnsibleTaggedTuple", "_AnsibleTaggedSet", "_AnsibleTaggedDict",
				"_AnsibleTaggedDate", "_AnsibleTaggedTime", "_AnsibleTaggedDateTime":
				return Untag(t.Get("value"))
			case "AnsibleSerializableDate", "AnsibleSerializableTime", "AnsibleSerializableDateTime":
				return t.Get("iso8601"), nil
			case "EncryptedString":
				s, _ := t.Get("value").(string)
				return yaml.VaultedString{Ciphertext: s}, nil
			}
			return nil, fmt.Errorf("Object of type %s is not JSON deserializable by the 'cache_persistence' profile.", template.PyRepr(typ))
		}
		out := omap.NewOMap()
		for _, k := range t.Keys() {
			pv, err := Untag(t.Get(k))
			if err != nil {
				return nil, err
			}
			out.Set(k, pv)
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			pv, err := Untag(item)
			if err != nil {
				return nil, err
			}
			out[i] = pv
		}
		return out, nil
	}
	return v, nil
}

// Origin is where a value came from: a file position, or (Line 0) a
// description such as "<CLI option '-e'>" or a whole file.
type Origin struct {
	File      string
	Line, Col int
}

// Tag is the serialized form of a fact's value v, with the tags
// ansible-core's would carry: a value loaded from YAML (a string found
// by its origin, a container and its items) carries its Origin, a
// loaded string TrustedAsTemplate too; a value templated from raw (the
// variable or argument as written, when hasRaw) carries the template's
// Origin; top, when known, is the value's origin otherwise.
func Tag(v, raw any, hasRaw bool, top Origin) any {
	if !hasRaw {
		raw = nil
	}
	var topPos *Origin
	if top.File != "" {
		topPos = &top
	}
	return tagNode(v, raw, topPos)
}

// isTemplate is whether a raw string is (or holds) a template.
func isTemplate(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#")
}

func stringOrigin(s string) (*Origin, bool) {
	if file, line, col, ok := yaml.Origin(s); ok {
		return &Origin{File: file, Line: line, Col: col}, true
	}
	return nil, false
}

func containerOrigin(v any) *Origin {
	if _, isStr := v.(string); isStr {
		return nil
	}
	if file, line, col, ok := yaml.ValueOrigin(v); ok {
		return &Origin{File: file, Line: line, Col: col}
	}
	return nil
}

func childOrigin(container any, key string) *Origin {
	if container == nil {
		return nil
	}
	if file, line, col, ok := yaml.ChildOrigin(container, key); ok {
		return &Origin{File: file, Line: line, Col: col}
	}
	return nil
}

// rawOrigin is the origin of a raw template string that produced a value.
func rawOrigin(raw any) *Origin {
	if s, ok := raw.(string); ok && isTemplate(s) {
		o, _ := stringOrigin(s)
		return o
	}
	return nil
}

func tagNode(v, raw any, pos *Origin) any {
	v = template.Undeprecate(v)
	switch t := v.(type) {
	case nil, bool:
		return v
	case string:
		if o, ok := stringOrigin(t); ok {
			return tagged("_AnsibleTaggedStr", t, o, true)
		}
		if rs, ok := raw.(string); ok && !isTemplate(rs) && rs == t {
			if o, ok := stringOrigin(rs); ok {
				return tagged("_AnsibleTaggedStr", t, o, true)
			}
		}
		return tagged("_AnsibleTaggedStr", t, first(rawOrigin(raw), pos), false)
	case yaml.UnsafeString:
		return tagged("_AnsibleTaggedStr", string(t), first(rawOrigin(raw), pos), false)
	case int, int64, *big.Int:
		return tagged("_AnsibleTaggedInt", t, first(rawOrigin(raw), pos), false)
	case float64:
		return tagged("_AnsibleTaggedFloat", t, first(rawOrigin(raw), pos), false)
	case yaml.VaultedString:
		m := omap.NewOMap()
		m.Set("value", t.Ciphertext)
		var tags []any
		if o := first(rawOrigin(raw), pos); o != nil {
			tags = append(tags, originTag(*o))
		}
		m.Set("tags", append([]any{}, tags...))
		m.Set(typeKey, "EncryptedString")
		return m
	case []any:
		rawList, _ := raw.([]any)
		if len(rawList) != len(t) {
			rawList = nil
		}
		out := make([]any, len(t))
		for i, item := range t {
			k := strconv.Itoa(i)
			var rawItem any
			if rawList != nil {
				rawItem = rawList[i]
			}
			out[i] = tagNode(item, rawItem, first(childOrigin(t, k), childOrigin(rawList, k)))
		}
		return tagged("_AnsibleTaggedList", out, first(containerOrigin(t), containerOrigin(rawList), rawOrigin(raw), pos), false)
	case map[string]any, *omap.OMap:
		keys, get := mappingOf(t)
		rawKeys, rawGet := mappingOf(raw)
		var rawMap any
		if rawKeys != nil {
			rawMap = raw
		}
		out := omap.NewOMap()
		for _, k := range keys {
			var rawItem any
			if rawGet != nil {
				rawItem, _ = rawGet(k)
			}
			val, _ := get(k)
			out.Set(k, tagNode(val, rawItem, first(childOrigin(t, k), childOrigin(rawMap, k))))
		}
		return tagged("_AnsibleTaggedDict", out, first(containerOrigin(t), containerOrigin(rawMap), rawOrigin(raw), pos), false)
	}
	if dt, ok := v.(interface{ Isoformat() string }); ok {
		m := omap.NewOMap()
		m.Set("iso8601", dt.Isoformat())
		m.Set("fold", int64(0))
		m.Set(typeKey, "AnsibleSerializableDateTime")
		return tagged("_AnsibleTaggedDateTime", m, first(rawOrigin(raw), pos), false)
	}
	return v
}

// mappingOf is a mapping's keys in order (a plain map's sorted, as
// understudy keeps no order for them) and an accessor; nil for a
// non-mapping.
func mappingOf(v any) ([]string, func(string) (any, bool)) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys, func(k string) (any, bool) { x, ok := t[k]; return x, ok }
	case *omap.OMap:
		return t.Keys(), t.GetItem
	}
	return nil, nil
}

func first(os ...*Origin) *Origin {
	for _, o := range os {
		if o != nil {
			return o
		}
	}
	return nil
}

// tagged is a value with its tags serialized (the value itself when it
// has none).
func tagged(typ string, v any, o *Origin, trusted bool) any {
	var tags []any
	if o != nil {
		tags = append(tags, originTag(*o))
	}
	if trusted {
		t := omap.NewOMap()
		t.Set(typeKey, "TrustedAsTemplate")
		tags = append(tags, t)
	}
	if len(tags) == 0 {
		return v
	}
	m := omap.NewOMap()
	m.Set("value", v)
	m.Set("tags", tags)
	m.Set(typeKey, typ)
	return m
}

// originTag is an Origin tag: its path (or description), line and
// column, the fields that are set.
func originTag(o Origin) *omap.OMap {
	m := omap.NewOMap()
	if strings.HasPrefix(o.File, "<") {
		m.Set("description", o.File)
	} else {
		m.Set("path", o.File)
	}
	if o.Line > 0 {
		m.Set("line_num", int64(o.Line))
		m.Set("col_num", int64(o.Col))
	}
	m.Set(typeKey, "Origin")
	return m
}
