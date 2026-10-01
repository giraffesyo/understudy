package yaml

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// KV is one ordered mapping entry for Marshal.
type KV struct {
	K string
	V any
}

// OrderedMap is a mapping whose key order Marshal preserves (plain maps
// emit sorted). Decode never produces it; it exists for renderers that
// care about human-readable key ordering (playbook generation).
type OrderedMap []KV

// Marshal renders v as block-style YAML for generated playbooks
// (Playbook.YAML). Mapping keys are sorted (use OrderedMap to control
// ordering). It does not aim to round-trip styles or comments — it
// produces clean output that this package can re-parse. The to_yaml
// filters use Dump, which reproduces PyYAML's output exactly.
func Marshal(v any, indent int) ([]byte, error) {
	if indent <= 0 {
		indent = 2
	}
	var b strings.Builder
	if err := emitValue(&b, v, 0, indent, true); err != nil {
		return nil, err
	}
	if b.Len() == 0 || b.String()[b.Len()-1] != '\n' {
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

func emitValue(b *strings.Builder, v any, depth, indent int, inline bool) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case *big.Int:
		b.WriteString(t.String())
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(formatFloat(t))
	case string:
		b.WriteString(quoteIfNeeded(t, depth*indent))
	case UnsafeString:
		b.WriteString(quoteIfNeeded(string(t), depth*indent))
	case VaultedString:
		b.WriteString("!vault |\n")
		pad := strings.Repeat(" ", (depth+1)*indent)
		for _, line := range strings.Split(strings.TrimRight(t.Ciphertext, "\n"), "\n") {
			b.WriteString(pad)
			b.WriteString(line)
			b.WriteByte('\n')
		}
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		for i, item := range t {
			if i > 0 || !inline {
				b.WriteByte('\n')
				b.WriteString(strings.Repeat(" ", depth*indent))
			}
			b.WriteString("- ")
			if err := emitValue(b, item, depth+1, indent, true); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(t) == 0 {
			b.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(OrderedMap, 0, len(keys))
		for _, k := range keys {
			ordered = append(ordered, KV{K: k, V: t[k]})
		}
		return emitValue(b, ordered, depth, indent, inline)
	case *OMap:
		om := make(OrderedMap, 0, t.Len())
		for _, k := range t.Keys() {
			om = append(om, KV{K: k, V: t.Get(k)})
		}
		return emitValue(b, om, depth, indent, inline)
	case OrderedMap:
		if len(t) == 0 {
			b.WriteString("{}")
			return nil
		}
		for i, kv := range t {
			if i > 0 || !inline {
				b.WriteByte('\n')
				b.WriteString(strings.Repeat(" ", depth*indent))
			}
			b.WriteString(quoteIfNeeded(kv.K, 0))
			b.WriteByte(':')
			val := kv.V
			if isScalarValue(val) || isEmptyContainer(val) {
				b.WriteByte(' ')
			}
			if err := emitValue(b, val, depth+1, indent, false); err != nil {
				return err
			}
		}
	default:
		// Generic slices/maps ([]string, map[string]string, int32...) from
		// caller-provided values normalize via reflection.
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			items := make([]any, rv.Len())
			for i := range items {
				items[i] = rv.Index(i).Interface()
			}
			return emitValue(b, items, depth, indent, inline)
		case reflect.Map:
			if rv.Type().Key().Kind() == reflect.String {
				m := make(map[string]any, rv.Len())
				for _, k := range rv.MapKeys() {
					m[k.String()] = rv.MapIndex(k).Interface()
				}
				return emitValue(b, m, depth, indent, inline)
			}
		case reflect.Int8, reflect.Int16, reflect.Int32,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return emitValue(b, rv.Convert(reflect.TypeOf(int64(0))).Interface(), depth, indent, inline)
		case reflect.Float32:
			return emitValue(b, rv.Float(), depth, indent, inline)
		}
		return fmt.Errorf("yaml: cannot marshal value of type %T", v)
	}
	return nil
}

func isScalarValue(v any) bool {
	switch v.(type) {
	case nil, bool, int, int64, float64, string, UnsafeString:
		return true
	}
	return false
}

func isEmptyContainer(v any) bool {
	switch t := v.(type) {
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case *OMap:
		return t.Len() == 0
	}
	return false
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return ".inf"
	case math.IsInf(f, -1):
		return "-.inf"
	case math.IsNaN(f):
		return ".nan"
	}
	// PyYAML's represent_float: repr(f), positional unless the decimal
	// exponent is below -4 or at least 16, with ".0" kept in the mantissa
	// so re-parsing yields a float (2.0, 1790738388.92, 1.0e+16).
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		if !strings.Contains(e, ".") {
			e = strings.Replace(e, "e", ".0e", 1)
		}
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// quoteIfNeeded returns s quoted iff a plain scalar would not round-trip.
func quoteIfNeeded(s string, _ int) string {
	if s == "" {
		return "''"
	}
	if needsQuoting(s) {
		return strconv.Quote(s) // double-quoted with \n, \t, \" escapes
	}
	return s
}

func needsQuoting(s string) bool {
	// Would resolve to a non-string type?
	if _, isStr := resolveScalar(s).(string); !isStr {
		return true
	}
	if strings.ContainsAny(s, "\n\t") {
		return true
	}
	// Leading indicator characters or whitespace.
	c := s[0]
	if strings.IndexByte("-?:,[]{}#&*!|>'\"%@` ", c) >= 0 {
		// "- x" needs quotes; "-x" does not, unless it resolves non-string
		// (handled above). Same for "? " and ": ".
		if c != '-' && c != '?' && c != ':' {
			return true
		}
		if len(s) == 1 || s[1] == ' ' {
			return true
		}
	}
	if s[len(s)-1] == ' ' {
		return true
	}
	// Structure-forming substrings.
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") ||
		strings.Contains(s, " #") {
		return true
	}
	return false
}
