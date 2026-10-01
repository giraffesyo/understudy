package template

import (
	"errors"
	"fmt"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// debug's var= evaluates its expression with ansible-core's
// ReplacingMarkerBehavior: an undefined value is not an error but is
// replaced, where it ends up in the result, by "<< error N - message >>",
// and the messages are then warned about ("Encountered N template
// errors."), grouped by the template each came from.

// Marker is one undefined value a replacing evaluation met.
type Marker struct {
	Msg string   // the undefined value's message
	Pos Position // the template it came from
}

// EvalExpressionReplacing evaluates src as debug's var= does: undefined
// values in the result are replaced by numbered placeholders, reported in
// markers in their order. Any other error fails as EvalExpression's do.
func (e *Engine) EvalExpressionReplacing(src string, vars VarGetter, pos Position) (out any, markers []Marker, err error) {
	if err := e.syntaxError(src, pos, true, false); err != nil {
		return nil, nil, err
	}
	toks, err := lex("{{ "+src+" }}", e.Opts.exprOpts(), pos)
	if err != nil {
		return nil, nil, err
	}
	p := &parser{tokens: toks, src: src, tplPos: pos}
	if _, err := p.expect(tokVarStart); err != nil {
		return nil, nil, err
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, nil, err
	}
	if _, err := p.expect(tokVarEnd); err != nil {
		return nil, nil, err
	}
	ec := &EvalCtx{engine: e, vars: vars, locals: map[string]any{}, pos: pos, src: src, own: newOwnership(src), replaceMarkers: true}
	v, err := ec.evalMarking(expr)
	if err != nil {
		return nil, nil, err
	}
	if HasCycle(v) {
		return nil, nil, &RecursionError{In: "expression"}
	}
	v = ec.finalize(v)
	return replaceMarkers(v, pos, &markers), markers, nil
}

// evalMarking is eval where an undefined value's use (or a variable's
// that failed to resolve as one) yields that undefined value as a marker.
func (ec *EvalCtx) evalMarking(e Expr) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			re, ok := r.(error)
			if !ok {
				panic(r)
			}
			v, err = markerOf(re)
		}
	}()
	v, err = ec.eval(e)
	if err != nil {
		return markerOf(err)
	}
	return v, nil
}

// markerOf is the marker an undefined error becomes; other errors stay.
func markerOf(err error) (any, error) {
	var ue *UndefinedError
	if errors.As(err, &ue) {
		return Undefined{Name: ue.Name, Err: ue}, nil
	}
	return nil, err
}

// replaceMarkers replaces the undefined values in v, depth first.
func replaceMarkers(v any, pos Position, markers *[]Marker) any {
	switch t := v.(type) {
	case Undefined:
		m := Marker{Pos: pos}
		var ue *UndefinedError
		if t.Err != nil && errors.As(t.Err, &ue) {
			if ue.Pos.File != "" {
				m.Pos = ue.Pos
			}
			m.Msg, _ = Cause(ue)
		} else if t.Err != nil {
			m.Msg, _ = Cause(t.Err)
		} else {
			m.Msg = undefinedCause(t.Name)
		}
		*markers = append(*markers, m)
		return fmt.Sprintf("<< error %d - %s >>", len(*markers), m.Msg)
	case []any:
		for i, item := range t {
			t[i] = replaceMarkers(item, pos, markers)
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			t[k] = replaceMarkers(t[k], pos, markers)
		}
	}
	if m, ok := v.(interface {
		Keys() []string
		Get(string) any
		Set(string, any)
	}); ok {
		for _, k := range m.Keys() {
			m.Set(k, replaceMarkers(m.Get(k), pos, markers))
		}
	}
	return v
}

// MarkerWarnings are the warnings ansible-core emits for markers: one per
// run of markers from the same template, "Encountered N template
// error(s)." with each numbered message.
func MarkerWarnings(markers []Marker) []MarkerWarning {
	var out []MarkerWarning
	for i := 0; i < len(markers); {
		j := i
		for j < len(markers) && markers[j].Pos == markers[i].Pos {
			j++
		}
		msg := fmt.Sprintf("Encountered %d template error%s.", j-i, pluralS(j-i > 1))
		for k := i; k < j; k++ {
			msg += fmt.Sprintf("\nerror %d - %s", k+1, markers[k].Msg)
		}
		out = append(out, MarkerWarning{Msg: msg, Pos: markers[i].Pos})
		i = j
	}
	return out
}

// MarkerWarning is one such warning, at its template's origin.
type MarkerWarning struct {
	Msg string
	Pos Position
}

// firstMarker is the first undefined value inside v's containers (depth
// first), which using v trips: a list or dict literal keeps an undefined
// item ([x.y] | length is 1) until its value is needed.
func firstMarker(v any) (Undefined, bool) {
	return firstMarkerIn(v, map[cycleID]bool{})
}

func firstMarkerIn(v any, active map[cycleID]bool) (Undefined, bool) {
	if u, ok := v.(Undefined); ok {
		return u, true
	}
	id, ok := containerOf(v)
	if !ok || active[id] {
		return Undefined{}, false
	}
	active[id] = true
	defer delete(active, id)
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if u, ok := firstMarkerIn(item, active); ok {
				return u, true
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if u, ok := firstMarkerIn(t[k], active); ok {
				return u, true
			}
		}
	case *yaml.OMap:
		for _, k := range t.Keys() {
			if u, ok := firstMarkerIn(t.Get(k), active); ok {
				return u, true
			}
		}
	}
	return Undefined{}, false
}

// tripMarkers is the error using v raises when it holds an undefined
// value, nil otherwise.
func tripMarkers(v any, pos Position) error {
	if u, ok := firstMarker(v); ok {
		return u.useError(pos)
	}
	return nil
}

// markerSafeFilters pass the items of their input along without using
// them, so an undefined item is not an error there.
var markerSafeFilters = map[string]bool{
	"length": true, "count": true, "first": true, "last": true, "list": true, "reverse": true,
	"select": true, "reject": true, "selectattr": true, "rejectattr": true, "map": true,
	"default": true, "d": true, "ternary": true, "mandatory": true, "type_debug": true,
	"batch": true, "slice": true, "random": true, "shuffle": true,
}

// markerSafeTests do not use the items of their input.
var markerSafeTests = map[string]bool{
	"defined": true, "undefined": true, "none": true, "iterable": true, "sequence": true,
	"mapping": true, "string": true, "number": true, "integer": true, "float": true,
	"boolean": true, "callable": true, "sameas": true, "truthy": true, "falsy": true,
	"true": true, "false": true,
}
