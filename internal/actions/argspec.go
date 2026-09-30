package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

func init() {
	Register("validate_argument_spec", actionFunc(runValidateArgumentSpec))
}

// runValidateArgumentSpec is ansible.builtin.validate_argument_spec, also
// the implicit first task of a role that ships meta/argument_specs.yml.
// Values come from the task vars named in the spec (role defaults, play
// vars, ...), overridden by provided_arguments (role params).
func runValidateArgumentSpec(_ context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	res := &agentproto.Result{Extra: map[string]any{}}
	vctx, _ := yaml.PlainMap(args["validate_args_context"])
	if vctx == nil {
		vctx = map[string]any{}
	}
	res.Extra["validate_args_context"] = vctx
	rawSpec, ok := args["argument_spec"]
	if !ok {
		return actionRaise("\"argument_spec\" arg is required in args: %s", template.PyRepr(args))
	}
	spec, ok := yaml.PlainMap(rawSpec)
	if !ok {
		return actionRaise("Incorrect type for argument_spec, expected dict and got %s", pyTypeName(rawSpec))
	}
	provided := map[string]any{}
	if p, ok := args["provided_arguments"]; ok && p != nil {
		m, ok := yaml.PlainMap(p)
		if !ok {
			return actionRaise("Incorrect type for provided_arguments, expected dict and got %s", pyTypeName(p))
		}
		provided = m
	}
	params := map[string]any{}
	if actx.Vars != nil {
		for name := range spec {
			if v, ok := actx.Vars.Get(name); ok {
				params[name] = v
			}
		}
	}
	for k, v := range provided {
		params[k] = v
	}
	errs := validateArgSpec(spec, params, nil)
	if len(errs) > 0 {
		res.Failed = true
		res.Msg = "Validation of arguments failed:\n" + strings.Join(errs, "\n")
		res.Extra["argument_spec_data"] = spec
		list := make([]any, len(errs))
		for i, e := range errs {
			list[i] = e
		}
		res.Extra["argument_errors"] = list
		return res
	}
	res.Msg = "The arg spec validation passed"
	return res
}

func pyTypeName(v any) string {
	switch v.(type) {
	case string:
		return "<class 'str'>"
	case bool:
		return "<class 'bool'>"
	case int, int64:
		return "<class 'int'>"
	case float64:
		return "<class 'float'>"
	case []any:
		return "<class 'list'>"
	case nil:
		return "<class 'NoneType'>"
	}
	if _, ok := yaml.PlainMap(v); ok {
		return "<class 'dict'>"
	}
	return "<class 'object'>"
}

func nativeTypeName(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case bool:
		return "bool"
	case int, int64:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case nil:
		return "NoneType"
	}
	if _, ok := yaml.PlainMap(v); ok {
		return "dict"
	}
	return "object"
}

func sortedSpecKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func specList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, template.PyStr(e))
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

func withContext(msg string, ctx []string) string {
	if len(ctx) > 0 {
		return msg + " found in " + strings.Join(ctx, " -> ")
	}
	return msg
}

