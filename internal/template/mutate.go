package template

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// In-place list and dict methods (x.append(1), d.update({...}), ...).
// ansible-core's Jinja sandbox allows them; they mutate the object the
// template holds. Variables reach a template as copies (ansible-core's lazy
// containers), so a mutation shows in the rest of the render, through
// every name that reaches the object, and never reaches the variable.
//
// A render whose source calls such a method owns what it mutates: the
// variables it reads (copied on first read), the containers reached
// through them (copied on first access and stored back) and those it
// builds. Dicts it owns change in place. Anything else (and a list, whose
// length is part of its value) changes as a copy stored back where the
// receiver came from: a name, a namespace attribute, or a key or index of
// such a place.

// mutator applies an in-place method to a receiver: the receiver's new
// value and the method's return value.
type mutator func(recv any, args []any, kwargs *yaml.OMap) (updated, ret any, err error)

var listMutators = map[string]mutator{
	"append": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("append", args, 1, 1); err != nil {
			return nil, nil, err
		}
		return append(ownedList(recv.([]any)), args[0]), nil, nil
	},
	"extend": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("extend", args, 1, 1); err != nil {
			return nil, nil, err
		}
		items, err := iterate(args[0])
		if err != nil {
			return nil, nil, err
		}
		return append(ownedList(recv.([]any)), items...), nil, nil
	},
	"insert": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("insert", args, 2, 2); err != nil {
			return nil, nil, err
		}
		l := recv.([]any)
		i, ok := asInt(args[0])
		if !ok {
			return nil, nil, fmt.Errorf("'%s' object cannot be interpreted as an integer", typeName(args[0]))
		}
		i = clampIndex(i, len(l))
		return slices.Insert(ownedList(l), int(i), args[1]), nil, nil
	},
	"pop": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("pop", args, 0, 1); err != nil {
			return nil, nil, err
		}
		l := recv.([]any)
		if len(l) == 0 {
			return nil, nil, fmt.Errorf("pop from empty list")
		}
		i := int64(-1)
		if len(args) == 1 {
			var ok bool
			if i, ok = asInt(args[0]); !ok {
				return nil, nil, fmt.Errorf("'%s' object cannot be interpreted as an integer", typeName(args[0]))
			}
		}
		if i < 0 {
			i += int64(len(l))
		}
		if i < 0 || i >= int64(len(l)) {
			return nil, nil, fmt.Errorf("pop index out of range")
		}
		return slices.Delete(ownedList(l), int(i), int(i)+1), l[i], nil
	},
	"remove": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("remove", args, 1, 1); err != nil {
			return nil, nil, err
		}
		l := recv.([]any)
		for i, item := range l {
			if equal(item, args[0]) {
				return slices.Delete(ownedList(l), i, i+1), nil, nil
			}
		}
		return nil, nil, fmt.Errorf("list.remove(x): x not in list")
	},
	"reverse": func(recv any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("reverse", args, 0, 0); err != nil {
			return nil, nil, err
		}
		out := ownedList(recv.([]any))
		slices.Reverse(out)
		return out, nil, nil
	},
	"sort": func(recv any, args []any, kwargs *yaml.OMap) (any, any, error) {
		if len(args) > 0 {
			return nil, nil, fmt.Errorf("sort() takes no positional arguments")
		}
		reverse := false
		if kwargs != nil {
			for _, k := range kwargs.Keys() {
				switch k {
				case "reverse":
					reverse = truthy(kwargs.Get(k))
				case "key":
					if kwargs.Get(k) != nil {
						return nil, nil, fmt.Errorf("sort() key functions are not supported")
					}
				default:
					return nil, nil, fmt.Errorf("'%s' is an invalid keyword argument for sort()", k)
				}
			}
		}
		out := ownedList(recv.([]any))
		var sortErr error
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if reverse {
				a, b = b, a
			}
			c, err := compare(a, b)
			if err != nil && sortErr == nil {
				sortErr = err
			}
			return c < 0
		})
		if sortErr != nil {
			return nil, nil, sortErr
		}
		return out, nil, nil
	},
	"clear": func(_ any, args []any, _ *yaml.OMap) (any, any, error) {
		if err := arity("clear", args, 0, 0); err != nil {
			return nil, nil, err
		}
		return ownedList(nil), nil, nil
	},
}

// dictMutator applies an in-place dict method to out (the receiver
// itself when the render owns it, else a copy of it).
type dictMutator func(out *yaml.OMap, args []any, kwargs *yaml.OMap) (ret any, err error)

