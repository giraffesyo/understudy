// Package args provides declarative module-argument validation with
// Ansible's coercion rules ("yes" -> true, "0644" already int from YAML,
// numeric strings -> int where an int is requested).
package args

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Spec maps argument names to their definitions.
type Spec map[string]Def

// Def describes one argument.
type Def struct {
	Type     string // "str" (default), "bool", "int", "list", "dict", "any"
	Required bool
	Default  any
	Choices  []string
	Aliases  []string
}

// Parsed provides typed access to validated arguments.
type Parsed struct {
	values map[string]any
}

// Parse validates raw args against the spec.
func (s Spec) Parse(raw map[string]any) (*Parsed, error) {
	values := map[string]any{}

	// Canonicalize aliases.
	canonical := map[string]string{}
	for name, def := range s {
		canonical[name] = name
		for _, alias := range def.Aliases {
			canonical[alias] = name
		}
	}

	var unknown []string
	for key, val := range raw {
		name, ok := canonical[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		if _, dup := values[name]; dup {
			return nil, fmt.Errorf("both %q and an alias were given", name)
		}
		values[name] = val
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("Unsupported parameters: %s", strings.Join(unknown, ", "))
	}

	// Ansible's check_required_arguments: every missing one, sorted.
	var missing []string
	for name, def := range s {
		if v, present := values[name]; def.Required && (!present || v == nil) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required arguments: %s", strings.Join(missing, ", "))
	}

	for name, def := range s {
		val, present := values[name]
		if !present || val == nil {
			if def.Default != nil {
				values[name] = def.Default
			}
			continue
		}
		coerced, err := coerce(val, def.Type)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %v", name, err)
		}
		if len(def.Choices) > 0 {
			s, _ := coerced.(string)
			ok := false
			for _, c := range def.Choices {
				if s == c {
					ok = true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf("value of %s must be one of: %s, got: %v",
					name, strings.Join(def.Choices, ", "), val)
			}
		}
		values[name] = coerced
	}
	return &Parsed{values: values}, nil
}

func coerce(v any, typ string) (any, error) {
	switch typ {
	case "", "str":
		switch t := v.(type) {
		case string:
			return t, nil
		case bool:
			if t {
				return "yes", nil
			}
			return "no", nil
		case int64:
			return strconv.FormatInt(t, 10), nil
		case int:
			return strconv.Itoa(t), nil
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64), nil
		}
		return nil, fmt.Errorf("expected a string, got %T", v)
	case "bool":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			switch strings.ToLower(t) {
			case "yes", "on", "1", "true":
				return true, nil
			case "no", "off", "0", "false":
				return false, nil
			}
			return nil, fmt.Errorf("%q is not a valid boolean", t)
		case int64:
			return t != 0, nil
		}
		return nil, fmt.Errorf("cannot interpret %T as a boolean", v)
	case "int":
		switch t := v.(type) {
		case int64:
			return t, nil
		case int:
			return int64(t), nil
		case float64:
			return int64(t), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a valid integer", t)
			}
			return n, nil
		}
		return nil, fmt.Errorf("cannot interpret %T as an integer", v)
	case "list":
		switch t := v.(type) {
		case []any:
			return t, nil
		case string:
			// Ansible: comma-separated string becomes a list.
			parts := strings.Split(t, ",")
			out := make([]any, 0, len(parts))
			for _, p := range parts {
				out = append(out, strings.TrimSpace(p))
			}
			return out, nil
		}
		return []any{v}, nil
	case "dict":
		if m, ok := v.(map[string]any); ok {
			return m, nil
		}
		return nil, fmt.Errorf("expected a dict, got %T", v)
	case "any":
		return v, nil
	}
	return nil, fmt.Errorf("internal error: unknown spec type %q", typ)
}

// Str returns a string argument ("" if absent).
func (p *Parsed) Str(name string) string {
	s, _ := p.values[name].(string)
	return s
}

// Has reports whether the argument was provided (or defaulted).
func (p *Parsed) Has(name string) bool {
	v, ok := p.values[name]
	return ok && v != nil
}

// Bool returns a bool argument (false if absent).
func (p *Parsed) Bool(name string) bool {
	b, _ := p.values[name].(bool)
	return b
}

// Int returns an int argument (0 if absent).
func (p *Parsed) Int(name string) int64 {
	n, _ := p.values[name].(int64)
	return n
}

// List returns a list argument (nil if absent).
func (p *Parsed) List(name string) []any {
	l, _ := p.values[name].([]any)
	return l
}

// Dict returns a dict argument (nil if absent).
func (p *Parsed) Dict(name string) map[string]any {
	m, _ := p.values[name].(map[string]any)
	return m
}

// Any returns the raw value.
func (p *Parsed) Any(name string) any { return p.values[name] }
