package template

import (
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
		return ec.getAttr(x, t.name, t.off)

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
		out := make([]any, 0, len(t.items))
		for _, item := range t.items {
			v, err := ec.eval(item)
			if err != nil {
				return nil, err
			}
			if u, ok := v.(Undefined); ok {
				return nil, &UndefinedError{Pos: ec.pos, Name: u.Name}
			}
			out = append(out, v)
		}
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
			v, err := ec.eval(t.vals[i])
			if err != nil {
				return nil, err
			}
			out.Set(ks, v)
		}
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
		if i, ok := asInt(x); ok {
			return -i, nil
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
		return &UndefinedError{Pos: ec.pos, Name: u.Name}
	}
	return nil
}

// getAttr implements a.b: mapping key first, then builtin methods.
func (ec *EvalCtx) getAttr(x any, name string, off int) (any, error) {
	x = ec.access(x)
	if u, ok := x.(Undefined); ok {
		return Undefined{Name: u.Name + "." + name}, nil
	}
	switch t := x.(type) {
	case map[string]any:
		if v, ok := t[name]; ok {
			return ec.access(v), nil
		}
	case Mapping:
		if v, ok := t.GetItem(name); ok {
			return ec.access(v), nil
		}
	}
	if m, ok := lookupMethod(x, name); ok {
		return m, nil
	}
	return Undefined{Name: describeOwner(x) + "." + name}, nil
}

// getItem implements a[i] / a['key'].
func (ec *EvalCtx) getItem(x, idx any, off int) (any, error) {
	x, idx = ec.access(x), ec.access(idx)
	if u, ok := x.(Undefined); ok {
		if s, ok := asString(idx); ok {
			return Undefined{Name: u.Name + "." + s}, nil
		}
		return Undefined{Name: u.Name + "[...]"}, nil
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
			return ec.access(v), nil
		}
		return Undefined{Name: describeOwner(x) + "." + s}, nil
	case []any:
		i, ok := asInt(idx)
		if !ok {
			return nil, ec.errf(off, "list indices must be integers, got %s", typeName(idx))
		}
		n := int64(len(t))
		if i < 0 {
			i += n
		}
		if i < 0 || i >= n {
			return nil, ec.errf(off, "list index out of range: %d (length %d)", i, n)
		}
		return ec.access(t[i]), nil
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
	// == and != tolerate Undefined (it equals only another Undefined);
	// ordering comparisons reject it.
	if op == "==" {
		return equal(l, r), nil
	}
	if op == "!=" {
		return !equal(l, r), nil
	}
	if err := ec.rejectUndefined(l, off); err != nil {
		return false, err
	}
	if err := ec.rejectUndefined(r, off); err != nil {
		return false, err
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
	c, err := compare(l, r)
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
		return nil, ec.errf(t.off, "no filter named %q", t.name)
	}
	if isUndefined(in) && !undefinedTolerantFilters[t.name] {
		u := in.(Undefined)
		return nil, &UndefinedError{Pos: ec.pos, Name: u.Name}
	}
	args, kwargs, err := ec.evalArgs(t.args, t.kwargs)
	if err != nil {
		return nil, err
	}
	if !passThroughFilters[t.name] {
		// The filter reads the deprecated values it was given (to_json,
		// dict2items, ...); those it passes on stay deprecated.
		walkDeprecated(in, ec.deprecated, false)
	}
	out, err := fn(ec, in, args, kwargs)
	if err != nil {
		if _, ok := err.(*TemplateError); ok {
			return nil, err
		}
		if _, ok := err.(*UndefinedError); ok {
			return nil, err
		}
		return nil, ec.errf(t.off, "filter %q: %s", t.name, err)
	}
	return out, nil
}

// passThroughFilters return their input or only measure it: deprecated
// values inside pass through unread.
var passThroughFilters = map[string]bool{
	"default": true, "d": true, "mandatory": true, "length": true, "count": true,
}

// undefinedTolerantTests may receive an Undefined input.
var undefinedTolerantTests = map[string]bool{
	"defined": true, "undefined": true, "none": true,
}

func (ec *EvalCtx) evalTest(t *testExpr) (any, error) {
	in, err := ec.eval(t.x)
	if err != nil {
		return nil, err
	}
	fn, ok := ec.engine.Tests[t.name]
	if !ok {
		return nil, ec.errf(t.off, "no test named %q", t.name)
	}
	if isUndefined(in) && !undefinedTolerantTests[t.name] {
		u := in.(Undefined)
		return nil, &UndefinedError{Pos: ec.pos, Name: u.Name}
	}
	args, _, err := ec.evalArgs(t.args, nil)
	if err != nil {
		return nil, err
	}
	res, err := fn(ec, in, args)
	if err != nil {
		if _, ok := err.(*TemplateError); ok {
			return nil, err
		}
		return nil, ec.errf(t.off, "test %q: %s", t.name, err)
	}
	if t.negated {
		return !res, nil
	}
	return res, nil
}

func (ec *EvalCtx) evalArgs(argExprs []Expr, kwargExprs []kwarg) ([]any, map[string]any, error) {
	args := make([]any, 0, len(argExprs))
	for _, a := range argExprs {
		v, err := ec.eval(a)
		if err != nil {
			return nil, nil, err
		}
		if err := ec.rejectUndefined(v, a.exprOff()); err != nil {
			return nil, nil, err
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
			if err := ec.rejectUndefined(v, kw.val.exprOff()); err != nil {
				return nil, nil, err
			}
			kwargs[kw.name] = v
		}
	}
	return args, kwargs, nil
}

func (ec *EvalCtx) evalCall(t *callExpr) (any, error) {
	// Method calls (x.upper()) are boundMethod values from getAttr.
	fn, err := ec.eval(t.fn)
	if err != nil {
		return nil, err
	}
	if err := ec.rejectUndefined(fn, t.off); err != nil {
		return nil, err
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
			if _, ok := err.(*TemplateError); ok {
				return nil, err
			}
			return nil, ec.errf(t.off, "%s", err)
		}
		return out, nil
	}
	return nil, ec.errf(t.off, "%s object is not callable", typeName(fn))
}