var dictMutators = map[string]dictMutator{
	"update": func(out *yaml.OMap, args []any, kwargs *yaml.OMap) (any, error) {
		if err := arity("update", args, 0, 1); err != nil {
			return nil, err
		}
		if len(args) == 1 {
			if keys, m, ok := orderedMap(args[0]); ok {
				for _, k := range keys {
					out.Set(k, m[k])
				}
			} else {
				pairs, err := iterate(args[0])
				if err != nil {
					return nil, err
				}
				for i, p := range pairs {
					kv, ok := Undeprecate(p).([]any)
					if !ok || len(kv) != 2 {
						items, err := iterate(p)
						if err != nil {
							return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", i)
						}
						return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len(items))
					}
					k, ok := asString(kv[0])
					if !ok {
						return nil, fmt.Errorf("dict keys must be strings, got %s", typeName(kv[0]))
					}
					out.Set(k, kv[1])
				}
			}
		}
		if kwargs != nil {
			for _, k := range kwargs.Keys() {
				out.Set(k, kwargs.Get(k))
			}
		}
		return nil, nil
	},
	"pop": func(out *yaml.OMap, args []any, _ *yaml.OMap) (any, error) {
		if err := arity("pop", args, 1, 2); err != nil {
			return nil, err
		}
		k, _ := asString(args[0])
		if v, ok := out.GetItem(k); ok {
			out.Delete(k)
			return v, nil
		}
		if len(args) == 2 {
			return args[1], nil
		}
		return nil, fmt.Errorf("%s", pyRepr(args[0]))
	},
	"popitem": func(out *yaml.OMap, args []any, _ *yaml.OMap) (any, error) {
		if err := arity("popitem", args, 0, 0); err != nil {
			return nil, err
		}
		keys := out.Keys()
		if len(keys) == 0 {
			return nil, fmt.Errorf("'popitem(): dictionary is empty'")
		}
		k := keys[len(keys)-1]
		v := out.Get(k)
		out.Delete(k)
		return []any{k, v}, nil
	},
	"setdefault": func(out *yaml.OMap, args []any, _ *yaml.OMap) (any, error) {
		if err := arity("setdefault", args, 1, 2); err != nil {
			return nil, err
		}
		k, ok := asString(args[0])
		if !ok {
			return nil, fmt.Errorf("dict keys must be strings, got %s", typeName(args[0]))
		}
		if v, ok := out.GetItem(k); ok {
			return v, nil
		}
		var def any
		if len(args) == 2 {
			def = args[1]
		}
		out.Set(k, def)
		return def, nil
	},
	"clear": func(out *yaml.OMap, args []any, _ *yaml.OMap) (any, error) {
		if err := arity("clear", args, 0, 0); err != nil {
			return nil, err
		}
		for _, k := range slices.Clone(out.Keys()) {
			out.Delete(k)
		}
		return nil, nil
	},
}

