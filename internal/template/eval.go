package template

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

const maxEvalDepth = 200

// eval walks the expression tree. Undefined values flow through name/attr/
// item access (chainable) and error everywhere else except the allowlisted
// filters and tests.
func (ec *EvalCtx) eval(e Expr) (any, error) {
	ec.depth++
	defer func() { ec.depth-- }()
	if ec.depth > maxEvalDepth {
		return nil, ec.errf(e.exprOff(), "expression nesting too deep")
	}

	switch t := e.(type) {
	case *literalExpr:
		return t.val, nil

	case *nameExpr:
		if v, ok := ec.lookupName(t.name); ok {
			return v, nil
		}
		return Undefined{Name: t.name}, nil

	case *getAttrExpr:
		x, err := ec.eval(t.x)
		if err != nil {
			return nil, err
		}
		v, err := ec.getAttr(x, t.name, t.off)
		if m, ok := v.(boundMethod); ok && err == nil {
			return &methodValue{call: m, name: t.name, recv: ec.access(x), fromVar: ec.isVarRef(t.x)}, nil
		}
		return v, err

	case *getItemExpr:
		x, err := ec.eval(t.x)
		if err != nil {
			return nil, err
		}
		idx, err := ec.eval(t.index)
		if err != nil {
			return nil, err
		}
		return ec.getItem(x, idx, t.off)

	case *sliceExpr:
		return ec.evalSlice(t)

	case *listExpr:
		out := make([]any, 0, max(len(t.items), 1)) // its own backing array: its identity
		for _, item := range t.items {
			v, err := ec.evalItem(item)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		ec.own.mark(out)
		return out, nil

	case *dictExpr:
		// Build an ordered map so a dict literal keeps its written key order
		// (Python dicts preserve insertion order; e.g. {'b':1,'a':2}|to_json).
		out := yaml.NewOMap()
		for i := range t.keys {
			k, err := ec.eval(t.keys[i])
			if err != nil {
				return nil, err
			}
			ks, ok := asString(k)
			if !ok {
				return nil, ec.errf(t.keys[i].exprOff(), "dict keys must be strings, got %s", typeName(k))
			}
			v, err := ec.evalItem(t.vals[i])
			if err != nil {
				return nil, err
			}
			out.Set(ks, v)
		}
		ec.own.mark(out)
		return out, nil

	case *negExpr:
		x, err := ec.eval(t.x)
		if err != nil {
			return nil, err
		}
		if err := ec.rejectUndefined(x, t.off); err != nil {
			return nil, err
		}
		if !t.neg {
			if isNumber(x) {
				return x, nil
			}
			return nil, ec.errf(t.off, "bad operand type for unary +: %s", typeName(x))
		}
		if i, ok := asInt(x); ok && i != math.MinInt64 {
			return -i, nil
		}
		if b, ok := asBigInt(x); ok {
			return normInt(new(big.Int).Neg(b)), nil
		}
		if f, ok := x.(float64); ok {
			return -f, nil
		}
		return nil, ec.errf(t.off, "bad operand type for unary -: %s", typeName(x))

	case *notExpr:
		x, err := ec.eval(t.x)
		if err != nil {
			return nil, err
		}
		if err := ec.rejectUndefined(x, t.off); err != nil {
			return nil, err
		}
		return !truthy(x), nil

	case *binExpr:
		return ec.evalBin(t)

	case *compareExpr:
		return ec.evalCompare(t)

	case *condExpr:
		cond, err := ec.eval(t.cond)
		if err != nil {
			return nil, err
		}
		if err := ec.rejectUndefined(cond, t.off); err != nil {
			return nil, err
		}
		if truthy(cond) {
			return ec.eval(t.val)
		}
		if t.els == nil {
			return Undefined{Name: "the inline-if else branch"}, nil
		}
		return ec.eval(t.els)

	case *filterExpr:
		return ec.evalFilter(t)

	case *testExpr:
		return ec.evalTest(t)

	case *callExpr:
		return ec.evalCall(t)
	}
	return nil, ec.errf(e.exprOff(), "internal error: unknown expression node %T", e)
}

func (ec *EvalCtx) rejectUndefined(v any, off int) error {
	if u, ok := v.(Undefined); ok {
		return u.useError(ec.pos)
	}
	return nil
}

// getAttr implements a.b: mapping key first, then builtin methods.
func (ec *EvalCtx) getAttr(x any, name string, off int) (any, error) {
	x = ec.access(x)
	if u, ok := x.(Undefined); ok {
		return Undefined{Name: u.Name + "." + name, Err: u.Err}, nil
	}
	switch t := x.(type) {
	case map[string]any:
		if v, ok := t[name]; ok {
			return ec.access(v), nil
		}
	case Mapping:
		if v, ok := t.GetItem(name); ok {
			return ec.ownedChild(x, name, ec.access(v)), nil
		}
	}
	if o, ok := x.(pyObject); ok {
		if v, ok := o.PyAttr(name); ok {
			return v, nil
		}
	}
	if m, ok := lookupMethod(x, name); ok {
		return m, nil
	}
	return missingItem(x, name), nil
}

// PyTyped is a Mapping with its own Python class, which messages name
// (hostvars' HostVars and HostVarsVars).
type PyTyped interface{ PyTypeName() string }

// MissingItemer is a Mapping whose missing items are undefined values
// with their own message.
type MissingItemer interface{ MissingItem(key string) string }

// missingItem is the undefined value of x's missing item key.
func missingItem(x any, key string) Undefined {
	name := describeOwner(x) + "." + key
	if m, ok := x.(MissingItemer); ok {
		return Undefined{Name: name, Err: &UndefinedError{Name: name, Hint: m.MissingItem(key)}}
	}
	return Undefined{Name: name}
}

// getItem implements a[i] / a['key'].
func (ec *EvalCtx) getItem(x, idx any, off int) (any, error) {
	x, idx = ec.access(x), ec.access(idx)
	if u, ok := x.(Undefined); ok {
		if s, ok := asString(idx); ok {
			return Undefined{Name: u.Name + "." + s, Err: u.Err}, nil
		}
		return Undefined{Name: u.Name + "[...]", Err: u.Err}, nil
	}
	if err := ec.rejectUndefined(idx, off); err != nil {
		return nil, err
	}
	switch t := x.(type) {
	case map[string]any:
		s, ok := asString(idx)
		if !ok {
			return nil, ec.errf(off, "dict indices must be strings, got %s", typeName(idx))
		}
		if v, found := t[s]; found {
			return ec.access(v), nil
		}
		return Undefined{Name: describeOwner(x) + "." + s}, nil
	case Mapping:
		s, ok := asString(idx)
		if !ok {
			return nil, ec.errf(off, "dict indices must be strings, got %s", typeName(idx))
		}
		if v, found := t.GetItem(s); found {
			return ec.ownedChild(x, s, ec.access(v)), nil
		}
		return missingItem(x, s), nil
	case []any:
		i, ok := asInt(idx)
		if !ok {
			return nil, ec.errf(off, "list indices must be integers, got %s", typeName(idx))
		}
		n, orig := int64(len(t)), i
		if i < 0 {
			i += n
		}
		if i < 0 || i >= n {
			// Jinja's getitem falls back to getattr: a missing index is
			// undefined, not an error.
			return Undefined{Name: fmt.Sprintf("%s[%d]", describeOwner(x), orig)}, nil
		}
		return ec.ownedChild(x, i, ec.access(t[i])), nil
	case string:
		return stringIndex(ec, t, idx, off)
	case yaml.UnsafeString:
		return stringIndex(ec, string(t), idx, off)
	case *rangeValue:
		i, ok := asInt(idx)
		if !ok {
			return nil, ec.errf(off, "range indices must be integers")
		}
		n := t.length()
		if i < 0 {
			i += n
		}
		if i < 0 || i >= n {
			return nil, ec.errf(off, "range index out of range")
		}
		return t.start + i*t.step, nil
	}
	return nil, ec.errf(off, "%s object is not subscriptable", typeName(x))
}

func stringIndex(ec *EvalCtx, s string, idx any, off int) (any, error) {
	i, ok := asInt(idx)
	if !ok {
		return nil, ec.errf(off, "string indices must be integers, got %s", typeName(idx))
	}
	runes := []rune(s)
	n := int64(len(runes))
	if i < 0 {
		i += n
	}
	if i < 0 || i >= n {
		return nil, ec.errf(off, "string index out of range: %d (length %d)", i, n)
	}
	return string(runes[i]), nil
}

// describeOwner names the container in chained-undefined paths.
func describeOwner(x any) string {
	if Undeprecate(x) == nil {
		return "NoneType object"
	}
	if t, ok := x.(PyTyped); ok {
		return t.PyTypeName() + " object"
	}
	return typeName(x) + " object"
}

func (ec *EvalCtx) evalSlice(t *sliceExpr) (any, error) {
	x, err := ec.eval(t.x)
	if err != nil {
		return nil, err
	}
	if err := ec.rejectUndefined(x, t.off); err != nil {
		return nil, err
	}
	get := func(e Expr, def int64) (int64, error) {
		if e == nil {
			return def, nil
		}
		v, err := ec.eval(e)
		if err != nil {
			return 0, err
		}
		i, ok := asInt(v)
		if !ok {
			return 0, ec.errf(e.exprOff(), "slice indices must be integers, got %s", typeName(v))
		}
		return i, nil
	}

	step, err := get(t.st, 1)
	if err != nil {
		return nil, err
	}
	if step == 0 {
		return nil, ec.errf(t.off, "slice step cannot be zero")
	}

	slice := func(n int64) (from, to int64, err error) {
		defLo, defHi := int64(0), n
		if step < 0 {
			defLo, defHi = n-1, -n-1 // Python's sentinel for "before the start"
		}
		lo, err := get(t.lo, defLo)
		if err != nil {
			return 0, 0, err
		}
		hi, err := get(t.hi, defHi)
		if err != nil {
			return 0, 0, err
		}
		if lo < 0 {
			lo += n
		}
		if t.hi != nil && hi < 0 {
			hi += n
		}
		return lo, hi, nil
	}

	sliceList := func(items []any) ([]any, error) {
		n := int64(len(items))
		lo, hi, err := slice(n)
		if err != nil {
			return nil, err
		}
		out := []any{}
		if step > 0 {
			lo = max(lo, 0)
			hi = min(hi, n)
			for i := lo; i < hi; i += step {
				out = append(out, items[i])
			}
		} else {
			lo = min(lo, n-1)
			for i := lo; i >= 0 && i > hi; i += step {
				out = append(out, items[i])
			}
		}
		return out, nil
	}

	switch v := x.(type) {
	case []any:
		return sliceList(v)
	case string, yaml.UnsafeString:
		s, _ := asString(v)
		runes := []rune(s)
		items := make([]any, len(runes))
		for i, r := range runes {
			items[i] = string(r)
		}
		out, err := sliceList(items)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, r := range out {
			b.WriteString(r.(string))
		}
		return b.String(), nil
	}
	return nil, ec.errf(t.off, "%s object is not sliceable", typeName(x))
}

func (ec *EvalCtx) evalBin(t *binExpr) (any, error) {
	// and/or short-circuit and return the operand value (Python semantics).
	if t.op == tokName {
		switch t.opNm {
		case "and":
			l, err := ec.eval(t.l)
			if err != nil {
				return nil, err
			}
			if err := ec.rejectUndefined(l, t.off); err != nil {
				return nil, err
			}
			if !truthy(l) {
				return l, nil
			}
			r, err := ec.eval(t.r)
			if err != nil {
				return nil, err
			}
			if err := ec.rejectUndefined(r, t.off); err != nil {
				return nil, err
			}
			return r, nil
		case "or":
			l, err := ec.eval(t.l)
			if err != nil {
				return nil, err
			}
			if err := ec.rejectUndefined(l, t.off); err != nil {
				return nil, err
			}
			if truthy(l) {
				return l, nil
			}
			r, err := ec.eval(t.r)
			if err != nil {
				return nil, err
			}
			if err := ec.rejectUndefined(r, t.off); err != nil {
				return nil, err
			}
			return r, nil
		}
	}

	l, err := ec.eval(t.l)
	if err != nil {
		return nil, err
	}
	if err := ec.rejectUndefined(l, t.l.exprOff()); err != nil {
		return nil, err
	}
	r, err := ec.eval(t.r)
	if err != nil {
		return nil, err
	}
	if err := ec.rejectUndefined(r, t.r.exprOff()); err != nil {
		return nil, err
	}

	if t.op == tokTilde {
		return toStr(l) + toStr(r), nil
	}
	v, err := arith(t.op, l, r)
	if err != nil {
		return nil, ec.errf(t.off, "%s", err)
	}
	return v, nil
}

func (ec *EvalCtx) evalCompare(t *compareExpr) (any, error) {
	left, err := ec.eval(t.first)
	if err != nil {
		return nil, err
	}
	for i, op := range t.ops {
		right, err := ec.eval(t.rest[i])
		if err != nil {
			return nil, err
		}
		ok, err := ec.compareOnce(op, left, right, t.off)
		if err != nil {
			return nil, err
		}
		if !ok {
			return false, nil
		}
		left = right
	}
	return true, nil
}

func (ec *EvalCtx) compareOnce(op string, l, r any, off int) (bool, error) {
	// Comparing an undefined value raises (StrictUndefined's __eq__ too).
	if err := ec.rejectUndefined(l, off); err != nil {
		return false, err
	}
	if err := ec.rejectUndefined(r, off); err != nil {
		return false, err
	}
	if op == "==" {
		return equal(l, r), nil
	}
	if op == "!=" {
		return !equal(l, r), nil
	}
	switch op {
	case "in", "not in":
		found, err := contains(l, r)
		if err != nil {
			return false, ec.errf(off, "%s", err)
		}
		if op == "not in" {
			return !found, nil
		}
		return found, nil
	}
	c, err := compareOp(l, r, op)
	if err != nil {
		return false, ec.errf(off, "%s", err)
	}
	switch op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return false, ec.errf(off, "internal error: unknown comparison %q", op)
}

// undefinedTolerantFilters may receive an Undefined input.
var undefinedTolerantFilters = map[string]bool{
	"default": true, "d": true, "mandatory": true, "type_debug": true,
}

func (ec *EvalCtx) evalFilter(t *filterExpr) (any, error) {
	in, err := ec.eval(t.x)
	if err != nil {
		return nil, err
	}
	fn, ok := ec.engine.Filters[t.name]
	if !ok {
		// Compiling rejects an unknown filter outside if-statements and
		// conditional expressions; inside them it fails when called.
		return nil, ec.errf(t.off, "No filter named %s found.", pyStrRepr(t.full))
	}
	if isUndefined(in) && !undefinedTolerantFilters[t.name] {
		u := in.(Undefined)
		return nil, u.useError(ec.pos)
	}
	args, kwargs, err := ec.evalArgsMarkers(t.args, t.kwargs, argsMarkerFilters[t.name] && builtinPlugin(t.full))
	if err != nil {
		return nil, err
	}
	if err := checkArity(false, t.name, t.full, len(args), t.kwargs); err != nil {
		return nil, ec.pluginError("filter", t.full, err)
	}
	if !markerSafeFilters[t.name] {
		if err := ec.tripArgs(in, args, kwargs); err != nil {
			return nil, err
		}
	}
	// The filter reads some of the deprecated values it was given
	// (to_json all of them, dict2items the values); those it passes on
	// stay deprecated.
	ec.filterReads(t.name, in, args, kwargs)
	saved, savedKw := ec.filterVars, ec.callKwargs
	ec.callKwargs = make([]string, len(t.kwargs))
	for i, k := range t.kwargs {
		ec.callKwargs[i] = k.name
	}
	ec.filterVars = make([]bool, 1+len(t.args))
	ec.filterVars[0] = ec.isVarRef(t.x)
	for i, a := range t.args {
		ec.filterVars[i+1] = ec.isVarRef(a)
	}
	out, err := fn(ec, in, args, kwargs)
	ec.filterVars, ec.callKwargs = saved, savedKw
	if err != nil {
		if _, ok := err.(*TemplateError); ok {
			return nil, err
		}
		if _, ok := err.(*UndefinedError); ok {
			return nil, err
		}
		return nil, ec.pluginError("filter", t.full, err)
	}
	return out, nil
}

// pluginError is AnsibleTemplatePluginRuntimeError: "The filter plugin
// 'ansible.builtin.combine' failed", caused by the plugin's exception.
func (ec *EvalCtx) pluginError(kind, name string, err error) error {
	if !strings.Contains(name, ".") {
		name = "ansible.builtin." + name
	}
	head := fmt.Sprintf("The %s plugin %s failed.", kind, pyStrRepr(name))
	detail := err.Error()
	msg := head
	if !strings.HasSuffix(head, detail) {
		msg = strings.TrimRight(head, ". ") + ": " + detail
	}
	te := &TemplateError{Pos: ec.pos, Msg: msg, Src: ec.src, Plugin: true}
	var he *handlingError
	var oe *objError
	if errors.As(err, &he) {
		te.pluginHead, te.pluginDetail = head, detail
	} else if errors.As(err, &oe) {
		h := head
		for _, p := range oe.pre {
			h = strings.TrimRight(h, ". ") + ": " + p
		}
		te.pluginHead, te.pluginDetail, te.pluginValue = h, oe.msg, oe.value
	}
	return te
}

// undefinedTolerantTests may receive an Undefined input.
var undefinedTolerantTests = map[string]bool{
	"defined": true, "undefined": true, "none": true,
}

func (ec *EvalCtx) evalTest(t *testExpr) (any, error) {
	if t.name == "vault_encrypted" && builtinPlugin(t.full) && len(t.args) == 0 && len(t.kwargs) == 0 {
		// A !vault variable is vault-encrypted, whether or not it would
		// decrypt.
		if raw, ok := ec.rawValue(t.x); ok {
			if _, vaulted := raw.(yaml.VaultedString); vaulted {
				return !t.negated, nil
			}
		}
	}
	in, err := ec.eval(t.x)
	if err != nil {
		return nil, err
	}
	fn, ok := ec.engine.Tests[t.name]
	if !ok {
		return nil, ec.errf(t.off, "No test named %s found.", pyStrRepr(t.full))
	}
	if isUndefined(in) && !undefinedTolerantTests[t.name] {
		u := in.(Undefined)
		return nil, u.useError(ec.pos)
	}
	args, kwargs, err := ec.evalArgs(t.args, t.kwargs)
	if err != nil {
		return nil, err
	}
	if err := checkArity(true, t.name, t.full, len(args), t.kwargs); err != nil {
		return nil, ec.pluginError("test", t.full, err)
	}
	if !markerSafeTests[t.name] {
		if err := ec.tripArgs(in, args, kwargs); err != nil {
			return nil, err
		}
	}
	saved := ec.testKwargs
	ec.testKwargs = kwargs
	res, err := fn(ec, in, args)
	ec.testKwargs = saved
	if err != nil {
		if _, ok := err.(*TemplateError); ok {
			return nil, err
		}
		if _, ok := err.(*UndefinedError); ok {
			return nil, err
		}
		return nil, ec.pluginError("test", t.full, err)
	}
	if t.negated {
		return !res, nil
	}
	return res, nil
}

func (ec *EvalCtx) evalArgs(argExprs []Expr, kwargExprs []kwarg) ([]any, map[string]any, error) {
	return ec.evalArgsMarkers(argExprs, kwargExprs, false)
}

// argsMarkerFilters take undefined arguments (ansible-core's
// accept_args_markers): default(x.y) is not an error until the default
// is used.
var argsMarkerFilters = map[string]bool{
	"default": true, "d": true, "ternary": true, "mandatory": true, "type_debug": true,
}

// evalArgsMarkers is evalArgs keeping undefined arguments (markers) when
// the plugin accepts them.
func (ec *EvalCtx) evalArgsMarkers(argExprs []Expr, kwargExprs []kwarg, markers bool) ([]any, map[string]any, error) {
	args := make([]any, 0, len(argExprs))
	for _, a := range argExprs {
		v, err := ec.eval(a)
		if err != nil {
			return nil, nil, err
		}
		if !markers {
			if err := ec.rejectUndefined(v, a.exprOff()); err != nil {
				return nil, nil, err
			}
		}
		args = append(args, v)
	}
	var kwargs map[string]any
	if len(kwargExprs) > 0 {
		kwargs = make(map[string]any, len(kwargExprs))
		for _, kw := range kwargExprs {
			v, err := ec.eval(kw.val)
			if err != nil {
				return nil, nil, err
			}
			if !markers {
				if err := ec.rejectUndefined(v, kw.val.exprOff()); err != nil {
					return nil, nil, err
				}
			}
			kwargs[kw.name] = v
		}
	}
	return args, kwargs, nil
}

func (ec *EvalCtx) evalCall(t *callExpr) (any, error) {
	if ga, ok := t.fn.(*getAttrExpr); ok && (listMutators[ga.name] != nil || dictMutators[ga.name] != nil) {
		if out, handled, err := ec.callMutator(t, ga); handled {
			return out, err
		}
	}
	// Method calls (x.upper()) are boundMethod values from getAttr.
	fn, err := ec.eval(t.fn)
	if err != nil {
		return nil, err
	}
	if err := ec.rejectUndefined(fn, t.off); err != nil {
		return nil, err
	}
	switch f := fn.(type) {
	case *globalValue:
		fn = f.fn
	case *methodValue:
		fn = f.call
	}
	if f, ok := fn.(kwOrderFunc); ok {
		// Keyword arguments in call order (dict(b=1, a=2) keeps b first).
		args, _, err := ec.evalArgs(t.args, nil)
		if err != nil {
			return nil, err
		}
		kw := yaml.NewOMap()
		for _, k := range t.kwargs {
			v, err := ec.eval(k.val)
			if err != nil {
				return nil, err
			}
			if err := ec.rejectUndefined(v, k.val.exprOff()); err != nil {
				return nil, err
			}
			kw.Set(k.name, v)
		}
		out, err := f(ec, args, kw)
		if err != nil {
			return nil, ec.errf(t.off, "%s", err)
		}
		return out, nil
	}
	args, kwargs, err := ec.evalArgs(t.args, t.kwargs)
	if err != nil {
		return nil, err
	}
	switch f := fn.(type) {
	case *macroValue:
		return f.call(ec, args, kwargs)
	case *loopValue:
		if len(args) != 1 {
			return nil, ec.errf(t.off, "loop() takes exactly one argument")
		}
		return f.recurse(args[0])
	case boundMethod:
		out, err := f(ec, args, kwargs)
		if err != nil {
			return nil, ec.errf(t.off, "%s", err)
		}
		return out, nil
	case globalFunc:
		out, err := f(ec, args, kwargs)
		if err != nil {
			switch err.(type) {
			case *TemplateError, *UndefinedError:
				return nil, err
			}
			return nil, ec.errf(t.off, "%s", err)
		}
		return out, nil
	}
	return nil, ec.errf(t.off, "%s object is not callable", typeName(fn))
}

// evalItem evaluates an item of a list or dict literal: an undefined one
// stays, an error only where the container's items are used (and one
// whose evaluation failed is a marker where markers are replaced, as
// debug's var= does).
func (ec *EvalCtx) evalItem(e Expr) (any, error) {
	if ec.replaceMarkers {
		return ec.evalMarking(e)
	}
	return ec.eval(e)
}

// tripArgs is the error a plugin using undefined items of its input or
// arguments raises.
func (ec *EvalCtx) tripArgs(in any, args []any, kwargs map[string]any) error {
	if _, isU := in.(Undefined); !isU {
		if err := tripMarkers(in, ec.pos); err != nil {
			return err
		}
	}
	for _, a := range args {
		if _, isU := a.(Undefined); isU {
			continue
		}
		if err := tripMarkers(a, ec.pos); err != nil {
			return err
		}
	}
	for _, k := range sortedKeys(kwargs) {
		if _, isU := kwargs[k].(Undefined); isU {
			continue
		}
		if err := tripMarkers(kwargs[k], ec.pos); err != nil {
			return err
		}
	}
	return nil
}
