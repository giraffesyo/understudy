package actions

import (
	"reflect"
	"testing"
)

// Messages observed from ansible-core 2.21's ArgumentSpecValidator.
func TestValidateArgSpecMessages(t *testing.T) {
	spec := map[string]any{
		"am_value": map[string]any{"type": "int", "required": true},
	}
	got := validateArgSpec(spec, map[string]any{"am_value": "not-a-number"}, nil)
	want := []string{`argument 'am_value' is of type str and we were unable to convert to int: "'not-a-number'" cannot be converted to an int`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("int: got %q", got)
	}

	spec = map[string]any{
		"as_mode": map[string]any{"type": "str", "choices": []any{"fast", "slow"}},
		"as_conf": map[string]any{"type": "dict", "options": map[string]any{
			"level": map[string]any{"type": "int", "required": true},
			"label": map[string]any{"type": "str"},
		}},
	}
	got = validateArgSpec(spec, map[string]any{
		"as_mode": "medium",
		"as_conf": map[string]any{"label": "x"},
	}, nil)
	want = []string{
		"value of as_mode must be one of: fast, slow, got: medium",
		"missing required arguments: level found in as_conf",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("choices/nested: got %q", got)
	}

	got = validateArgSpec(map[string]any{"a": map[string]any{"required": true}}, map[string]any{"b": 1}, nil)
	want = []string{"missing required arguments: a", "b. Supported parameters include: a."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("required/unsupported: got %q", got)
	}

	// Lenient conversions Ansible accepts must pass.
	ok := validateArgSpec(map[string]any{
		"p": map[string]any{"type": "int"},
		"b": map[string]any{"type": "bool"},
		"l": map[string]any{"type": "list", "elements": "str"},
		"s": map[string]any{"type": "str"},
	}, map[string]any{"p": "8080", "b": "yes", "l": "a,b", "s": int64(3)}, nil)
	if len(ok) != 0 {
		t.Errorf("lenient: got %q", ok)
	}
}
