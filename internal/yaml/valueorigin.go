package yaml

import (
	"reflect"
	"strconv"
	"sync"
	"unsafe"

	"github.com/giraffesyo/understudy/internal/omap"
)

// Where decoded containers and their items came from. ansible-core tags
// every loaded value with its origin, which a broken conditional's
// message names ("derived from value of type 'int' at 'vars.yml:3:8'").
// Strings are found by their backing array (see Origin); a decoded list
// or mapping is found by its identity, and records its items' positions
// too, so an item with no identity of its own (an int) is found by its
// container and key. A plain map flattened from a decoded mapping
// (PlainMap) inherits the mapping's records.

type containerOrigin struct {
	keep     any // keeps the container alive, so its address is not reused
	self     origin
	children map[string]origin
}

var containerOrigins sync.Map // containerKey -> *containerOrigin

// containerKey identifies a list (its backing array) or a mapping.
func containerKey(v any) (uintptr, bool) {
	switch t := v.(type) {
	case []any:
		if cap(t) == 0 {
			return 0, false
		}
		return uintptr(unsafe.Pointer(unsafe.SliceData(t[:1]))), true
	case *omap.OMap:
		return uintptr(unsafe.Pointer(t)), true
	case map[string]any:
		return reflect.ValueOf(t).Pointer(), true
	}
	return 0, false
}

// recordContainer remembers where a decoded collection and its items
// were.
func recordContainer(v any, n *Node, items map[string]*Node) {
	if n.file == "" {
		return
	}
	k, ok := containerKey(v)
	if !ok {
		return
	}
	co := &containerOrigin{keep: v, self: origin{file: n.file, line: n.Line, col: n.Column},
		children: make(map[string]origin, len(items))}
	for key, item := range items {
		co.children[key] = origin{file: n.file, line: item.Line, col: item.Column}
	}
	containerOrigins.Store(k, co)
}

func lookupContainer(v any) (*containerOrigin, bool) {
	k, ok := containerKey(v)
	if !ok {
		return nil, false
	}
	co, found := containerOrigins.Load(k)
	if !found {
		return nil, false
	}
	return co.(*containerOrigin), true
}

// ValueOrigin reports where v was parsed: a decoded string, list or
// mapping (or a plain map flattened from one). line 0 is a source with no
// position (a whole file).
func ValueOrigin(v any) (file string, line, col int, ok bool) {
	if s, isStr := v.(string); isStr {
		return Origin(s)
	}
	co, found := lookupContainer(v)
	if !found {
		return "", 0, 0, false
	}
	return co.self.file, co.self.line, co.self.col, true
}

// ChildOrigin reports where item key (a list's index) of a decoded
// container was parsed.
func ChildOrigin(container any, key string) (file string, line, col int, ok bool) {
	co, found := lookupContainer(container)
	if !found {
		return "", 0, 0, false
	}
	o, has := co.children[key]
	if !has {
		return "", 0, 0, false
	}
	return o.file, o.line, o.col, true
}

// RecordOrigin remembers where the string s came from, for sources other
// than YAML (an INI inventory's line: col 0).
func RecordOrigin(s, file string, line, col int) { recordOrigin(s, file, line, col) }

// RecordChildOrigins remembers where the items of m (by key) came from,
// for maps built other than by decoding (merged vars, an INI section).
func RecordChildOrigins(m map[string]any, file string, children map[string][2]int) {
	k, ok := containerKey(m)
	if !ok || file == "" {
		return
	}
	co := &containerOrigin{keep: m, self: origin{file: file}, children: make(map[string]origin, len(children))}
	for key, lc := range children {
		co.children[key] = origin{file: file, line: lc[0], col: lc[1]}
	}
	containerOrigins.Store(k, co)
}

// InheritChildOrigins records dst's items as having come from where the
// same keys of srcs did (later sources win): a map merged from decoded
// ones.
func InheritChildOrigins(dst map[string]any, srcs ...map[string]any) {
	k, ok := containerKey(dst)
	if !ok {
		return
	}
	co := &containerOrigin{keep: dst, children: map[string]origin{}}
	found := false
	for _, src := range srcs {
		sco, has := lookupContainer(src)
		if !has {
			continue
		}
		found = true
		if co.self.file == "" {
			co.self = sco.self
		}
		for key := range src {
			if o, has := sco.children[key]; has {
				co.children[key] = o
			}
		}
	}
	if found {
		containerOrigins.Store(k, co)
	}
}

func indexKey(i int) string { return strconv.Itoa(i) }

// ChildPos is where an item of a container came from.
type ChildPos struct {
	File      string
	Line, Col int
}

// ChildOrigins is a copy of where the items of container came from.
func ChildOrigins(container any) map[string]ChildPos {
	co, found := lookupContainer(container)
	if !found {
		return nil
	}
	out := make(map[string]ChildPos, len(co.children))
	for k, o := range co.children {
		out[k] = ChildPos{File: o.file, Line: o.line, Col: o.col}
	}
	return out
}

// SetChildOrigins records where m's items came from (replacing what was
// recorded): a map assembled from several sources (inventory vars).
func SetChildOrigins(m map[string]any, children map[string]ChildPos) {
	k, ok := containerKey(m)
	if !ok {
		return
	}
	if len(children) == 0 {
		containerOrigins.Delete(k)
		return
	}
	co := &containerOrigin{keep: m, children: make(map[string]origin, len(children))}
	for key, p := range children {
		co.children[key] = origin{file: p.File, line: p.Line, col: p.Col}
	}
	containerOrigins.Store(k, co)
}

// MergeChildOrigin records in into (a ChildOrigins map being assembled)
// where item key of src came from, or forgets it when unknown.
func MergeChildOrigin(into map[string]ChildPos, key string, src any) {
	if file, line, col, ok := ChildOrigin(src, key); ok {
		into[key] = ChildPos{File: file, Line: line, Col: col}
	} else {
		delete(into, key)
	}
}
