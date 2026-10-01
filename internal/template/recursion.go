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
// ("expression").
type RecursionError struct{ In string }

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
				return nil, errors.New(errMaxRecursion)
			}
			return f(ec, in, args, kwargs)
		}
	}
}
