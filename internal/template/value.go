package template

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// Mapping lets the vars layer expose lazy dict-likes (hostvars) to the
// engine. getattr/getitem, `in`, iteration, length, and the mapping test all
// honor it.
type Mapping interface {
	GetItem(key string) (any, bool)
	Keys() []string // deterministic order
	Len() int
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// truthy implements Python truthiness. Undefined must be checked by callers
// first (it errors in boolean context).
func truthy(v any) bool {
	switch t := v.(type) {
	case Deprecated:
		return truthy(t.Value)
	case nil:
		return false
	case bool:
		return t
	case *big.Int:
		return t.Sign() != 0
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case yaml.UnsafeString:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	case Mapping:
		return t.Len() > 0
	case Omit:
		return true
	}
	return true
}

// toStr renders a value the way Jinja/Python str() would, for template
// output and the ~ operator.
func toStr(v any) string { return toStrIn(v, nil) }

// toStrIn is toStr inside the containers of active: one met again shows
// as Python's repr shows a recursive list or dict ("[...]", "{...}").
func toStrIn(v any, active map[cycleID]bool) string {
	if id, ok := containerOf(v); ok {
		if active[id] {
			if _, isList := v.([]any); isList {
				return "[...]"
			}
			return "{...}"
		}
		if active == nil {
			active = map[cycleID]bool{}
		}
		active[id] = true
		defer delete(active, id)
	}
	switch t := v.(type) {
	case Deprecated:
		return toStrIn(t.Value, active)
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case *big.Int:
		return bigIntStr(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return pyFloatStr(t)
	case string:
		return t
	case yaml.UnsafeString:
		return string(t)
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyReprIn(item, active))
		}
		b.WriteByte(']')
		return b.String()
	case map[string]any:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range sortedKeys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyReprIn(k, active))
			b.WriteString(": ")
			b.WriteString(pyReprIn(t[k], active))
		}
		b.WriteByte('}')
		return b.String()
	case Mapping:
		// Render in the mapping's own key order (Python dict str() preserves
		// insertion order; *OMap carries YAML source order).
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range t.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			mv, _ := t.GetItem(k)
			b.WriteString(pyReprIn(k, active))
			b.WriteString(": ")
			b.WriteString(pyReprIn(mv, active))
		}
		b.WriteByte('}')
		return b.String()
	}
	return fmt.Sprintf("%v", v)
}

// pyRepr renders a value like Python repr(): strings get quotes.
func pyRepr(v any) string { return pyReprIn(v, nil) }

func pyReprIn(v any, active map[cycleID]bool) string {
	switch t := v.(type) {
	case Deprecated:
		return pyReprIn(t.Value, active)
	case string:
		return pyStrRepr(t)
	case yaml.UnsafeString:
		return pyStrRepr(string(t))
	default:
		return toStrIn(v, active)
	}
}

// pyStrRepr is Python's repr() of a str: single quotes unless the text
// holds a single quote and no double quote, backslash escapes for the
// quote, backslash and control characters, printable text kept as is.
func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r != utf8.RuneError && !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func pyFloatStr(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	return pyFloatRepr(f)
}

// pyFloatRepr is Python's repr(float) for finite values: the shortest
// round-tripping digits, positional unless the decimal exponent is below
// -4 or at least 16 (1790738388.92, 2.0, 1e+16, 1.5e-05). Go's 'g' verb
// switches to exponent form much earlier (1.79073838892e+09).
func pyFloatRepr(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0" // 2.0 not 2
	}
	return s
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mappingToMap(m Mapping) map[string]any {
	out := make(map[string]any, m.Len())
	for _, k := range m.Keys() {
		v, _ := m.GetItem(k)
		out[k] = v
	}
	return out
}

// asInt reports v as an int64 if it is integral.
func asInt(v any) (int64, bool) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case bool: // Python: True == 1
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	v = Undeprecate(v)
	if i, ok := asInt(v); ok {
		return float64(i), true
	}
	if b, ok := v.(*big.Int); ok {
		f, _ := new(big.Float).SetInt(b).Float64()
		return f, true
	}
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

func isNumber(v any) bool {
	_, ok := asFloat(v)
	return ok
}

func asString(v any) (string, bool) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case string:
		return t, true
	case yaml.UnsafeString:
		return string(t), true
	}
	return "", false
}

