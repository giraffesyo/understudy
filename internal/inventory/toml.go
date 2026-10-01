package inventory

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// A port of Python's tomllib (the TOML 1.0 parser the toml inventory
// plugin loads files with): the same acceptance, the same values, and
// TOMLDecodeError's messages and positions. Tables decode to ordered
// mappings (Python dicts keep insertion order), arrays to []any, integers
// to int64 (*big.Int beyond it), and dates and times to the strings
// ansible-core shows them as (their isoformat()).

// tomlError is tomllib's TOMLDecodeError; cause is the message of the
// ValueError it was raised from, if any.
type tomlError struct {
	msg, cause string
}

func (e *tomlError) Error() string { return e.msg }

type tomlParser struct {
	src []rune
}

// newTOMLError words an error at pos as TOMLDecodeError does.
func (p *tomlParser) errAt(msg string, pos int) error {
	lineno := 1
	last := -1
	for i := 0; i < pos && i < len(p.src); i++ {
		if p.src[i] == '\n' {
			lineno++
			last = i
		}
	}
	colno := pos + 1
	if lineno > 1 {
		colno = pos - last
	}
	coord := fmt.Sprintf("line %d, column %d", lineno, colno)
	if pos >= len(p.src) {
		coord = "end of document"
	}
	return &tomlError{msg: fmt.Sprintf("%s (at %s)", msg, coord)}
}

// tomlKey is a parsed key: its parts.
type tomlKey []string

// String is the key's Python repr (a tuple of str).
func (k tomlKey) String() string {
	parts := make([]string, len(k))
	for i, s := range k {
		parts[i] = pyQuote(s)
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func (k tomlKey) plus(o tomlKey) tomlKey {
	out := make(tomlKey, 0, len(k)+len(o))
	return append(append(out, k...), o...)
}

const (
	flagFrozen       = 0
	flagExplicitNest = 1
)

type flagNode struct {
	flags, recursive map[int]bool
	nested           map[string]*flagNode
}

func newFlagNode() *flagNode {
	return &flagNode{flags: map[int]bool{}, recursive: map[int]bool{}, nested: map[string]*flagNode{}}
}

type tomlFlags struct {
	root    map[string]*flagNode
	pending []struct {
		key  tomlKey
		flag int
	}
}

func newTOMLFlags() *tomlFlags { return &tomlFlags{root: map[string]*flagNode{}} }

func (f *tomlFlags) addPending(key tomlKey, flag int) {
	f.pending = append(f.pending, struct {
		key  tomlKey
		flag int
	}{key, flag})
}

func (f *tomlFlags) finalizePending() {
	for _, p := range f.pending {
		f.set(p.key, p.flag, false)
	}
	f.pending = nil
}

func (f *tomlFlags) unsetAll(key tomlKey) {
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return
		}
		cont = n.nested
	}
	delete(cont, key[len(key)-1])
}

func (f *tomlFlags) set(key tomlKey, flag int, recursive bool) {
	cont := f.root
	for _, k := range key[:len(key)-1] {
		if _, ok := cont[k]; !ok {
			cont[k] = newFlagNode()
		}
		cont = cont[k].nested
	}
	stem := key[len(key)-1]
	if _, ok := cont[stem]; !ok {
		cont[stem] = newFlagNode()
	}
	if recursive {
		cont[stem].recursive[flag] = true
	} else {
		cont[stem].flags[flag] = true
	}
}

func (f *tomlFlags) is(key tomlKey, flag int) bool {
	if len(key) == 0 {
		return false
	}
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return false
		}
		if n.recursive[flag] {
			return true
		}
		cont = n.nested
	}
	n, ok := cont[key[len(key)-1]]
	if !ok {
		return false
	}
	return n.flags[flag] || n.recursive[flag]
}

var errNoNest = fmt.Errorf("There is no nest behind this key")

// getOrCreateNest is NestedDict.get_or_create_nest.
func getOrCreateNest(root *yaml.OMap, key tomlKey, accessLists bool) (*yaml.OMap, error) {
	cont := root
	for _, k := range key {
		if !cont.Has(k) {
			cont.Set(k, yaml.NewOMap())
		}
		v := cont.Get(k)
		if l, ok := v.([]any); ok && accessLists {
			v = l[len(l)-1]
		}
		m, ok := v.(*yaml.OMap)
		if !ok {
			return nil, errNoNest
		}
		cont = m
	}
	return cont, nil
}