// arity checks a method's positional argument count as Python does.
func arity(name string, args []any, lo, hi int) error {
	n := len(args)
	switch {
	case n >= lo && n <= hi:
		return nil
	case lo == hi && lo == 0:
		return fmt.Errorf("%s() takes no arguments (%d given)", name, n)
	case lo == hi && lo == 1:
		return fmt.Errorf("%s() takes exactly one argument (%d given)", name, n)
	case lo == hi:
		return fmt.Errorf("%s expected %d arguments, got %d", name, lo, n)
	case n < lo:
		return fmt.Errorf("%s expected at least %d argument%s, got %d", name, lo, plural(lo), n)
	}
	return fmt.Errorf("%s expected at most %d argument%s, got %d", name, hi, plural(hi), n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clampIndex is list.insert's index handling: negative counts from the
// end, and out-of-range indexes clamp to the ends.
func clampIndex(i int64, n int) int64 {
	if i < 0 {
		i += int64(n)
		if i < 0 {
			i = 0
		}
	}
	if i > int64(n) {
		i = int64(n)
	}
	return i
}

// copyDict copies a mapping into an ordered map in its iteration order.
func copyDict(v any) *yaml.OMap {
	out := yaml.NewOMap()
	if keys, m, ok := orderedMap(v); ok {
		for _, k := range keys {
			out.Set(k, m[k])
		}
	}
	return out
}

// callMutator evaluates recv.name(args...) for an in-place method: the
// receiver is evaluated as a place, the method applied, and a changed
// copy stored back. handled is false when the receiver has no such
// method (the ordinary call path reports that).
func (ec *EvalCtx) callMutator(t *callExpr, ga *getAttrExpr) (out any, handled bool, err error) {
	recv, set, err := ec.evalPlace(ga.x)
	if err != nil {
		return nil, true, err
	}
	recv = Undeprecate(recv)
	var lm mutator
	var dm dictMutator
	switch recv.(type) {
	case []any:
		if lm = listMutators[ga.name]; lm == nil {
			return nil, false, nil
		}
	case map[string]any, Mapping:
		if _, ns := recv.(*namespaceValue); ns {
			return nil, false, nil
		}
		if dm = dictMutators[ga.name]; dm == nil {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	args, _, err := ec.evalArgs(t.args, nil)
	if err != nil {
		return nil, true, err
	}
	var kwargs *yaml.OMap
	if len(t.kwargs) > 0 {
		kwargs = yaml.NewOMap()
		for _, kw := range t.kwargs {
			v, err := ec.eval(kw.val)
			if err != nil {
				return nil, true, err
			}
			if err := ec.rejectUndefined(v, kw.val.exprOff()); err != nil {
				return nil, true, err
			}
			kwargs.Set(kw.name, v)
		}
	}
	if dm != nil {
		target, inPlace := recv.(*yaml.OMap)
		inPlace = inPlace && ec.own.owns(recv)
		if !inPlace {
			target = copyDict(recv)
		}
		ret, err := dm(target, args, kwargs)
		if err != nil {
			return nil, true, ec.errf(t.off, "%s", err)
		}
		if !inPlace {
			ec.own.mark(target)
			if set != nil {
				set(target)
			}
		}
		return ret, true, nil
	}
	updated, ret, err := lm(recv, args, kwargs)
	if err != nil {
		return nil, true, ec.errf(t.off, "%s", err)
	}
	ec.own.mark(updated)
	ec.own.moveList(recv, updated)
	if set != nil {
		set(updated)
	}
	return ret, true, nil
}

// evalPlace evaluates an expression that may be stored back into: its
// value, and a setter that replaces it (nil when it is not a place).
func (ec *EvalCtx) evalPlace(e Expr) (any, func(any), error) {
	switch t := e.(type) {
	case *nameExpr:
		v, ok := ec.lookupName(t.name)
		if !ok {
			return Undefined{Name: t.name}, nil, nil
		}
		return v, func(nv any) { ec.rebind(t.name, nv) }, nil
	case *getAttrExpr:
		parent, setParent, err := ec.evalPlace(t.x)
		if err != nil {
			return nil, nil, err
		}
		v, err := ec.getAttr(parent, t.name, t.off)
		if err != nil {
			return nil, nil, err
		}
		return v, ec.childSetter(parent, t.name, setParent), nil
	case *getItemExpr:
		parent, setParent, err := ec.evalPlace(t.x)
		if err != nil {
			return nil, nil, err
		}
		idx, err := ec.eval(t.index)
		if err != nil {
			return nil, nil, err
		}
		v, err := ec.getItem(parent, idx, t.off)
		if err != nil {
			return nil, nil, err
		}
		return v, ec.childSetter(parent, ec.access(idx), setParent), nil
	}
	v, err := ec.eval(e)
	return v, nil, err
}

// childSetter stores a new value under key (a mapping key or list index)
// of parent: in place in a namespace or a container the render owns,
// else as a changed copy stored back into parent's own place.
func (ec *EvalCtx) childSetter(parent, key any, setParent func(any)) func(any) {
	parent = Undeprecate(parent)
	if ns, ok := parent.(*namespaceValue); ok {
		k, _ := asString(key)
		return func(nv any) { ns.attrs[k] = nv }
	}
	owned := ec.own.owns(parent)
	if setParent == nil && !owned {
		return nil
	}
	switch p := parent.(type) {
	case []any:
		i, ok := asInt(key)
		if !ok {
			return nil
		}
		if i < 0 {
			i += int64(len(p))
		}
		if i < 0 || i >= int64(len(p)) {
			return nil
		}
		if owned {
			return func(nv any) { p[i] = nv }
		}
		return func(nv any) {
			out := slices.Clone(p)
			out[i] = nv
			ec.own.mark(out)
			setParent(out)
		}
	case map[string]any, Mapping:
		k, ok := asString(key)
		if !ok {
			return nil
		}
		if m, isOMap := p.(*yaml.OMap); isOMap && owned {
			return func(nv any) { m.Set(k, nv) }
		}
		return func(nv any) {
			out := copyDict(p)
			out.Set(k, nv)
			ec.own.mark(out)
			setParent(out)
		}
	}
	return nil
}

// rebind stores a name's new value in the scope that holds it: the
// innermost frame binding it, else the render's outermost frame (where
// it shadows the variable for the rest of the render).
func (ec *EvalCtx) rebind(name string, v any) {
	s := ec
	for f := ec; f != nil; f = f.parent {
		if _, ok := f.locals[name]; ok {
			f.locals[name] = v
			return
		}
		s = f
	}
	if s.locals == nil {
		s.locals = map[string]any{}
	}
	s.locals[name] = v
}

// ownership is what a render that calls in-place methods owns (nil for
// any other render: nothing is owned).
type ownership struct {
	owned map[uintptr]bool
	vars  map[string]any // the render's copies of the variables it read

	// moved maps an owned list that a method changed to its new value:
	// a list's length is part of its value, so a changed list is a new
	// one, and every read of the old one (another name, a container
	// holding it) follows it here.
	moved map[uintptr][]any
}

// mutatingCall spots a template that may call an in-place method.
var mutatingCall = regexp.MustCompile(`\.\s*(append|extend|insert|pop|remove|reverse|sort|clear|update|popitem|setdefault)\s*\(`)

// newOwnership is the ownership for a render of src (nil when src calls
// no in-place method).
func newOwnership(src string) *ownership {
	if !mutatingCall.MatchString(src) {
		return nil
	}
	return &ownership{owned: map[uintptr]bool{}, vars: map[string]any{}, moved: map[uintptr][]any{}}
}

// ownedList is a new list the render owns: its own backing array (even
// when empty), which is its identity.
func ownedList(items []any) []any {
	return append(make([]any, 0, max(len(items), 1)), items...)
}

// resolve follows a changed owned list to its current value.
func (o *ownership) resolve(v any) any {
	if o == nil || len(o.moved) == 0 {
		return v
	}
	for {
		l, ok := v.([]any)
		if !ok {
			return v
		}
		id, ok := containerID(l)
		if !ok {
			return v
		}
		next, ok := o.moved[id]
		if !ok {
			return v
		}
		v = next
	}
}

// settle replaces changed lists inside an owned value with their current
// values, so a rendered result shows every change.
func (o *ownership) settle(v any) any {
	if o == nil || len(o.moved) == 0 {
		return v
	}
	v = o.resolve(v)
	if !o.owns(v) {
		return v
	}
	switch t := v.(type) {
	case []any:
		for i, item := range t {
			t[i] = o.settle(item)
		}
	case *yaml.OMap:
		for _, k := range t.Keys() {
			t.Set(k, o.settle(t.Get(k)))
		}
	}
	return v
}

// moveList records that an owned list changed into updated.
func (o *ownership) moveList(old, updated any) {
	if o == nil || !o.owns(old) {
		return
	}
	if id, ok := containerID(old); ok {
		o.moved[id] = updated.([]any)
	}
}

// containerID identifies a mutable container: an ordered dict, or a
// list by its backing array.
func containerID(v any) (uintptr, bool) {
	switch t := v.(type) {
	case *yaml.OMap:
		return reflect.ValueOf(t).Pointer(), true
	case []any:
		if cap(t) > 0 {
			return reflect.ValueOf(t).Pointer(), true
		}
	}
	return 0, false
}

func (o *ownership) owns(v any) bool {
	if o == nil {
		return false
	}
	id, ok := containerID(v)
	return ok && o.owned[id]
}

func (o *ownership) mark(v any) {
	if o == nil {
		return
	}
	if id, ok := containerID(v); ok {
		o.owned[id] = true
	}
}

// adopt returns the render's own copy of a container value (v itself when
// it is not a container, or already owned).
func (o *ownership) adopt(v any) any {
	if o == nil || o.owns(v) {
		return v
	}
	var c any
	switch t := v.(type) {
	case []any:
		c = ownedList(t)
	case *yaml.OMap:
		c = t.Clone()
	case map[string]any:
		c = copyDict(t)
	default:
		return v
	}
	o.mark(c)
	return c
}

// variable is the render's copy of a variable's value, made on first read.
func (o *ownership) variable(name string, v any) any {
	if o == nil {
		return v
	}
	if c, ok := o.vars[name]; ok {
		return c
	}
	c := o.adopt(v)
	switch c.(type) {
	case []any, *yaml.OMap:
		o.vars[name] = c
	}
	return c
}

// ownedChild is a container value read from key of an owned parent: the
// render's copy of it, stored back into the parent so every later read
// (and every name bound to it) sees the same object.
func (ec *EvalCtx) ownedChild(parent, key, v any) any {
	o := ec.own
	if o == nil || !o.owns(parent) {
		return v
	}
	switch v.(type) {
	case []any, *yaml.OMap, map[string]any:
	default:
		return v
	}
	if v = o.resolve(v); o.owns(v) {
		return v
	}
	c := o.adopt(v)
	switch p := parent.(type) {
	case *yaml.OMap:
		if k, ok := asString(key); ok {
			p.Set(k, c)
		}
	case []any:
		if i, ok := asInt(key); ok && i >= 0 && i < int64(len(p)) {
			p[i] = c
		}
	}
	return c
}