// arith implements Jinja's binary arithmetic and concatenation operators.
func arith(op tokKind, a, b any) (any, error) {
	// String/list operators first.
	if s, ok := asString(a); ok {
		switch op {
		case tokAdd:
			if s2, ok := asString(b); ok {
				return s + s2, nil
			}
		case tokMul:
			if n, ok := asInt(b); ok {
				return repeatString(s, n)
			}
		case tokMod:
			return nil, fmt.Errorf("the printf-style '%%' string operator is not supported; use the format filter")
		}
	}
	if la, ok := a.([]any); ok {
		switch op {
		case tokAdd:
			if lb, ok := b.([]any); ok {
				out := make([]any, 0, len(la)+len(lb))
				out = append(out, la...)
				return append(out, lb...), nil
			}
		case tokMul:
			if n, ok := asInt(b); ok {
				count, err := clampRepeat(n, len(la))
				if err != nil {
					return nil, err
				}
				out := make([]any, 0, len(la)*count)
				for range count {
					out = append(out, la...)
				}
				return out, nil
			}
		}
	}
	if n, ok := asInt(a); ok && op == tokMul {
		if s, ok := asString(b); ok {
			return repeatString(s, n)
		}
	}
	return numArith(op, a, b)
}

// numArith is Python's arithmetic on ints (arbitrary precision) and
// floats.
func numArith(op tokKind, a, b any) (any, error) {
	ab, aInt := asBigInt(a)
	bb, bInt := asBigInt(b)
	af, aFloat := Undeprecate(a).(float64)
	bf, bFloat := Undeprecate(b).(float64)
	if !(aInt || aFloat) || !(bInt || bFloat) {
		if at := typeName(a); op == tokAdd && (at == "str" || at == "list") {
			return nil, fmt.Errorf("can only concatenate %s (not \"%s\") to %s", at, typeName(b), at)
		}
		return nil, fmt.Errorf("unsupported operand type(s) for %s: '%s' and '%s'", opName(op), typeName(a), typeName(b))
	}

	if aInt && bInt {
		ai, aSmall := asInt(a)
		bi, bSmall := asInt(b)
		switch op {
		case tokAdd:
			if aSmall && bSmall {
				if r, ok := addInt64(ai, bi); ok {
					return r, nil
				}
			}
			return bigArith(op, ab, bb)
		case tokSub:
			if aSmall && bSmall {
				if r, ok := subInt64(ai, bi); ok {
					return r, nil
				}
			}
			return bigArith(op, ab, bb)
		case tokMul:
			if aSmall && bSmall {
				if r, ok := mulInt64(ai, bi); ok {
					return r, nil
				}
			}
			return bigArith(op, ab, bb)
		case tokDiv: // true division is always float
			return bigTrueDiv(ab, bb)
		case tokFloorDiv, tokMod:
			if bb.Sign() == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			if aSmall && bSmall && !(ai == math.MinInt64 && bi == -1) {
				if op == tokMod {
					return pyModInt(ai, bi), nil
				}
				return pyFloorDivInt(ai, bi), nil
			}
			return bigArith(op, ab, bb)
		case tokPow:
			if bb.Sign() >= 0 {
				// Bounded (a 4 GiB result), where Python would run out of
				// memory.
				if n := int64(ab.BitLen()); n > 1 && (!bb.IsInt64() || bb.Int64() > (1<<35)/n) {
					return nil, fmt.Errorf("MemoryError")
				}
				return bigArith(op, ab, bb)
			}
			// A negative exponent: float(a) ** float(b).
			if ab.Sign() == 0 {
				return nil, fmt.Errorf("zero to a negative power")
			}
		}
	}
	if aInt {
		f, err := bigToFloat(ab)
		if err != nil {
			return nil, err
		}
		af = f
	}
	if bInt {
		f, err := bigToFloat(bb)
		if err != nil {
			return nil, err
		}
		bf = f
	}
	switch op {
	case tokAdd:
		return af + bf, nil
	case tokSub:
		return af - bf, nil
	case tokMul:
		return af * bf, nil
	case tokDiv:
		if bf == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return af / bf, nil
	case tokFloorDiv:
		if bf == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return math.Floor(af / bf), nil
	case tokMod:
		if bf == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		m := math.Mod(af, bf)
		if m != 0 && (m < 0) != (bf < 0) {
			m += bf
		}
		return m, nil
	case tokPow:
		if af == 0 && bf < 0 {
			return nil, fmt.Errorf("zero to a negative power")
		}
		return math.Pow(af, bf), nil
	}
	return nil, fmt.Errorf("unknown operator %s", opName(op))
}