func appendNestToList(root *yaml.OMap, key tomlKey) error {
	cont, err := getOrCreateNest(root, key[:len(key)-1], true)
	if err != nil {
		return err
	}
	last := key[len(key)-1]
	if cont.Has(last) {
		l, ok := cont.Get(last).([]any)
		if !ok {
			return errNoNest
		}
		cont.Set(last, append(l, yaml.NewOMap()))
	} else {
		cont.Set(last, []any{yaml.NewOMap()})
	}
	return nil
}

// parseTOML is tomllib.loads.
func parseTOML(s string) (*yaml.OMap, error) {
	p := &tomlParser{src: []rune(strings.ReplaceAll(s, "\r\n", "\n"))}
	return p.loads()
}

func (p *tomlParser) at(pos int) (rune, bool) {
	if pos < 0 || pos >= len(p.src) {
		return 0, false
	}
	return p.src[pos], true
}

func (p *tomlParser) startsWith(s string, pos int) bool {
	r := []rune(s)
	if pos+len(r) > len(p.src) {
		return false
	}
	for i, c := range r {
		if p.src[pos+i] != c {
			return false
		}
	}
	return true
}

func isTOMLWS(c rune) bool { return c == ' ' || c == '\t' }

func isBareKeyChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func isASCIICtrl(c rune) bool { return c < 32 || c == 127 }

func (p *tomlParser) skipWS(pos int, newlines bool) int {
	for pos < len(p.src) && (isTOMLWS(p.src[pos]) || newlines && p.src[pos] == '\n') {
		pos++
	}
	return pos
}

func (p *tomlParser) loads() (*yaml.OMap, error) {
	data := yaml.NewOMap()
	flags := newTOMLFlags()
	var header tomlKey
	pos := 0
	for {
		pos = p.skipWS(pos, false)
		c, ok := p.at(pos)
		if !ok {
			break
		}
		if c == '\n' {
			pos++
			continue
		}
		var err error
		switch {
		case isBareKeyChar(c) || c == '"' || c == '\'':
			if pos, err = p.keyValueRule(pos, data, flags, header); err != nil {
				return nil, err
			}
			pos = p.skipWS(pos, false)
		case c == '[':
			second, _ := p.at(pos + 1)
			flags.finalizePending()
			if second == '[' {
				pos, header, err = p.createListRule(pos, data, flags)
			} else {
				pos, header, err = p.createDictRule(pos, data, flags)
			}
			if err != nil {
				return nil, err
			}
			pos = p.skipWS(pos, false)
		case c != '#':
			return nil, p.errAt("Invalid statement", pos)
		}
		if pos, err = p.skipComment(pos); err != nil {
			return nil, err
		}
		c, ok = p.at(pos)
		if !ok {
			break
		}
		if c != '\n' {
			return nil, p.errAt("Expected newline or end of document after a statement", pos)
		}
		pos++
	}
	return data, nil
}

// skipUntil is tomllib's skip_until.
func (p *tomlParser) skipUntil(pos int, expect string, errorOn func(rune) bool, errorOnEOF bool) (int, error) {
	newPos := -1
	for i := pos; i < len(p.src); i++ {
		if p.startsWith(expect, i) {
			newPos = i
			break
		}
	}
	if newPos < 0 {
		newPos = len(p.src)
		if errorOnEOF {
			return 0, p.errAt(fmt.Sprintf("Expected %s", pyQuote(expect)), newPos)
		}
	}
	for i := pos; i < newPos; i++ {
		if errorOn(p.src[i]) {
			return 0, p.errAt(fmt.Sprintf("Found invalid character %s", pyRuneRepr(p.src[i])), i)
		}
	}
	return newPos, nil
}

func illegalBasic(c rune) bool     { return isASCIICtrl(c) && c != '\t' }
func illegalMultiline(c rune) bool { return isASCIICtrl(c) && c != '\t' && c != '\n' }

func (p *tomlParser) skipComment(pos int) (int, error) {
	if c, ok := p.at(pos); ok && c == '#' {
		return p.skipUntil(pos+1, "\n", illegalBasic, false)
	}
	return pos, nil
}

func (p *tomlParser) skipCommentsAndArrayWS(pos int) (int, error) {
	for {
		before := pos
		pos = p.skipWS(pos, true)
		var err error
		if pos, err = p.skipComment(pos); err != nil {
			return 0, err
		}
		if pos == before {
			return pos, nil
		}
	}
}

