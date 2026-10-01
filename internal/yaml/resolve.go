package yaml

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// This file ports PyYAML's implicit resolver rules (YAML 1.1), which is what
// Ansible uses. The famous gotchas are deliberately replicated:
//   - yes/no/on/off (word list only; single 'y'/'n' are NOT booleans)
//   - 0644 is a legacy octal int (=420)
//   - 1.10 is the float 1.1
//   - 1e5 is a STRING (PyYAML's float regex requires a sign after e/E)
// Divergences (documented): timestamps and sexagesimals (1:30) resolve as
// strings; ints overflowing int64 fall back to string.

var (
	boolWords = map[string]bool{
		"yes": true, "Yes": true, "YES": true,
		"true": true, "True": true, "TRUE": true,
		"on": true, "On": true, "ON": true,
		"no": false, "No": false, "NO": false,
		"false": false, "False": false, "FALSE": false,
		"off": false, "Off": false, "OFF": false,
	}

	// PyYAML's int pattern, minus the sexagesimal alternative, plus 0o
	// octals (a YAML 1.2 spelling PyYAML lacks; harmless extension).
	intRe = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0o[0-7_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+)$`)

	// PyYAML's float pattern, minus the sexagesimal alternative. Note the
	// REQUIRED sign in the exponent — bug-for-bug with PyYAML.
	floatRe = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9_]+(?:[eE][-+][0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
)

// resolveScalar applies YAML 1.1 implicit typing to a plain scalar.
func resolveScalar(s string) any {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil
	}
	if b, ok := boolWords[s]; ok {
		return b
	}
	// Cheap first-character gate before regexes (PyYAML does the same).
	c := s[0]
	if c == '-' || c == '+' || (c >= '0' && c <= '9') || c == '.' {
		if intRe.MatchString(s) {
			if v, ok := parseInt11(s); ok {
				return v
			}
			if v, ok := parseBigInt11(s); ok {
				return v // Python ints have no size limit
			}
			return s
		}
		if floatRe.MatchString(s) {
			return parseFloat11(s)
		}
	}
	return s
}

// parseInt11 parses a YAML 1.1 integer (with '_' separators, 0x/0b/0o/0NNN).
func parseInt11(s string) (int64, bool) {
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	s = strings.ReplaceAll(s, "_", "")
	base := 10
	switch {
	case strings.HasPrefix(s, "0b"):
		base, s = 2, s[2:]
	case strings.HasPrefix(s, "0x"):
		base, s = 16, s[2:]
	case strings.HasPrefix(s, "0o"):
		base, s = 8, s[2:]
	case len(s) > 1 && s[0] == '0':
		base, s = 8, s[1:] // legacy octal: 0644
	}
	v, err := strconv.ParseInt(s, base, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

// parseBigInt11 is parseInt11 for an integer beyond int64.
func parseBigInt11(s string) (*big.Int, bool) {
	neg := s[0] == '-'
	s = strings.TrimLeft(s, "+-")
	s = strings.ReplaceAll(s, "_", "")
	base := 10
	switch {
	case strings.HasPrefix(s, "0b"):
		base, s = 2, s[2:]
	case strings.HasPrefix(s, "0x"):
		base, s = 16, s[2:]
	case strings.HasPrefix(s, "0o"):
		base, s = 8, s[2:]
	case len(s) > 1 && s[0] == '0':
		base, s = 8, s[1:]
	}
	b, ok := new(big.Int).SetString(s, base)
	if !ok {
		return nil, false
	}
	if neg {
		b.Neg(b)
	}
	return b, true
}

func parseFloat11(s string) float64 {
	lower := strings.ToLower(s)
	switch {
	case strings.HasSuffix(lower, ".inf"):
		if strings.HasPrefix(lower, "-") {
			return math.Inf(-1)
		}
		return math.Inf(1)
	case strings.HasSuffix(lower, ".nan"):
		return math.NaN()
	}
	s = strings.ReplaceAll(s, "_", "")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
