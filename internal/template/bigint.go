package template

import (
	"errors"
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"
)

// Python ints have arbitrary precision. The engine keeps an int as int64
// while it fits, and as *big.Int (never one that fits int64) beyond:
// arithmetic, comparisons, str() and serialization treat both as int.

// asBigInt reports v as a big integer if it is a Python int (bools
// included, as True == 1).
func asBigInt(v any) (*big.Int, bool) {
	v = Undeprecate(v)
	switch t := v.(type) {
	case *big.Int:
		return t, true
	case int64:
		return big.NewInt(t), true
	case int:
		return big.NewInt(int64(t)), true
	case bool:
		if t {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return nil, false
}

// isBigInt reports whether v is an int beyond int64.
func isBigInt(v any) bool {
	_, ok := Undeprecate(v).(*big.Int)
	return ok
}

// normInt is b as the engine keeps an int: int64 when it fits.
func normInt(b *big.Int) any {
	if b.IsInt64() {
		return b.Int64()
	}
	return b
}

// errIntTooLarge is OverflowError converting an int to float.
var errIntTooLarge = errors.New("int too large to convert to float")

// bigToFloat is float(b): correctly rounded, OverflowError beyond the
// float range.
func bigToFloat(b *big.Int) (float64, error) {
	f, _ := new(big.Float).SetInt(b).Float64()
	if math.IsInf(f, 0) {
		return 0, errIntTooLarge
	}
	return f, nil
}

// addInt64, subInt64 and mulInt64 are int64 arithmetic that reports
// overflow (the result then needs a big int).
func addInt64(a, b int64) (int64, bool) {
	s := a + b
	return s, (s > a) == (b > 0)
}

func subInt64(a, b int64) (int64, bool) {
	d := a - b
	return d, (d < a) == (b > 0)
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(absInt64(a)), uint64(absInt64(b)))
	neg := (a < 0) != (b < 0)
	if hi != 0 || (a == math.MinInt64 || b == math.MinInt64) {
		return 0, false
	}
	if neg {
		if lo > 1<<63 {
			return 0, false
		}
		return -int64(lo), true
	}
	if lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo), true
}

func absInt64(a int64) int64 {
	if a < 0 {
		return -a
	}
	return a
}

// bigArith is int arithmetic on Python ints (op one of + - * // % **,
// the exponent non-negative).
func bigArith(op tokKind, a, b *big.Int) (any, error) {
	r := new(big.Int)
	switch op {
	case tokAdd:
		r.Add(a, b)
	case tokSub:
		r.Sub(a, b)
	case tokMul:
		r.Mul(a, b)
	case tokFloorDiv, tokMod:
		if b.Sign() == 0 {
			return nil, errors.New("division by zero")
		}
		q, m := new(big.Int).DivMod(a, b, new(big.Int))
		// DivMod is Euclidean (m >= 0); Python floors (m takes b's sign).
		if m.Sign() != 0 && b.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
			m.Add(m, b)
		}
		if op == tokMod {
			r = m
		} else {
			r = q
		}
	case tokPow:
		r.Exp(a, b, nil)
	}
	return normInt(r), nil
}

// bigTrueDiv is int / int: the correctly rounded float quotient.
func bigTrueDiv(a, b *big.Int) (any, error) {
	if b.Sign() == 0 {
		return nil, errors.New("division by zero")
	}
	f, _ := new(big.Rat).SetFrac(a, b).Float64()
	if math.IsInf(f, 0) {
		return nil, errors.New("integer division result too large for a float")
	}
	return f, nil
}

// compareIntFloat compares an int with a float exactly, as Python does
// (ok is false when f is NaN: the values are unordered).
func compareIntFloat(a *big.Int, f float64) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	if math.IsInf(f, 1) {
		return -1, true
	}
	if math.IsInf(f, -1) {
		return 1, true
	}
	return new(big.Float).SetInt(a).Cmp(new(big.Float).SetFloat64(f)), true
}