func (p *tomlParser) createDictRule(pos int, data *yaml.OMap, flags *tomlFlags) (int, tomlKey, error) {
	pos++
	pos = p.skipWS(pos, false)
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, err
	}
	if flags.is(key, flagExplicitNest) || flags.is(key, flagFrozen) {
		return 0, nil, p.errAt(fmt.Sprintf("Cannot declare %s twice", key), pos)
	}
	flags.set(key, flagExplicitNest, false)
	if _, err := getOrCreateNest(data, key, true); err != nil {
		return 0, nil, p.errAt("Cannot overwrite a value", pos)
	}
	if !p.startsWith("]", pos) {
		return 0, nil, p.errAt("Expected ']' at the end of a table declaration", pos)
	}
	return pos + 1, key, nil
}

func (p *tomlParser) createListRule(pos int, data *yaml.OMap, flags *tomlFlags) (int, tomlKey, error) {
	pos += 2
	pos = p.skipWS(pos, false)
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, err
	}
	if flags.is(key, flagFrozen) {
		return 0, nil, p.errAt(fmt.Sprintf("Cannot mutate immutable namespace %s", key), pos)
	}
	flags.unsetAll(key)
	flags.set(key, flagExplicitNest, false)
	if err := appendNestToList(data, key); err != nil {
		return 0, nil, p.errAt("Cannot overwrite a value", pos)
	}
	if !p.startsWith("]]", pos) {
		return 0, nil, p.errAt("Expected ']]' at the end of an array declaration", pos)
	}
	return pos + 2, key, nil
}

func isContainer(v any) bool {
	switch v.(type) {
	case *yaml.OMap, []any:
		return true
	}
	return false
}

func (p *tomlParser) keyValueRule(pos int, data *yaml.OMap, flags *tomlFlags, header tomlKey) (int, error) {
	pos, key, value, err := p.parseKeyValuePair(pos)
	if err != nil {
		return 0, err
	}
	parent, stem := key[:len(key)-1], key[len(key)-1]
	absParent := header.plus(parent)
	for i := 1; i < len(key); i++ {
		contKey := header.plus(key[:i])
		if flags.is(contKey, flagExplicitNest) {
			return 0, p.errAt(fmt.Sprintf("Cannot redefine namespace %s", contKey), pos)
		}
		flags.addPending(contKey, flagExplicitNest)
	}
	if flags.is(absParent, flagFrozen) {
		return 0, p.errAt(fmt.Sprintf("Cannot mutate immutable namespace %s", absParent), pos)
	}
	nest, err := getOrCreateNest(data, absParent, true)
	if err != nil {
		return 0, p.errAt("Cannot overwrite a value", pos)
	}
	if nest.Has(stem) {
		return 0, p.errAt("Cannot overwrite a value", pos)
	}
	if isContainer(value) {
		flags.set(header.plus(key), flagFrozen, true)
	}
	nest.Set(stem, value)
	return pos, nil
}

func (p *tomlParser) parseKeyValuePair(pos int) (int, tomlKey, any, error) {
	pos, key, err := p.parseKey(pos)
	if err != nil {
		return 0, nil, nil, err
	}
	if c, ok := p.at(pos); !ok || c != '=' {
		return 0, nil, nil, p.errAt("Expected '=' after a key in a key/value pair", pos)
	}
	pos++
	pos = p.skipWS(pos, false)
	pos, value, err := p.parseValue(pos)
	if err != nil {
		return 0, nil, nil, err
	}
	return pos, key, value, nil
}

func (p *tomlParser) parseKey(pos int) (int, tomlKey, error) {
	pos, part, err := p.parseKeyPart(pos)
	if err != nil {
		return 0, nil, err
	}
	key := tomlKey{part}
	pos = p.skipWS(pos, false)
	for {
		if c, ok := p.at(pos); !ok || c != '.' {
			return pos, key, nil
		}
		pos++
		pos = p.skipWS(pos, false)
		if pos, part, err = p.parseKeyPart(pos); err != nil {
			return 0, nil, err
		}
		key = append(key, part)
		pos = p.skipWS(pos, false)
	}
}

func (p *tomlParser) parseKeyPart(pos int) (int, string, error) {
	c, _ := p.at(pos)
	switch {
	case pos < len(p.src) && isBareKeyChar(c):
		start := pos
		for pos < len(p.src) && isBareKeyChar(p.src[pos]) {
			pos++
		}
		return pos, string(p.src[start:pos]), nil
	case c == '\'' && pos < len(p.src):
		return p.parseLiteralStr(pos)
	case c == '"' && pos < len(p.src):
		return p.parseBasicStr(pos+1, false)
	}
	return 0, "", p.errAt("Invalid initial character for a key part", pos)
}

