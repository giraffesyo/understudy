package template

import (
	"fmt"
	"math/big"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Filter errors name values by their Python class, as the exception a
// plugin raises does. A value read from a variable is not the plain
// type ansible-core's plugins see: containers are lazy templating
// wrappers and scalars loaded from YAML carry tags. The engine tracks
// which of a filter's arguments are variable references; values computed
// in the template are plain.

// isVarRef reports whether e reads a variable (a name that is not a
// template local, or an attribute or item of one).
func (ec *EvalCtx) isVarRef(e Expr) bool {
	switch t := e.(type) {
	case *nameExpr:
		for s := ec; s != nil; s = s.parent {
			if _, ok := s.locals[t.name]; ok {
				return false
			}
		}
		if _, ok := ec.engine.Globals[t.name]; ok {
			if ec.vars == nil {
				return false
			}
			if _, isVar := ec.vars.Get(t.name); !isVar {
				return false
			}
		}
		return true
	case *getAttrExpr:
		return ec.isVarRef(t.x)
	case *getItemExpr:
		return ec.isVarRef(t.x)
	}
	return false
}

// fromVar reports whether the filter being called got argument i (-1:
// its input) from a variable.
func (ec *EvalCtx) fromVar(i int) bool {
	i++
	return i >= 0 && i < len(ec.filterVars) && ec.filterVars[i]
}

// pyClassName is the class name of v as a plugin sees it (fromVar: read
// from a variable).
func pyClassName(v any, fromVar bool) string {
	v = Undeprecate(v)
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		if fromVar {
			return "_AnsibleTaggedInt"
		}
		return "int"
	case float64:
		if fromVar {
			return "_AnsibleTaggedFloat"
		}
		return "float"
	case string:
		if _, _, _, ok := yaml.Origin(t); ok || fromVar {
			return "_AnsibleTaggedStr"
		}
		return "str"
	case yaml.UnsafeString:
		if fromVar {
			return "_AnsibleTaggedStr"
		}
		return "str"
	case []any:
		if fromVar {
			return "_AnsibleLazyTemplateList"
		}
		return "list"
	case Undefined:
		return "AnsibleUndefined"
	}
	if _, ok := v.(Mapping); ok || isMap(v) {
		if fromVar {
			return "_AnsibleLazyTemplateDict"
		}
		return "dict"
	}
	return typeName(v)
}

func isMap(v any) bool {
	_, ok := anyToMap(v)
	return ok
}

// pyTypeRepr is repr(type(v)): "<class 'int'>", the tagged and lazy
// classes with their module.
func pyTypeRepr(v any, fromVar bool) string {
	name := pyClassName(v, fromVar)
	switch name {
	case "_AnsibleTaggedInt", "_AnsibleTaggedFloat", "_AnsibleTaggedStr":
		return "<class 'ansible.module_utils._internal._datatag." + name + "'>"
	case "_AnsibleLazyTemplateList", "_AnsibleLazyTemplateDict":
		return "<class 'ansible._internal._templating._lazy_containers." + name + "'>"
	}
	return "<class '" + name + "'>"
}

// isFlattenNull is the flatten filter's null check: element in (None,
// 'None', 'null').
func isFlattenNull(v any) bool {
	v = Undeprecate(v)
	if v == nil {
		return true
	}
	s, ok := asString(v)
	return ok && (s == "None" || s == "null")
}

// errNotIterable is Python's TypeError iterating a non-iterable.
func errNotIterable(v any, fromVar bool) error {
	return fmt.Errorf("'%s' object is not iterable", pyClassName(v, fromVar))
}