// maxRepeatResult bounds sequence repetition ("x" * n) so a typo'd count
// fails with an error instead of exhausting memory.
const maxRepeatResult = 10 << 20

func clampRepeat(n int64, unit int) (int, error) {
	if n < 0 {
		return 0, nil
	}
	if unit > 0 && n > int64(maxRepeatResult/unit) {
		return 0, fmt.Errorf("repetition result too large (%d elements)", n*int64(unit))
	}
	return int(n), nil
}

func repeatString(s string, n int64) (string, error) {
	count, err := clampRepeat(n, len(s))
	if err != nil {
		return "", err
	}
	return strings.Repeat(s, count), nil
}

func pyFloorDivInt(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func pyModInt(a, b int64) int64 {
	m := a % b
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

func opName(op tokKind) string {
	switch op {
	case tokAdd:
		return "+"
	case tokSub:
		return "-"
	case tokMul:
		return "*"
	case tokDiv:
		return "/"
	case tokFloorDiv:
		return "//"
	case tokMod:
		return "%"
	case tokPow:
		return "**"
	case tokTilde:
		return "~"
	}
	return op.String()
}

func typeName(v any) string {
	v = Undeprecate(v)
	switch v.(type) {
	case nil:
		return "None"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		return "int"
	case float64:
		return "float"
	case string, yaml.UnsafeString:
		return "str"
	case []any:
		return "list"
	case map[string]any, Mapping:
		return "dict"
	case Undefined:
		return "undefined"
	case Omit:
		return "omit"
	case yaml.VaultedString:
		return "vault-encrypted str"
	}
	return fmt.Sprintf("%T", v)
}

// compare returns -1/0/1 for Python-style ordering, or an error for
// incomparable types (Python 3 raises on e.g. int < str).
func compare(a, b any) (int, error) { return compareOp(a, b, "<") }

// compareOp is compare for the operator op, which its error names.
func compareOp(a, b any, op string) (int, error) {
	a, b = Undeprecate(a), Undeprecate(b)
	if c, isNum := compareNumbers(a, b); isNum {
		return c, nil
	}
	if as, ok := asString(a); ok {
		if bs, ok := asString(b); ok {
			return strings.Compare(as, bs), nil
		}
	}
	if la, ok := a.([]any); ok {
		if lb, ok := b.([]any); ok {
			if ia, ok := containerOf(a); ok {
				if ib, _ := containerOf(b); ia == ib {
					return 0, nil // the same list
				}
			}
			for i := 0; i < len(la) && i < len(lb); i++ {
				if equal(la[i], lb[i]) {
					continue // Python skips items equal by identity or ==
				}
				c, err := compareOp(la[i], lb[i], op)
				if err != nil || c != 0 {
					return c, err
				}
			}
			switch {
			case len(la) < len(lb):
				return -1, nil
			case len(la) > len(lb):
				return 1, nil
			}
			return 0, nil
		}
	}
	return 0, fmt.Errorf("'%s' not supported between instances of '%s' and '%s'", op, pyClassName(a, false), pyClassName(b, false))
}

// Equal is Python's == for template values.
func Equal(a, b any) bool { return equal(a, b) }

// equal implements Python ==: cross-type numeric comparison works; other
// cross-type comparisons are false, never an error.
func equal(a, b any) bool {
	a, b = Undeprecate(a), Undeprecate(b)
	if isUndefined(a) || isUndefined(b) {
		return isUndefined(a) && isUndefined(b)
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	// The same container is equal to itself (Python compares items by
	// identity first, so a recursive list equals itself).
	if ia, ok := containerOf(a); ok {
		if ib, ok := containerOf(b); ok && ia == ib {
			return true
		}
	}
	if isNumber(a) {
		if !isNumber(b) {
			return false
		}
		c, _ := compareNumbers(a, b)
		return c == 0 && !isNaN(a) && !isNaN(b)
	}
	if as, ok := asString(a); ok {
		if bs, ok := asString(b); ok {
			return as == bs
		}
		return false
	}
	if la, ok := a.([]any); ok {
		lb, ok := b.([]any)
		if !ok || len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !equal(la[i], lb[i]) {
				return false
			}
		}
		return true
	}
	ma, aIsMap := anyToMap(a)
	if aIsMap {
		mb, bIsMap := anyToMap(b)
		if !bIsMap || len(ma) != len(mb) {
			return false
		}
		for k, va := range ma {
			vb, ok := mb[k]
			if !ok || !equal(va, vb) {
				return false
			}
		}
		return true
	}
	// Uncomparable types (funcs, lazy ranges) must not panic under ==.
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

func anyToMap(v any) (map[string]any, bool) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case Mapping:
		return mappingToMap(t), true
	}
	return nil, false
}

// orderedMap returns v's keys in iteration order plus a value map. An ordered
// mapping (*OMap or any Mapping) yields its own key order; a plain Go map has
// no inherent order and yields sorted keys for determinism. Used by filters
// (dict2items) that must iterate a dict the way Python would.
func orderedMap(v any) ([]string, map[string]any, bool) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case Mapping:
		return t.Keys(), mappingToMap(t), true
	case map[string]any:
		return sortedKeys(t), t, true
	}
	return nil, nil, false
}