func (p *tomlParser) parseArray(pos int) (int, any, error) {
	pos++
	array := []any{}
	pos, err := p.skipCommentsAndArrayWS(pos)
	if err != nil {
		return 0, nil, err
	}
	if p.startsWith("]", pos) {
		return pos + 1, array, nil
	}
	for {
		var val any
		if pos, val, err = p.parseValue(pos); err != nil {
			return 0, nil, err
		}
		array = append(array, val)
		if pos, err = p.skipCommentsAndArrayWS(pos); err != nil {
			return 0, nil, err
		}
		c, _ := p.at(pos)
		if pos < len(p.src) && c == ']' {
			return pos + 1, array, nil
		}
		if pos >= len(p.src) || c != ',' {
			return 0, nil, p.errAt("Unclosed array", pos)
		}
		pos++
		if pos, err = p.skipCommentsAndArrayWS(pos); err != nil {
			return 0, nil, err
		}
		if p.startsWith("]", pos) {
			return pos + 1, array, nil
		}
	}
}

func (p *tomlParser) parseInlineTable(pos int) (int, any, error) {
	pos++
	table := yaml.NewOMap()
	flags := newTOMLFlags()
	pos = p.skipWS(pos, false)
	if p.startsWith("}", pos) {
		return pos + 1, table, nil
	}
	for {
		var key tomlKey
		var value any
		var err error
		if pos, key, value, err = p.parseKeyValuePair(pos); err != nil {
			return 0, nil, err
		}
		parent, stem := key[:len(key)-1], key[len(key)-1]
		if flags.is(key, flagFrozen) {
			return 0, nil, p.errAt(fmt.Sprintf("Cannot mutate immutable namespace %s", key), pos)
		}
		nest, err := getOrCreateNest(table, parent, false)
		if err != nil {
			return 0, nil, p.errAt("Cannot overwrite a value", pos)
		}
		if nest.Has(stem) {
			return 0, nil, p.errAt(fmt.Sprintf("Duplicate inline table key %s", pyQuote(stem)), pos)
		}
		nest.Set(stem, value)
		pos = p.skipWS(pos, false)
		c, _ := p.at(pos)
		if pos < len(p.src) && c == '}' {
			return pos + 1, table, nil
		}
		if pos >= len(p.src) || c != ',' {
			return 0, nil, p.errAt("Unclosed inline table", pos)
		}
		if isContainer(value) {
			flags.set(key, flagFrozen, true)
		}
		pos++
		pos = p.skipWS(pos, false)
	}
}

var tomlEscapes = map[string]string{
	`\b`: "\b", `\t`: "\t", `\n`: "\n", `\f`: "\f", `\r`: "\r", `\"`: `"`, `\\`: `\`,
}

func (p *tomlParser) parseBasicStrEscape(pos int, multiline bool) (int, string, error) {
	end := min(pos+2, len(p.src))
	id := string(p.src[pos:end])
	pos += 2
	if multiline && (id == "\\ " || id == "\\\t" || id == "\\\n") {
		if id != "\\\n" {
			pos = p.skipWS(pos, false)
			c, ok := p.at(pos)
			if !ok {
				return pos, "", nil
			}
			if c != '\n' {
				return 0, "", p.errAt(`Unescaped '\' in a string`, pos)
			}
			pos++
		}
		return p.skipWS(pos, true), "", nil
	}
	switch id {
	case `\u`:
		return p.parseHexChar(pos, 4)
	case `\U`:
		return p.parseHexChar(pos, 8)
	}
	if r, ok := tomlEscapes[id]; ok {
		return pos, r, nil
	}
	return 0, "", p.errAt(`Unescaped '\' in a string`, pos)
}

func (p *tomlParser) parseHexChar(pos, n int) (int, string, error) {
	end := min(pos+n, len(p.src))
	hex := string(p.src[pos:end])
	valid := len([]rune(hex)) == n
	for _, c := range hex {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			valid = false
		}
	}
	if !valid {
		return 0, "", p.errAt("Invalid hex value", pos)
	}
	pos += n
	v, _ := strconv.ParseUint(hex, 16, 64)
	if !(v <= 55295 || v >= 57344 && v <= 1114111) {
		return 0, "", p.errAt("Escaped character is not a Unicode scalar value", pos)
	}
	return pos, string(rune(v)), nil
}

