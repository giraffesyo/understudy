package template

import (
	"errors"
	"reflect"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// A value can contain itself: a YAML alias inside the collection it
// anchors builds a recursive list or dict, as PyYAML does. Reading into
// one works (rl[1][1][0], length, keys); what walks all of it fails as
// Python does, with its recursion limit: finalizing a template result
// ("Recursive loop detected in template: maximum recursion depth
// exceeded") and the filters that serialize a value. Walkers that must
// not fail stop at a container they are already inside.

// RecursionError is Python's RecursionError where ansible-core reports
// one: in a template result ("template") or a `var` expression
// ("expression"). Pos, when known, is the template that recursed.
type RecursionError struct {
	In  string
	Pos Position
}

// HoldsRecursion reports whether v is or holds the placeholder of a
// lazy container's item that recursed (an Undefined raising a
// RecursionError).
func HoldsRecursion(v any) bool {
	return firstRecursion(v, map[cycleID]bool{}) != nil
}

// firstRecursion is the RecursionError of the first item in v, depth
// first, that recursed.
func firstRecursion(v any, active map[cycleID]bool) *RecursionError {
	if u, ok := v.(Undefined); ok {
		var re *RecursionError
		if u.Err != nil && errors.As(u.Err, &re) {
			return re
		}
		return nil
	}
	id, ok := containerOf(v)
	if !ok || active[id] {
		return nil
	}
	active[id] = true
	defer delete(active, id)
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if re := firstRecursion(item, active); re != nil {
				return re
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if re := firstRecursion(t[k], active); re != nil {
				return re
			}
		}
	case *yaml.OMap:
		for _, k := range t.Keys() {
			if re := firstRecursion(t.Get(k), active); re != nil {
				return re
			}
		}
	}
	return nil
}

func (e *RecursionError) Error() string {
	return "Recursive loop detected in " + e.In + ": maximum recursion depth exceeded"
}

// errMaxRecursion is RecursionError's own message, as a filter that walks
// a recursive value reports it.
const errMaxRecursion = "maximum recursion depth exceeded"

// cycleID identifies a list (its backing array and length) or a map.
type cycleID struct {
	ptr uintptr
	n   int
}

func containerOf(v any) (cycleID, bool) {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return cycleID{}, false
		}
		return cycleID{reflect.ValueOf(t).Pointer(), len(t)}, true
	case map[string]any:
		return cycleID{reflect.ValueOf(t).Pointer(), -1}, true
	case *yaml.OMap:
		return cycleID{reflect.ValueOf(t).Pointer(), -2}, true
	}
	return cycleID{}, false
}

// HasCycle reports whether v contains itself: a list or dict found again
// inside its own items.
func HasCycle(v any) bool {
	return hasCycle(v, map[cycleID]bool{})
}

func hasCycle(v any, active map[cycleID]bool) bool {
	id, ok := containerOf(v)
	if !ok {
		if d, isDep := v.(Deprecated); isDep {
			return hasCycle(d.Value, active)
		}
		return false
	}
	if active[id] {
		return true
	}
	active[id] = true
	defer delete(active, id)
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if hasCycle(item, active) {
				return true
			}
		}
	case map[string]any:
		for _, item := range t {
			if hasCycle(item, active) {
				return true
			}
		}
	case *yaml.OMap:
		for _, k := range t.Keys() {
			if hasCycle(t.Get(k), active) {
				return true
			}
		}
	}
	return false
}

// serializingFilters walk the whole of their input.
var serializingFilters = []string{"string", "to_json", "to_nice_json", "to_yaml", "to_nice_yaml", "flatten", "hash", "checksum"}

// guardRecursion makes the serializing filters fail on a recursive value
// as ansible-core's do, rather than walk it forever.
func guardRecursion(e *Engine) {
	for _, name := range serializingFilters {
		f, ok := e.Filters[name]
		if !ok {
			continue
		}
		e.Filters[name] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
			if HasCycle(in) {
				if name == "string" || name == "hash" || name == "checksum" {
					return nil, &pyTypeError{errMaxRecursion}
				}
				// Elsewhere the RecursionError surfaces while handling
				// another exception.
				return nil, whileHandling("%s", errMaxRecursion)
			}
			return f(ec, in, args, kwargs)
		}
	}
}
