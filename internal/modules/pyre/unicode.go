package pyre

import (
	"bytes"
	"compress/zlib"
	_ "embed"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// LookupName is unicodedata.lookup(name) for a name of one code point
// (\N{...} in str patterns and unicode_escape).
func LookupName(name string) (rune, bool) { return lookupName(name) }

//go:embed names.bin
var namesData []byte

var (
	namesOnce sync.Once
	names     map[string]rune
)

// loadNames reads names.bin (see gen_tables.py): the names, then the
// aliases.
func loadNames() {
	names = make(map[string]rune, 60000)
	zr, err := zlib.NewReader(bytes.NewReader(namesData))
	if err != nil {
		panic("pyre: names.bin: " + err.Error())
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		panic("pyre: names.bin: " + err.Error())
	}
	cp, aliases := int64(0), false
	for len(data) > 0 {
		line, rest, _ := bytes.Cut(data, []byte{'\n'})
		data = rest
		if string(line) == "=" {
			aliases = true
			continue
		}
		num, name, _ := bytes.Cut(line, []byte{'\t'})
		if aliases {
			v, _ := strconv.ParseInt(string(num), 16, 32)
			names[string(name)] = rune(v)
			continue
		}
		d, _ := strconv.ParseInt(string(num), 10, 32)
		cp += d
		names[string(name)] = rune(cp)
	}
}

// lookupName is unicodedata.lookup() of a single code point's name, as
// CPython 3.14 looks it up: ASCII case-insensitively, Hangul syllables
// from their jamo, CJK unified and Tangut ideographs by their hex code
// point, then the names and aliases (a named sequence is several code
// points: no single one).
func lookupName(name string) (rune, bool) {
	up := []byte(name)
	for i, c := range up {
		switch {
		case c >= 0x80:
			return 0, false
		case c >= 'a' && c <= 'z':
			up[i] = c - 32
		}
	}
	key := string(up)
	if rest, ok := strings.CutPrefix(key, "HANGUL SYLLABLE "); ok {
		return hangulSyllable(rest)
	}
	for _, r := range nameRanges {
		h, ok := strings.CutPrefix(key, r.prefix)
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(h, 16, 32)
		if err != nil || fmt.Sprintf("%04X", v) != h || !inTab(r.tab, rune(v)) {
			return 0, false
		}
		return rune(v), true
	}
	namesOnce.Do(loadNames)
	r, ok := names[key]
	return r, ok
}

// hangulSyllable is CPython's find_syllable over the leading consonant,
// the vowel and the trailing consonant in turn: each the longest jamo
// name the text starts with.
func hangulSyllable(s string) (rune, bool) {
	longest := func(s string, jamo []string) (int, int) {
		best, n := -1, -1
		for i, j := range jamo {
			if len(j) > n && strings.HasPrefix(s, j) {
				best, n = i, len(j)
			}
		}
		return best, max(n, 0)
	}
	l, n := longest(s, jamoL)
	s = s[n:]
	v, n := longest(s, jamoV)
	s = s[n:]
	t, n := longest(s, jamoT)
	s = s[n:]
	if l < 0 || v < 0 || t < 0 || s != "" {
		return 0, false
	}
	return rune(0xAC00 + (l*21+v)*28 + t), true
}