func (p *tomlParser) parseLiteralStr(pos int) (int, string, error) {
	pos++
	start := pos
	pos, err := p.skipUntil(pos, "'", illegalBasic, true)
	if err != nil {
		return 0, "", err
	}
	return pos + 1, string(p.src[start:pos]), nil
}

func (p *tomlParser) parseMultilineStr(pos int, literal bool) (int, string, error) {
	pos += 3
	if p.startsWith("\n", pos) {
		pos++
	}
	var delim, result string
	if literal {
		delim = "'"
		end, err := p.skipUntil(pos, "'''", illegalMultiline, true)
		if err != nil {
			return 0, "", err
		}
		result = string(p.src[pos:end])
		pos = end + 3
	} else {
		delim = `"`
		var err error
		if pos, result, err = p.parseBasicStr(pos, true); err != nil {
			return 0, "", err
		}
	}
	if !p.startsWith(delim, pos) {
		return pos, result, nil
	}
	pos++
	if !p.startsWith(delim, pos) {
		return pos, result + delim, nil
	}
	pos++
	return pos, result + delim + delim, nil
}

func (p *tomlParser) parseBasicStr(pos int, multiline bool) (int, string, error) {
	illegal := illegalBasic
	if multiline {
		illegal = illegalMultiline
	}
	var b strings.Builder
	start := pos
	for {
		c, ok := p.at(pos)
		if !ok {
			return 0, "", p.errAt("Unterminated string", pos)
		}
		if c == '"' {
			if !multiline {
				b.WriteString(string(p.src[start:pos]))
				return pos + 1, b.String(), nil
			}
			if p.startsWith(`"""`, pos) {
				b.WriteString(string(p.src[start:pos]))
				return pos + 3, b.String(), nil
			}
			pos++
			continue
		}
		if c == '\\' {
			b.WriteString(string(p.src[start:pos]))
			var esc string
			var err error
			if pos, esc, err = p.parseBasicStrEscape(pos, multiline); err != nil {
				return 0, "", err
			}
			b.WriteString(esc)
			start = pos
			continue
		}
		if illegal(c) {
			return 0, "", p.errAt(fmt.Sprintf("Illegal character %s", pyRuneRepr(c)), pos)
		}
		pos++
	}
}

const tomlTimeRe = `([01][0-9]|2[0-3]):([0-5][0-9]):([0-5][0-9])(?:\.([0-9]{1,6})[0-9]*)?`

var (
	tomlNumberRe    = regexp.MustCompile(`^(?:0(?:x[0-9A-Fa-f](?:_?[0-9A-Fa-f])*|b[01](?:_?[01])*|o[0-7](?:_?[0-7])*)|[+-]?(?:0|[1-9](?:_?[0-9])*)((?:\.[0-9](?:_?[0-9])*)?(?:[eE][+-]?[0-9](?:_?[0-9])*)?))`)
	tomlLocalTimeRe = regexp.MustCompile(`^` + tomlTimeRe)
	tomlDateTimeRe  = regexp.MustCompile(`^([0-9]{4})-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])(?:[Tt ]` + tomlTimeRe + `(?:([Zz])|([+-])([01][0-9]|2[0-3]):([0-5][0-9]))?)?`)
)

// matchAt runs re (anchored) against the source from pos, returning the
// submatches and the match length in runes.
func (p *tomlParser) matchAt(re *regexp.Regexp, pos int) ([]string, int) {
	rest := string(p.src[pos:])
	m := re.FindStringSubmatchIndex(rest)
	if m == nil {
		return nil, 0
	}
	groups := make([]string, len(m)/2)
	for i := range groups {
		if m[2*i] >= 0 {
			groups[i] = rest[m[2*i]:m[2*i+1]]
		} else {
			groups[i] = "\x00" // unmatched group
		}
	}
	return groups, len([]rune(rest[:m[1]]))
}

