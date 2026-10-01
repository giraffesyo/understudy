package template

import (
	"errors"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// A template's result is variable storage: ansible-core converts the
// values storage does not support. Markup becomes str, with a warning at
// the template; a value with no such conversion (a timedelta) fails the
// template.

// StorageError is ansible-core's "Type '...' is unsupported for variable
// storage." for a template result holding such a value.
type StorageError struct {
	Type  string // the value's Python type
	Value string // its str(), which the error shows as its source
}

func (e *StorageError) Error() string {
	return "Type '" + e.Type + "' is unsupported for variable storage."
}

// StorageCause reports a template that failed for a value variable
// storage does not support: the cause ansible-core shows under the
// rendering error ("Type 'timedelta' is unsupported ...") and the value.
func StorageCause(err error) (msg, value string, ok bool) {
	var se *StorageError
	if errors.As(err, &se) {
		return se.Error(), se.Value, true
	}
	return "", "", false
}

// storable is v as variable storage keeps it.
func (ec *EvalCtx) storable(v any) (any, error) {
	out, _, err := ec.storableIn(v, nil)
	return out, err
}

func (ec *EvalCtx) storableIn(v any, active map[cycleID]bool) (any, bool, error) {
	if id, ok := containerOf(v); ok {
		if active[id] {
			return v, false, nil
		}
		if active == nil {
			active = map[cycleID]bool{}
		}
		active[id] = true
		defer delete(active, id)
	}
	switch t := v.(type) {
	case Markup:
		if ec.engine.Warning != nil {
			// A container's items are converted with the container.
			pos := ec.pos
			if pos.ContainerFile != "" {
				pos = Position{File: pos.ContainerFile, Line: pos.ContainerLine, Col: pos.ContainerCol}
			}
			ec.engine.Warning(pos, "Type 'Markup' is unsupported in variable storage, converting to 'str'.")
		}
		return string(t), true, nil
	case pyTimedelta:
		return nil, false, &StorageError{Type: "timedelta", Value: toStr(t)}
	case []any:
		var cp []any
		for i, item := range t {
			r, ch, err := ec.storableIn(item, active)
			if err != nil {
				return nil, false, err
			}
			if ch && cp == nil {
				cp = append([]any(nil), t...)
			}
			if cp != nil {
				cp[i] = r
			}
		}
		if cp != nil {
			return cp, true, nil
		}
	case map[string]any:
		var cp map[string]any
		for _, k := range sortedKeys(t) {
			r, ch, err := ec.storableIn(t[k], active)
			if err != nil {
				return nil, false, err
			}
			if ch && cp == nil {
				cp = make(map[string]any, len(t))
				for k2, v2 := range t {
					cp[k2] = v2
				}
			}
			if cp != nil {
				cp[k] = r
			}
		}
		if cp != nil {
			return cp, true, nil
		}
	case *yaml.OMap:
		var cp *yaml.OMap
		for _, k := range t.Keys() {
			r, ch, err := ec.storableIn(t.Get(k), active)
			if err != nil {
				return nil, false, err
			}
			if ch && cp == nil {
				cp = t.Clone()
			}
			if cp != nil {
				cp.Set(k, r)
			}
		}
		if cp != nil {
			return cp, true, nil
		}
	}
	return v, false, nil
}
