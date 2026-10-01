package template

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Deprecated wraps a value ansible-core tags as deprecated (its Deprecated
// data tag: a registered result's skipped_reason, the play_hosts magic
// variable, top-level injected facts). The value behaves as Value, but
// reading it in a template reports a deprecation warning through
// Engine.Deprecation, at the template's origin: when a variable or a
// container item yields it, and when a template's result still carries
// it (a whole dict rendered or dumped).
type Deprecated struct {
	Value   any
	Msg     string // "The 'skipped_reason' value is deprecated."
	Help    string // "Use 'skip_reason' instead."
	Version string // the ansible-core version that removes it
	// Obj, for a warning about a value (a plugin's obj=), is the value's
	// text, shown when it has no origin.
	Obj string
	// Bare: a plugin's warning about no value, shown without an origin.
	Bare bool
}

// MarshalJSON encodes the plain value.
func (d Deprecated) MarshalJSON() ([]byte, error) { return json.Marshal(d.Value) }

// Undeprecate returns v without its Deprecated wrapper (top level only).
func Undeprecate(v any) any {
	if d, ok := v.(Deprecated); ok {
		return d.Value
	}
	return v
}

// KeepsDeprecated is implemented by a VarGetter whose templating results
// keep their Deprecated wrappers (set_fact: the copy stays deprecated, as
// ansible-core's tags travel with the value). Results are otherwise
// returned plain.
type KeepsDeprecated interface {
	KeepDeprecated() bool
}

// TaggedGetter is a VarGetter that can return a variable with its
// Deprecated wrapper; the engine prefers it so reading the variable warns.
type TaggedGetter interface {
	GetTagged(name string) (any, bool)
}

// deprecated reports d at this template's origin.
func (ec *EvalCtx) deprecated(d Deprecated) {
	if ec.engine.Deprecation != nil {
		ec.engine.Deprecation(ec.pos, d)
	}
}

// pluginDeprecated is a plugin's deprecation warning (Display.deprecated),
// at pos (zero: none, or with d.Obj, the value's of unknown origin).
func (ec *EvalCtx) pluginDeprecated(d Deprecated, pos Position) {
	d.Bare = d.Obj == "" && pos.File == ""
	if ec.engine.Deprecation != nil {
		ec.engine.Deprecation(pos, d)
	}
}

// ignoredInput is from_yaml's and from_yaml_all's deprecation warning
// for input that is not a str, about that value: where it was written,
// else its text.
func (ec *EvalCtx) ignoredInput(filter string, in any) {
	d := Deprecated{Msg: fmt.Sprintf("The %s filter ignored non-string input of type %s.", filter, pyStrRepr(NativeTypeName(in))),
		Version: "2.23", Obj: toStr(in)}
	ec.pluginDeprecated(d, ec.inputOrigin())
}

// finalized reports d found in a template's result: at the template's
// origin, or for its container when the template is an item of one.
func (ec *EvalCtx) finalized(d Deprecated) {
	if ec.pos.InContainer {
		if ec.engine.Deprecation != nil {
			ec.engine.Deprecation(ContainerOrigin, d)
		}
		return
	}
	ec.deprecated(d)
}

// access is reading v from a variable or container: a deprecated value
// warns and yields its plain value.
func (ec *EvalCtx) access(v any) any {
	for {
		d, ok := v.(Deprecated)
		if !ok {
			return v
		}
		ec.deprecated(d)
		v = d.Value
		last := d
		last.Value = v
		ec.lastDeprecated = &last
	}
}

// finalize reports every deprecated value a template result still holds.
// The result keeps its wrappers when the variables ask for it (then a
// result that is itself a deprecated value read last, like "{{ fact }}",
// stays deprecated too), and is returned plain otherwise.
func (ec *EvalCtx) finalize(v any) any {
	keep := false
	if k, ok := ec.vars.(KeepsDeprecated); ok {
		keep = k.KeepDeprecated()
	}
	out, _ := walkDeprecated(v, ec.finalized, !keep)
	if keep && ec.lastDeprecated != nil && sameValue(out, ec.lastDeprecated.Value) {
		d := *ec.lastDeprecated
		d.Value = out
		return d
	}
	return out
}

// sameValue reports whether a and b are the same value: the same
// container, or equal scalars.
func sameValue(a, b any) (same bool) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && reflect.ValueOf(x).Pointer() == reflect.ValueOf(y).Pointer()
	case []any:
		y, ok := b.([]any)
		return ok && len(x) == len(y) && reflect.ValueOf(x).Pointer() == reflect.ValueOf(y).Pointer()
	}
	defer func() {
		if recover() != nil {
			same = false // uncomparable
		}
	}()
	return a == b
}

// walkDeprecated calls visit for each deprecated value inside v. With
// strip, it returns v without the wrappers (copying only the containers
// that held one); changed reports whether it did.
func walkDeprecated(v any, visit func(Deprecated), strip bool) (out any, changed bool) {
	return walkDeprecatedIn(v, visit, strip, nil)
}

// walkDeprecatedIn is walkDeprecated inside the containers of active (a
// container met again, in a recursive value, is not walked twice).
func walkDeprecatedIn(v any, visit func(Deprecated), strip bool, active map[cycleID]bool) (out any, changed bool) {
	if id, ok := containerOf(v); ok {
		if active[id] {
			return v, false
		}
		if active == nil {
			active = map[cycleID]bool{}
		}
		active[id] = true
		defer delete(active, id)
	}
	switch t := v.(type) {
	case Deprecated:
		visit(t)
		inner, _ := walkDeprecatedIn(t.Value, visit, strip, active)
		if strip {
			return inner, true
		}
		return v, false
	case []any:
		var cp []any
		for i, item := range t {
			r, ch := walkDeprecatedIn(item, visit, strip, active)
			if ch && cp == nil {
				cp = append([]any(nil), t...)
			}
			if cp != nil {
				cp[i] = r
			}
		}
		if cp != nil {
			return cp, true
		}
	case map[string]any:
		var cp map[string]any
		for _, k := range sortedKeys(t) { // sorted: warnings in a stable order
			r, ch := walkDeprecatedIn(t[k], visit, strip, active)
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
			return cp, true
		}
	case *yaml.OMap:
		var cp *yaml.OMap
		for _, k := range t.Keys() {
			r, ch := walkDeprecatedIn(t.Get(k), visit, strip, active)
			if ch && cp == nil {
				cp = yaml.NewOMap()
				for _, k2 := range t.Keys() {
					cp.Set(k2, t.Get(k2))
				}
			}
			if cp != nil {
				cp.Set(k, r)
			}
		}
		if cp != nil {
			return cp, true
		}
	}
	return v, false
}

// StripDeprecated returns v without any Deprecated wrappers, silently.
func StripDeprecated(v any) any {
	out, _ := walkDeprecated(v, func(Deprecated) {}, true)
	return out
}