func (p *tomlParser) parseValue(pos int) (int, any, error) {
	c, _ := p.at(pos)
	inRange := pos < len(p.src)
	switch {
	case inRange && c == '"':
		if p.startsWith(`"""`, pos) {
			return toAny(p.parseMultilineStr(pos, false))
		}
		return toAny(p.parseBasicStr(pos+1, false))
	case inRange && c == '\'':
		if p.startsWith("'''", pos) {
			return toAny(p.parseMultilineStr(pos, true))
		}
		return toAny(p.parseLiteralStr(pos))
	case inRange && c == 't' && p.startsWith("true", pos):
		return pos + 4, true, nil
	case inRange && c == 'f' && p.startsWith("false", pos):
		return pos + 5, false, nil
	case inRange && c == '[':
		return p.parseArray(pos)
	case inRange && c == '{':
		return p.parseInlineTable(pos)
	}
	if !inRange {
		return 0, nil, p.errAt("Invalid value", pos)
	}
	if g, n := p.matchAt(tomlDateTimeRe, pos); g != nil {
		v, cause := tomlDateTime(g)
		if cause != "" {
			err := p.errAt("Invalid date or datetime", pos)
			err.(*tomlError).cause = cause
			return 0, nil, err
		}
		return pos + n, v, nil
	}
	if g, n := p.matchAt(tomlLocalTimeRe, pos); g != nil {
		return pos + n, tomlTime(g[1], g[2], g[3], g[4]), nil
	}
	if g, n := p.matchAt(tomlNumberRe, pos); g != nil {
		return pos + n, tomlNumber(g[0], g[1]), nil
	}
	end := min(pos+4, len(p.src))
	switch s := string(p.src[pos:min(pos+3, len(p.src))]); s {
	case "inf":
		return pos + 3, math.Inf(1), nil
	case "nan":
		return pos + 3, math.NaN(), nil
	}
	switch string(p.src[pos:end]) {
	case "+inf":
		return pos + 4, math.Inf(1), nil
	case "-inf":
		return pos + 4, math.Inf(-1), nil
	case "+nan", "-nan":
		return pos + 4, math.NaN(), nil
	}
	return 0, nil, p.errAt("Invalid value", pos)
}

func toAny(pos int, s string, err error) (int, any, error) { return pos, s, err }

// tomlNumber is match_to_number: int(s, 0) or float(s).
func tomlNumber(s, floatPart string) any {
	if floatPart != "" && floatPart != "\x00" {
		f, _ := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
		return f
	}
	digits := strings.ReplaceAll(s, "_", "")
	neg := false
	switch {
	case strings.HasPrefix(digits, "-"):
		neg, digits = true, digits[1:]
	case strings.HasPrefix(digits, "+"):
		digits = digits[1:]
	}
	base := 10
	if len(digits) > 1 && digits[0] == '0' {
		switch digits[1] {
		case 'x':
			base = 16
		case 'o':
			base = 8
		case 'b':
			base = 2
		}
		digits = digits[2:]
	}
	n, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return int64(0)
	}
	if neg {
		n.Neg(n)
	}
	if n.IsInt64() {
		return n.Int64()
	}
	return n
}

func tomlMicros(s string) int {
	if s == "\x00" || s == "" {
		return 0
	}
	n, _ := strconv.Atoi((s + "000000")[:6])
	return n
}

// tomlTime is a local time's isoformat().
func tomlTime(h, m, sec, frac string) string {
	out := h + ":" + m + ":" + sec
	if us := tomlMicros(frac); us != 0 {
		out += fmt.Sprintf(".%06d", us)
	}
	return out
}

// tomlDateTime is match_to_datetime's value as its isoformat(), or for
// a date that does not exist the ValueError datetime raises.
func tomlDateTime(g []string) (string, string) {
	year, _ := strconv.Atoi(g[1])
	month, _ := strconv.Atoi(g[2])
	day, _ := strconv.Atoi(g[3])
	if year < 1 {
		return "", fmt.Sprintf("year must be in 1..9999, not %d", year)
	}
	if last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day(); day > last {
		return "", fmt.Sprintf("day %d must be in range 1..%d for month %d in year %d", day, last, month, year)
	}
	date := g[1] + "-" + g[2] + "-" + g[3]
	if g[4] == "\x00" {
		return date, ""
	}
	out := date + "T" + tomlTime(g[4], g[5], g[6], g[7])
	switch {
	case g[9] != "\x00":
		out += g[9] + g[10] + ":" + g[11]
		if g[10] == "00" && g[11] == "00" && g[9] == "-" {
			out = out[:len(out)-6] + "+00:00"
		}
	case g[8] != "\x00":
		out += "+00:00"
	}
	return out, ""
}

// pyRuneRepr is repr() of a one-character str.
func pyRuneRepr(c rune) string {
	return pyQuote(string(c))
}
