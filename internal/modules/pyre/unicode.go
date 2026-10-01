package pyre

import (
	"fmt"
	"sort"
	"strings"
)

//go:generate python3 testdata/gen_tables.py tables.go

type rangeTab struct{ lo, hi rune }

type casePair struct{ from, to rune }

func inTab(t []rangeTab, c rune) bool {
	i := sort.Search(len(t), func(i int) bool { return t[i].hi >= c })
	return i < len(t) && t[i].lo <= c
}

func mapCase(t []casePair, c rune) rune {
	i := sort.Search(len(t), func(i int) bool { return t[i].from >= c })
	if i < len(t) && t[i].from == c {
		return t[i].to
	}
	return c
}

// sre's character predicates (Python's str methods for str patterns).

func uniIsDigit(c rune) bool { return inTab(tabDecimal, c) }
func uniIsSpace(c rune) bool { return inTab(tabSpace, c) }
func uniIsAlnum(c rune) bool { return inTab(tabAlnum, c) }
func uniIsWord(c rune) bool  { return c == '_' || uniIsAlnum(c) }

// uniLower and uniUpper are sre_lower_unicode / sre_upper_unicode: the
// first code point of str.lower() / str.upper().
func uniLower(c rune) rune { return mapCase(tabLower, c) }
func uniUpper(c rune) rune { return mapCase(tabUpper, c) }

func uniIsCased(c rune) bool { return uniLower(c) != c || uniUpper(c) != c }

func asciiIsDigit(c rune) bool { return c >= '0' && c <= '9' }
func asciiIsSpace(c rune) bool {
	return c == ' ' || c >= '\t' && c <= '\r'
}
func asciiIsAlnum(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func asciiIsWord(c rune) bool { return c == '_' || asciiIsAlnum(c) }

func asciiLower(c rune) rune {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func asciiIsCased(c rune) bool {
	return c < 128 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')
}

// isIdentifier is str.isidentifier().
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range []rune(s) {
		if i == 0 {
			if !inTab(tabIDStart, r) {
				return false
			}
		} else if !inTab(tabIDContinue, r) {
			return false
		}
	}
	return true
}

// isASCIIDecimal is name.isdecimal() and name.isascii().
func isASCIIDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pyRepr is repr() of a Python str.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
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
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case inTab(tabPrintable, r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

var jamoL = []string{"G", "GG", "N", "D", "DD", "R", "M", "B", "BB", "S", "SS", "", "J", "JJ", "C", "K", "T", "P", "H"}
var jamoV = []string{"A", "AE", "YA", "YAE", "EO", "E", "YEO", "YE", "O", "WA", "WAE", "OE", "YO", "U", "WEO", "WE", "WI", "YU", "EU", "YI", "I"}
var jamoT = []string{"", "G", "GG", "GS", "N", "NJ", "NH", "D", "L", "LG", "LM", "LB", "LS", "LT", "LP", "LH", "M", "B", "BS", "S", "SS", "NG", "J", "C", "K", "T", "P", "H"}

var cjkRanges = []rangeTab{
	{0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0x20000, 0x2A6DF}, {0x2A700, 0x2B739},
	{0x2B740, 0x2B81D}, {0x2B820, 0x2CEA1}, {0x2CEB0, 0x2EBE0}, {0x2EBF0, 0x2EE5D},
	{0x30000, 0x3134A}, {0x31350, 0x323AF},
}

// LookupName is unicodedata.lookup(name) over the names \N{...} knows.
func LookupName(name string) (rune, bool) { return lookupName(name) }

// lookupName is unicodedata.lookup() for \N{...}: case-insensitive, over
// a subset of the names (see gen_tables.py) plus the computed CJK
// unified ideograph and Hangul syllable names.
func lookupName(name string) (rune, bool) {
	up := strings.ToUpper(name)
	if r, ok := charNames[up]; ok {
		return r, true
	}
	if h, ok := strings.CutPrefix(up, "CJK UNIFIED IDEOGRAPH-"); ok && (len(h) == 4 || len(h) == 5) {
		var v rune
		for _, c := range h {
			switch {
			case c >= '0' && c <= '9':
				v = v*16 + c - '0'
			case c >= 'A' && c <= 'F':
				v = v*16 + c - 'A' + 10
			default:
				return 0, false
			}
		}
		if inTab(cjkRanges, v) {
			return v, true
		}
		return 0, false
	}
	if rest, ok := strings.CutPrefix(up, "HANGUL SYLLABLE "); ok {
		for l, ls := range jamoL {
			if !strings.HasPrefix(rest, ls) {
				continue
			}
			for v, vs := range jamoV {
				if !strings.HasPrefix(rest[len(ls):], vs) {
					continue
				}
				for t, ts := range jamoT {
					if rest[len(ls)+len(vs):] == ts {
						return rune(0xAC00 + (l*21+v)*28 + t), true
					}
				}
			}
		}
	}
	return 0, false
}