// validateArgSpec is ArgumentSpecValidator.validate for role/argspec use
// (a port of the common path of module_utils/common/parameters.py):
// aliases, unsupported parameters, required, type conversion, choices,
// elements, and nested options. It returns the error messages in
// ansible-core's order.
func validateArgSpec(spec, params map[string]any, ctx []string) []string {
	var errs []string
	defs := map[string]map[string]any{}
	for _, name := range sortedSpecKeys(spec) {
		d, _ := yaml.PlainMap(spec[name])
		if d == nil {
			d = map[string]any{}
		}
		defs[name] = d
	}
	// Aliases.
	legal := map[string]bool{}
	var supported, aliasNames []string
	for _, name := range sortedSpecKeys(spec) {
		legal[name] = true
		supported = append(supported, name)
		for _, a := range specList(defs[name]["aliases"]) {
			legal[a] = true
			aliasNames = append(aliasNames, a)
			if v, ok := params[a]; ok {
				if _, has := params[name]; !has {
					params[name] = v
				}
			}
		}
	}
	// Unsupported parameters.
	var unsupported []string
	for k := range params {
		if !legal[k] {
			unsupported = append(unsupported, k)
		}
	}
	// Required.
	var missing []string
	for name, d := range defs {
		if truthyBool(d["required"]) {
			if _, ok := params[name]; !ok {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		errs = append(errs, withContext("missing required arguments: "+strings.Join(missing, ", "), ctx))
	}
	// Types.
	for _, name := range sortedSpecKeys(spec) {
		d := defs[name]
		v, ok := params[name]
		if !ok {
			continue
		}
		if v == nil && !truthyBool(d["required"]) && d["default"] == nil {
			continue
		}
		wanted := "str"
		if t, ok := d["type"].(string); ok && t != "" {
			wanted = t
		}
		conv, err := convertArgType(wanted, v)
		if err != nil {
			msg := fmt.Sprintf("argument '%s' is of type %s", name, nativeTypeName(v))
			if len(ctx) > 0 {
				msg += fmt.Sprintf(" found in '%s'.", strings.Join(ctx, " -> "))
			}
			msg += fmt.Sprintf(" and we were unable to convert to %s: %s", wanted, err)
			errs = append(errs, msg)
			continue
		}
		params[name] = conv
		if el, ok := d["elements"].(string); ok && el != "" {
			if list, ok := conv.([]any); ok {
				for i, e := range list {
					if el == "dict" && d["options"] != nil {
						continue // validated with the sub spec below
					}
					c, err := convertArgType(el, e)
					if err != nil {
						msg := fmt.Sprintf("Elements value for option '%s'", name)
						if len(ctx) > 0 {
							msg += fmt.Sprintf(" found in '%s'", strings.Join(ctx, " -> "))
						}
						msg += fmt.Sprintf(" is of type %s and we were unable to convert to %s: %s", nativeTypeName(e), el, err)
						errs = append(errs, msg)
						continue
					}
					list[i] = c
				}
			}
		}
	}
	// Choices.
	for _, name := range sortedSpecKeys(spec) {
		choices, ok := defs[name]["choices"].([]any)
		if !ok {
			continue
		}
		v, ok := params[name]
		if !ok {
			continue
		}
		in := func(x any) bool {
			for _, c := range choices {
				if template.PyStr(c) == template.PyStr(x) && sameKind(c, x) {
					return true
				}
			}
			return false
		}
		var cs []string
		for _, c := range choices {
			cs = append(cs, template.PyStr(c))
		}
		if list, ok := v.([]any); ok {
			var diff []string
			for _, x := range list {
				if !in(x) {
					diff = append(diff, template.PyStr(x))
				}
			}
			if len(diff) > 0 {
				errs = append(errs, withContext(fmt.Sprintf("value of %s must be one or more of: %s. Got no match for: %s",
					name, strings.Join(cs, ", "), strings.Join(diff, ", ")), ctx))
			}
		} else if !in(v) {
			errs = append(errs, withContext(fmt.Sprintf("value of %s must be one of: %s, got: %s",
				name, strings.Join(cs, ", "), template.PyStr(v)), ctx))
		}
	}
	// Nested options.
	for _, name := range sortedSpecKeys(spec) {
		sub, ok := yaml.PlainMap(defs[name]["options"])
		if !ok {
			continue
		}
		v, ok := params[name]
		if !ok || v == nil {
			continue
		}
		var items []any
		if list, ok := v.([]any); ok {
			items = list
		} else {
			items = []any{v}
		}
		for _, it := range items {
			m, ok := yaml.PlainMap(it)
			if !ok {
				continue
			}
			sctx := append(append([]string{}, ctx...), name)
			cp := make(map[string]any, len(m))
			for k, x := range m {
				cp[k] = x
			}
			errs = append(errs, validateArgSpec(sub, cp, sctx)...)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		sort.Strings(aliasNames)
		s := strings.Join(supported, ", ")
		if len(aliasNames) > 0 {
			s += " (" + strings.Join(aliasNames, ", ") + ")"
		}
		names := unsupported
		if len(ctx) > 0 {
			names = nil
			for _, u := range unsupported {
				names = append(names, strings.Join(ctx, ".")+"."+u)
			}
		}
		errs = append(errs, fmt.Sprintf("%s. Supported parameters include: %s.", strings.Join(names, ", "), s))
	}
	return errs
}

func sameKind(a, b any) bool {
	_, as := a.(string)
	_, bs := b.(string)
	return as == bs
}

func truthyBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(t) {
		case "yes", "true", "on", "1", "y", "t":
			return true
		}
	case int64:
		return t != 0
	}
	return false
}

// convertArgType is the check_type_* family (lenient where Ansible is).
func convertArgType(wanted string, v any) (any, error) {
	switch wanted {
	case "str", "raw", "":
		return v, nil
	case "path":
		if s, ok := v.(string); ok {
			if strings.HasPrefix(s, "~") {
				if home, err := os.UserHomeDir(); err == nil && (s == "~" || strings.HasPrefix(s, "~/")) {
					s = home + s[1:]
				}
			}
			return os.ExpandEnv(s), nil
		}
		return v, nil
	case "list":
		switch t := v.(type) {
		case []any:
			return t, nil
		case string:
			parts := strings.Split(t, ",")
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out, nil
		case int64, float64:
			return []any{template.PyStr(t)}, nil
		}
		return nil, fmt.Errorf("%s cannot be converted to a list", pyTypeName(v))
	case "dict":
		if _, ok := yaml.PlainMap(v); ok {
			return v, nil
		}
		if s, ok := v.(string); ok {
			if strings.HasPrefix(strings.TrimSpace(s), "{") {
				var m map[string]any
				if err := json.Unmarshal([]byte(s), &m); err == nil {
					return m, nil
				}
			}
			m := map[string]any{}
			for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
				k, val, ok := strings.Cut(f, "=")
				if !ok {
					return nil, fmt.Errorf("dictionary requested, could not parse JSON or key=value")
				}
				m[strings.TrimSpace(k)] = strings.TrimSpace(val)
			}
			return m, nil
		}
		return nil, fmt.Errorf("%s cannot be converted to a dict", pyTypeName(v))
	case "bool":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string, int64, float64:
			s := strings.ToLower(template.PyStr(t))
			switch s {
			case "y", "yes", "on", "1", "true", "t", "1.0":
				return true, nil
			case "n", "no", "off", "0", "false", "f", "0.0", "":
				return false, nil
			}
			return nil, fmt.Errorf("The value %s is not a valid boolean.  Valid booleans include: 'y', 'yes', 'on', '1', 'true', 't', 1, 1.0, True, 'n', 'no', 'off', '0', 'false', 'f', 0, 0.0, False", template.PyRepr(t))
		}
		return nil, fmt.Errorf("%s cannot be converted to a bool", pyTypeName(v))
	case "int":
		switch t := v.(type) {
		case int64:
			return t, nil
		case float64:
			if t != math.Trunc(t) {
				return nil, fmt.Errorf("Significant decimal part found")
			}
			return int64(t), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("\"%s\" cannot be converted to an int", template.PyRepr(t))
			}
			return n, nil
		}
		return nil, fmt.Errorf("%s cannot be converted to an int", pyTypeName(v))
	case "float":
		switch t := v.(type) {
		case float64:
			return t, nil
		case int64:
			return float64(t), nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err != nil {
				return nil, fmt.Errorf("%s cannot be converted to a float", pyTypeName(v))
			}
			return f, nil
		}
		return nil, fmt.Errorf("%s cannot be converted to a float", pyTypeName(v))
	case "json", "jsonarg":
		if s, ok := v.(string); ok {
			return s, nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
	return v, nil // bytes, bits, and unknown types: accepted as-is
}