// compareNumbers orders two numbers (ints of any size, floats, bools) as
// Python does: exactly, an int against a float included. isNum is false
// when either is not a number; NaN compares as 0 (unordered).
func compareNumbers(a, b any) (c int, isNum bool) {
	a, b = Undeprecate(a), Undeprecate(b)
	if ai, ok := asInt(a); ok {
		if bi, ok := asInt(b); ok {
			switch {
			case ai < bi:
				return -1, true
			case ai > bi:
				return 1, true
			}
			return 0, true
		}
	}
	ab, aInt := asBigInt(a)
	bb, bInt := asBigInt(b)
	af, aFloat := a.(float64)
	bf, bFloat := b.(float64)
	switch {
	case aInt && bInt:
		return ab.Cmp(bb), true
	case aInt && bFloat:
		c, _ := compareIntFloat(ab, bf)
		return c, true
	case aFloat && bInt:
		c, _ := compareIntFloat(bb, af)
		return -c, true
	case aFloat && bFloat:
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func isNaN(v any) bool {
	f, ok := Undeprecate(v).(float64)
	return ok && math.IsNaN(f)
}

// maxIntStrDigits is sys.get_int_max_str_digits(): longer decimal strings
// do not convert.
const maxIntStrDigits = 4300

// stripUnderscores removes the single underscores Python allows between
// digits (ok is false for a leading, trailing or doubled one).
func stripUnderscores(s string) (string, bool) {
	if !strings.Contains(s, "_") {
		return s, true
	}
	if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return "", false
	}
	return strings.ReplaceAll(s, "_", ""), true
}

// pyParseInt is int(s, base) for a str: surrounding whitespace, a sign,
// the base's prefix (any for base 0) and underscores between digits.
func pyParseInt(s string, base int64) (any, bool) {
	if base != 0 && (base < 2 || base > 36) {
		return nil, false
	}
	s = strings.TrimFunc(s, isPySpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if len(s) >= 2 && s[0] == '0' {
		var pb int64
		switch s[1] {
		case 'x', 'X':
			pb = 16
		case 'o', 'O':
			pb = 8
		case 'b', 'B':
			pb = 2
		}
		if pb != 0 && (base == 0 || base == pb) {
			base = pb
			s = strings.TrimPrefix(s[2:], "_")
			if s == "" || s[0] == '_' {
				return nil, false
			}
		}
	}
	if base == 0 {
		base = 10
		if t := strings.TrimLeft(s, "0_"); t != "" && strings.HasPrefix(s, "0") {
			return nil, false // 0123: leading zeros need a prefix
		}
	}
	digits, ok := stripUnderscores(s)
	if !ok || digits == "" || (base == 10 && len(digits) > maxIntStrDigits) {
		return nil, false
	}
	b, ok := new(big.Int).SetString(digits, int(base))
	if !ok || strings.ContainsAny(digits, "+-") {
		return nil, false
	}
	if neg {
		b.Neg(b)
	}
	return normInt(b), true
}

// pyParseFloat is float(s) for a str.
func pyParseFloat(s string) (float64, bool) {
	s = strings.TrimFunc(s, isPySpace)
	switch t := strings.ToLower(strings.TrimLeft(s, "+-")); t {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	lower := strings.ToLower(s)
	if s == "" || strings.Contains(lower, "x") || strings.Contains(lower, "p") || strings.Contains(lower, "in") {
		return 0, false
	}
	digits, ok := stripUnderscores(s)
	if !ok || strings.Contains(s, "_.") || strings.Contains(s, "._") || strings.Contains(lower, "_e") || strings.Contains(lower, "e_") {
		return 0, false
	}
	f, err := strconv.ParseFloat(digits, 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) && ne.Err == strconv.ErrRange {
			return f, true // overflow is ±inf, underflow 0
		}
		return 0, false
	}
	return f, true
}

// floatToInt is int(f): truncated, any size (ok is false for inf, nan).
func floatToInt(f float64) (any, bool) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, false
	}
	if f >= -9.2e18 && f <= 9.2e18 {
		return int64(f), true
	}
	b, _ := new(big.Float).SetFloat64(math.Trunc(f)).Int(nil)
	return normInt(b), true
}

// bigIntStr is str() of an int beyond int64.
func bigIntStr(b *big.Int) string { return b.String() }