// contains implements the `in` operator.
func contains(needle, haystack any) (bool, error) {
	switch h := haystack.(type) {
	case string:
		s, ok := asString(needle)
		if !ok {
			return false, fmt.Errorf("'in <string>' requires string as left operand, not %s", typeName(needle))
		}
		return strings.Contains(h, s), nil
	case yaml.UnsafeString:
		return contains(needle, string(h))
	case []any:
		for _, item := range h {
			if equal(item, needle) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		s, ok := asString(needle)
		if !ok {
			return false, nil
		}
		_, found := h[s]
		return found, nil
	case Mapping:
		s, ok := asString(needle)
		if !ok {
			return false, nil
		}
		_, found := h.GetItem(s)
		return found, nil
	}
	return false, fmt.Errorf("argument of type '%s' is not a container or iterable", pyClassName(haystack, false))
}

// length implements the length/count filter and len() semantics.
func length(v any) (int, error) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case string:
		return len([]rune(t)), nil
	case yaml.UnsafeString:
		return len([]rune(string(t))), nil
	case []any:
		return len(t), nil
	case map[string]any:
		return len(t), nil
	case Mapping:
		return t.Len(), nil
	case *rangeValue:
		return int(t.length()), nil
	}
	return 0, fmt.Errorf("object of type '%s' has no len()", pyClassName(v, false))
}

// iterate returns the items of an iterable: list items, string runes (as
// 1-char strings), or mapping keys (sorted for determinism).
func iterate(v any) ([]any, error) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case yaml.UnsafeString:
		return iterate(string(t))
	case map[string]any:
		keys := sortedKeys(t)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	case Mapping:
		keys := t.Keys()
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	case *rangeValue:
		return t.materialize(), nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyClassName(v, false))
}

// rangeValue is the lazy result of range(): iterable and indexable without
// materializing (range(100000000)|length must be O(1)).
type rangeValue struct {
	start, stop, step int64
}

func (r *rangeValue) length() int64 {
	if r.step > 0 {
		if r.stop <= r.start {
			return 0
		}
		return (r.stop - r.start + r.step - 1) / r.step
	}
	if r.stop >= r.start {
		return 0
	}
	return (r.start - r.stop - r.step - 1) / (-r.step)
}

func (r *rangeValue) materialize() []any {
	n := r.length()
	if n > 10_000_000 {
		n = 10_000_000 // hard cap against accidental memory bombs
	}
	out := make([]any, 0, n)
	for i := int64(0); i < n; i++ {
		out = append(out, r.start+i*r.step)
	}
	return out
}

// Truthy is Python truthiness for a rendered value.
func Truthy(v any) bool { return truthy(v) }

// pyToBytes is ansible's to_bytes(v): a string's text, anything else
// as str(v) (its nonstring='simplerepr').
func pyToBytes(v any) string {
	if s, ok := asString(Undeprecate(v)); ok {
		return s
	}
	return toStr(v)
}

// AsDict is v as a plain map, when it is a dict.
func AsDict(v any) (map[string]any, bool) { return anyToMap(v) }

// NativeTypeName is ansible-core's native_type_name(v): its Python type.
func NativeTypeName(v any) string {
	if _, ok := Undeprecate(v).(yaml.UnsafeString); ok {
		return "str"
	}
	return pyTypeName(Undeprecate(v))
}
