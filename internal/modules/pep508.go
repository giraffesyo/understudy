package modules

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PEP 508 dependency specifiers as packaging.requirements.Requirement
// parses and prints them (see pep440.go for the release-dependent
// rules): the pip module's Package wraps one, so what it accepts and how
// it renders it decide the pip command line.

// pyRequirement is a parsed Requirement.
type pyRequirement struct {
	name   string
	extras []string
	spec   *pepSpecifierSet
	url    string
	marker []any // markerList: *markerItem, []any (a group) and "and"/"or"
	flavor pkgFlavor
}

// markerVar is a marker variable or a quoted value.
type markerVar struct {
	isVar bool
	value string
}

type markerItem struct {
	lhs, rhs markerVar
	op       string
}

// parseRequirement is Requirement(s); ok false where it raises
// InvalidRequirement (or, for marker strings Python cannot evaluate,
// another error).
func parseRequirement(s string, f pkgFlavor) (*pyRequirement, bool) {
	var r *pyRequirement
	var ok bool
	if f.legacy() {
		r, ok = parseRequirementLegacy(s, f)
	} else {
		r, ok = parseRequirementModern(s, f)
	}
	if !ok {
		return nil, false
	}
	r.flavor = f
	return r, true
}

// canonicalizeName is packaging.utils.canonicalize_name (PEP 503).
func canonicalizeName(name string) string {
	return strings.ToLower(pepNameSepRe.ReplaceAllString(name, "-"))
}

var pepNameSepRe = regexp.MustCompile(`[-_.]+`)

// String is str(Requirement).
func (r *pyRequirement) String() string {
	var b strings.Builder
	b.WriteString(r.name)
	if len(r.extras) > 0 {
		extras := append([]string(nil), dedupeStrings(r.extras)...)
		sort.Strings(extras)
		b.WriteString("[" + strings.Join(extras, ",") + "]")
	}
	if r.spec != nil {
		b.WriteString(r.spec.String())
	}
	if r.url != "" {
		if r.flavor.atLeast(26, 0) {
			b.WriteString(" @ " + r.url)
		} else {
			b.WriteString("@ " + r.url)
		}
		if r.marker != nil {
			b.WriteString(" ")
		}
	}
	if r.marker != nil {
		b.WriteString("; " + formatMarker(r.marker, true, r.flavor))
	}
	return b.String()
}

// hasSpecifier is bool(requirement.specifier).
func (r *pyRequirement) hasSpecifier() bool { return r.spec != nil && len(r.spec.specs) > 0 }

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// formatMarker is markers._format_marker. Before 26.3 unwrapping a
// redundant group restarted at the top level (dropping the parentheses
// a nested group needs).
func formatMarker(m any, first bool, f pkgFlavor) string {
	switch t := m.(type) {
	case []any:
		if len(t) == 1 {
			switch t[0].(type) {
			case []any, *markerItem:
				if f.atLeast(26, 3) {
					return formatMarker(t[0], first, f)
				}
				return formatMarker(t[0], true, f)
			}
		}
		inner := make([]string, len(t))
		for i, x := range t {
			inner[i] = formatMarker(x, false, f)
		}
		if first {
			return strings.Join(inner, " ")
		}
		return "(" + strings.Join(inner, " ") + ")"
	case *markerItem:
		return t.lhs.serialize(f) + " " + t.op + " " + t.rhs.serialize(f)
	case string:
		return t
	}
	return ""
}

func (v markerVar) serialize(f pkgFlavor) string {
	if v.isVar {
		return v.value
	}
	if f.atLeast(26, 3) && strings.Contains(v.value, `"`) && !strings.Contains(v.value, "'") {
		return "'" + v.value + "'"
	}
	return `"` + v.value + `"`
}

// --- packaging >= 22: the hand-written tokenizer and parser ---

type reqTokenizer struct {
	src string
	pos int
	f   pkgFlavor
}

// pyWordChar is a character Python's \w matches in a str pattern.
func pyWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Nl, r) ||
		unicode.Is(unicode.No, r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r)
}

// wordBoundary is \b at byte offset i.
func wordBoundary(s string, i int) bool {
	before, after := false, false
	if i > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		before = pyWordChar(r)
	}
	if i < len(s) {
		r, _ := utf8.DecodeRuneInString(s[i:])
		after = pyWordChar(r)
	}
	return before != after
}

// match returns the length of the rule's match at the current position,
// or -1.
func (t *reqTokenizer) match(rule string) int {
	s, i := t.src, t.pos
	rest := s[i:]
	lit := func(l string) int {
		if strings.HasPrefix(rest, l) {
			return len(l)
		}
		return -1
	}
	word := func(words ...string) int {
		if !wordBoundary(s, i) {
			return -1
		}
		for _, w := range words {
			if strings.HasPrefix(rest, w) && wordBoundary(s, i+len(w)) {
				return len(w)
			}
		}
		return -1
	}
	switch rule {
	case "(", ")", "[", "]", ";", ",", "@":
		return lit(rule)
	case "QUOTED_STRING":
		if rest == "" || (rest[0] != '\'' && rest[0] != '"') {
			return -1
		}
		if j := strings.IndexByte(rest[1:], rest[0]); j >= 0 {
			return j + 2
		}
		return -1
	case "OP":
		for _, op := range []string{"===", "==", "~=", "!=", "<=", ">=", "<", ">"} {
			if strings.HasPrefix(rest, op) {
				return len(op)
			}
		}
		return -1
	case "BOOLOP":
		return word("or", "and")
	case "IN":
		return word("in")
	case "NOT":
		return word("not")
	case "VARIABLE":
		vars := []string{"python_version", "python_full_version", "os_name", "os.name", "sys_platform",
			"sys.platform", "platform_release", "platform_system", "platform_version", "platform.version",
			"platform_machine", "platform.machine", "platform_python_implementation",
			"platform.python_implementation", "python_implementation", "implementation_name",
			"implementation_version"}
		if t.f.atLeast(25, 0) {
			vars = append(vars, "extras", "extra", "dependency_groups")
		} else {
			vars = append(vars, "extra")
		}
		return word(vars...)
	case "SPECIFIER":
		if loc := modernSpecTokenRe.FindStringIndex(rest); loc != nil {
			return loc[1]
		}
		return -1
	case "URL":
		n := 0
		for n < len(rest) && rest[n] != ' ' && rest[n] != '\t' {
			n++
		}
		if n == 0 {
			return -1
		}
		return n
	case "IDENTIFIER":
		if !wordBoundary(s, i) || rest == "" || !isASCIIAlnum(rest[0]) {
			return -1
		}
		n := 1
		for n < len(rest) && (isASCIIAlnum(rest[n]) || strings.IndexByte("._-", rest[n]) >= 0) {
			n++
		}
		for ; n > 0; n-- {
			if wordBoundary(s, i+n) {
				return n
			}
		}
		return -1
	case "WS":
		n := 0
		for n < len(rest) && (rest[n] == ' ' || rest[n] == '\t') {
			n++
		}
		if n == 0 {
			return -1
		}
		return n
	case "END":
		// "$" before 26.3 also matches before a final newline.
		if rest == "" || (!t.f.atLeast(26, 3) && rest == "\n") {
			return 0
		}
		return -1
	}
	return -1
}

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func (t *reqTokenizer) check(rule string) bool { return t.match(rule) >= 0 }

// read consumes the rule's token (which must match) and returns it.
func (t *reqTokenizer) read(rule string) string {
	n := t.match(rule)
	tok := t.src[t.pos : t.pos+n]
	t.pos += n
	return tok
}

func (t *reqTokenizer) consume(rule string) {
	if t.check(rule) {
		t.read(rule)
	}
}

type reqSyntaxError struct{}

func (t *reqTokenizer) expect(rule string) string {
	if !t.check(rule) {
		panic(reqSyntaxError{})
	}
	return t.read(rule)
}

func parseRequirementModern(s string, f pkgFlavor) (r *pyRequirement, ok bool) {
	defer func() {
		if e := recover(); e != nil {
			if _, syntax := e.(reqSyntaxError); !syntax {
				panic(e)
			}
			r, ok = nil, false
		}
	}()
	t := &reqTokenizer{src: s, f: f}
	t.consume("WS")
	r = &pyRequirement{name: t.expect("IDENTIFIER")}
	t.consume("WS")
	if t.check("[") {
		t.read("[")
		t.consume("WS")
		if t.check("IDENTIFIER") {
			r.extras = append(r.extras, t.read("IDENTIFIER"))
			for {
				t.consume("WS")
				if t.check("IDENTIFIER") {
					panic(reqSyntaxError{})
				}
				if !t.check(",") {
					break
				}
				t.read(",")
				t.consume("WS")
				r.extras = append(r.extras, t.expect("IDENTIFIER"))
			}
		}
		t.consume("WS")
		t.expect("]")
	}
	t.consume("WS")
	specText := ""
	if t.check("@") {
		t.read("@")
		t.consume("WS")
		r.url = t.expect("URL")
		if !t.check("END") {
			t.expect("WS")
			if !t.check("END") {
				r.marker = t.requirementMarker()
			}
		}
	} else {
		paren := t.check("(")
		if paren {
			t.read("(")
		}
		t.consume("WS")
		for t.check("SPECIFIER") {
			specText += t.read("SPECIFIER")
			t.consume("WS")
			if !t.check(",") {
				break
			}
			specText += t.read(",")
			t.consume("WS")
		}
		t.consume("WS")
		if paren {
			t.expect(")")
		}
		t.consume("WS")
		if !t.check("END") {
			r.marker = t.requirementMarker()
		}
	}
	t.expect("END")
	if r.url != "" && !f.atLeast(23, 2) && !pyURLValid(r.url) {
		return nil, false
	}
	spec, valid := parseSpecifierSet(specText, f)
	if !valid {
		return nil, false
	}
	r.spec = spec
	if r.marker != nil {
		r.marker = normalizeMarkerExtras(r.marker, f)
	}
	return r, true
}

func (t *reqTokenizer) requirementMarker() []any {
	t.expect(";")
	m := t.marker()
	t.consume("WS")
	return m
}

func (t *reqTokenizer) marker() []any {
	expr := []any{t.markerAtom()}
	for t.check("BOOLOP") {
		op := t.read("BOOLOP")
		expr = append(expr, op, t.markerAtom())
	}
	return expr
}

func (t *reqTokenizer) markerAtom() any {
	t.consume("WS")
	var atom any
	if t.check("(") {
		t.read("(")
		t.consume("WS")
		atom = t.marker()
		t.consume("WS")
		t.expect(")")
	} else {
		t.consume("WS")
		lhs := t.markerVar()
		t.consume("WS")
		op := t.markerOp()
		t.consume("WS")
		rhs := t.markerVar()
		t.consume("WS")
		atom = &markerItem{lhs: lhs, op: op, rhs: rhs}
	}
	t.consume("WS")
	return atom
}

func (t *reqTokenizer) markerVar() markerVar {
	switch {
	case t.check("VARIABLE"):
		v := strings.ReplaceAll(t.read("VARIABLE"), ".", "_")
		if v == "python_implementation" {
			v = "platform_python_implementation"
		}
		return markerVar{isVar: true, value: v}
	case t.check("QUOTED_STRING"):
		val, ok := pyLiteralString(t.read("QUOTED_STRING"))
		if !ok {
			panic(reqSyntaxError{})
		}
		return markerVar{value: val}
	}
	panic(reqSyntaxError{})
}

func (t *reqTokenizer) markerOp() string {
	switch {
	case t.check("IN"):
		t.read("IN")
		return "in"
	case t.check("NOT"):
		t.read("NOT")
		t.expect("WS")
		t.expect("IN")
		return "not in"
	case t.check("OP"):
		return t.read("OP")
	}
	panic(reqSyntaxError{})
}

// normalizeMarkerExtras canonicalizes the value compared with `extra`
// (PEP 685), as the parser does: 22-25 only in the first item, 26.0 in
// every top-level item, 26.3 in nested groups too and for the values
// tested `in extras` / `in dependency_groups`.
func normalizeMarkerExtras(m []any, f pkgFlavor) []any {
	norm := func(it *markerItem) *markerItem {
		c := *it
		switch {
		case c.lhs.isVar && c.lhs.value == "extra" && (!c.rhs.isVar || !f.atLeast(26, 0)):
			c.rhs = markerVar{value: canonicalizeName(c.rhs.value)}
		case c.rhs.isVar && c.rhs.value == "extra" && (!c.lhs.isVar || !f.atLeast(26, 0)):
			c.lhs = markerVar{value: canonicalizeName(c.lhs.value)}
		case f.atLeast(26, 3) && c.rhs.isVar && (c.rhs.value == "extras" || c.rhs.value == "dependency_groups") && !c.lhs.isVar:
			c.lhs = markerVar{value: canonicalizeName(c.lhs.value)}
		}
		return &c
	}
	if !f.atLeast(26, 0) {
		out := append([]any(nil), m...)
		if it, ok := out[0].(*markerItem); ok {
			out[0] = norm(it)
		}
		return out
	}
	var walk func(l []any, nested bool) []any
	walk = func(l []any, nested bool) []any {
		out := make([]any, len(l))
		for i, x := range l {
			switch t := x.(type) {
			case *markerItem:
				out[i] = norm(t)
			case []any:
				if f.atLeast(26, 3) {
					out[i] = walk(t, true)
				} else {
					out[i] = t
				}
			default:
				out[i] = x
			}
		}
		return out
	}
	return walk(m, false)
}

// pyLiteralString is ast.literal_eval of a quoted Python string literal
// (no prefix): its escapes decoded; ok false where Python raises.
func pyLiteralString(lit string) (string, bool) {
	body := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\n' || c == '\r' {
			return "", false // an unterminated single-line literal
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(body) {
			return "", false // the backslash escapes the closing quote
		}
		i++
		switch e := body[i]; e {
		case '\n':
		case '\\', '\'', '"':
			b.WriteByte(e)
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(body) && j < i+3 && body[j] >= '0' && body[j] <= '7' {
				j++
			}
			n, _ := strconv.ParseUint(body[i:j], 8, 32)
			b.WriteRune(rune(n))
			i = j - 1
		case 'x', 'u', 'U':
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[e]
			if i+1+width > len(body) {
				return "", false
			}
			n, err := strconv.ParseUint(body[i+1:i+1+width], 16, 32)
			if err != nil || n > unicode.MaxRune {
				return "", false
			}
			b.WriteRune(rune(n))
			i += width
		case 'N':
			return "", false // \N{name}: not modeled
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
		}
	}
	return b.String(), true
}

// pyURLValid is the URL check packaging made before 23.2: a file: URL
// must survive urlparse/urlunparse unchanged, any other needs a scheme
// and a network location.
func pyURLValid(url string) bool {
	scheme, rest := "", url
	if i := strings.IndexByte(url, ':'); i > 0 && (url[0] < 0x80 && unicode.IsLetter(rune(url[0]))) {
		okScheme := true
		for _, c := range url[:i] {
			if !(c < 0x80 && (unicode.IsLetter(c) || unicode.IsDigit(c)) || c == '+' || c == '-' || c == '.') {
				okScheme = false
				break
			}
		}
		if okScheme {
			scheme, rest = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	netloc, path := "", rest
	hasNetloc := strings.HasPrefix(rest, "//")
	if hasNetloc {
		end := len(rest)
		for _, d := range "/?#" {
			if j := strings.IndexRune(rest[2:], d); j >= 0 && j+2 < end {
				end = j + 2
			}
		}
		netloc, path = rest[2:end], rest[end:]
	}
	if scheme != "file" {
		return scheme != "" && netloc != ""
	}
	frag, query := "", ""
	if j := strings.IndexByte(path, '#'); j >= 0 {
		path, frag = path[:j], path[j+1:]
	}
	if j := strings.IndexByte(path, '?'); j >= 0 {
		path, query = path[:j], path[j+1:]
	}
	rebuilt := path
	if netloc != "" || rebuilt[:min(2, len(rebuilt))] != "//" {
		if rebuilt != "" && rebuilt[0] != '/' {
			rebuilt = "/" + rebuilt
		}
		rebuilt = "//" + netloc + rebuilt
	}
	rebuilt = scheme + ":" + rebuilt
	if query != "" {
		rebuilt += "?" + query
	}
	if frag != "" {
		rebuilt += "#" + frag
	}
	return rebuilt == url
}

// --- packaging < 22: the pyparsing grammar ---

type legacyParser struct {
	src string
	pos int
}

func (p *legacyParser) skipWS() {
	for p.pos < len(p.src) && strings.IndexByte(" \t\n\r", p.src[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *legacyParser) lit(l string) bool {
	p.skipWS()
	if strings.HasPrefix(p.src[p.pos:], l) {
		p.pos += len(l)
		return true
	}
	return false
}

// identifier is ALPHANUM + ZeroOrMore(ALPHANUM | PUNCTUATION* ALPHANUM),
// combined (no whitespace inside).
func (p *legacyParser) identifier() (string, bool) {
	p.skipWS()
	s, i := p.src, p.pos
	if i >= len(s) || !isASCIIAlnum(s[i]) {
		return "", false
	}
	j := i
	for j < len(s) && isASCIIAlnum(s[j]) {
		j++
	}
	for {
		k := j
		for k < len(s) && strings.IndexByte("-_.", s[k]) >= 0 {
			k++
		}
		if k < len(s) && isASCIIAlnum(s[k]) {
			for k < len(s) && isASCIIAlnum(s[k]) {
				k++
			}
			j = k
			continue
		}
		break
	}
	p.pos = j
	return s[i:j], true
}

// versionOne is VERSION_PEP440 ^ VERSION_LEGACY: the longer match.
func (p *legacyParser) versionOne() (string, bool) {
	p.skipWS()
	rest := p.src[p.pos:]
	best := -1
	for _, re := range []*regexp.Regexp{legacy20TokenRe, legacySpecTokenRe} {
		if loc := re.FindStringIndex(rest); loc != nil && loc[1] > best {
			best = loc[1]
		}
	}
	if best < 0 {
		return "", false
	}
	p.pos += best
	return rest[:best], true
}

func (p *legacyParser) versionMany() (string, bool) {
	first, ok := p.versionOne()
	if !ok {
		return "", false
	}
	parts := []string{first}
	for {
		save := p.pos
		if !p.lit(",") {
			break
		}
		v, ok := p.versionOne()
		if !ok {
			p.pos = save
			break
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, ","), true
}

var legacyMarkerVars = []string{"implementation_version", "platform_python_implementation", "implementation_name",
	"python_full_version", "platform_release", "platform_version", "platform_machine", "platform_system",
	"python_version", "sys_platform", "os_name", "os.name", "sys.platform", "platform.version",
	"platform.machine", "platform.python_implementation", "python_implementation", "extra"}

var legacyMarkerAliases = map[string]string{"os.name": "os_name", "sys.platform": "sys_platform",
	"platform.version": "platform_version", "platform.machine": "platform_machine",
	"platform.python_implementation": "platform_python_implementation",
	"python_implementation":          "platform_python_implementation"}

func (p *legacyParser) markerVar() (markerVar, bool) {
	p.skipWS()
	rest := p.src[p.pos:]
	for _, v := range legacyMarkerVars {
		if strings.HasPrefix(rest, v) {
			p.pos += len(v)
			if a, ok := legacyMarkerAliases[v]; ok {
				v = a
			}
			return markerVar{isVar: true, value: v}, true
		}
	}
	if rest != "" && (rest[0] == '\'' || rest[0] == '"') {
		for j := 1; j < len(rest); j++ {
			switch rest[j] {
			case '\n', '\r':
				return markerVar{}, false
			case rest[0]:
				p.pos += j + 1
				return markerVar{value: rest[1:j]}, true
			}
		}
	}
	return markerVar{}, false
}

func (p *legacyParser) markerOp() (string, bool) {
	for _, op := range []string{"===", "==", ">=", "<=", "!=", "~=", ">", "<", "not in", "in"} {
		if p.lit(op) {
			return op, true
		}
	}
	return "", false
}

func (p *legacyParser) markerAtom() (any, bool) {
	save := p.pos
	if lhs, ok := p.markerVar(); ok {
		if op, ok := p.markerOp(); ok {
			if rhs, ok := p.markerVar(); ok {
				return &markerItem{lhs: lhs, op: op, rhs: rhs}, true
			}
		}
	}
	p.pos = save
	if p.lit("(") {
		if expr, ok := p.markerExpr(); ok && p.lit(")") {
			return expr, true
		}
	}
	p.pos = save
	return nil, false
}

func (p *legacyParser) markerExpr() ([]any, bool) {
	atom, ok := p.markerAtom()
	if !ok {
		return nil, false
	}
	expr := []any{atom}
	for {
		save := p.pos
		op := ""
		switch {
		case p.lit("and"):
			op = "and"
		case p.lit("or"):
			op = "or"
		}
		if op == "" {
			break
		}
		rest, ok := p.markerExpr()
		if !ok {
			p.pos = save
			break
		}
		expr = append(append(expr, op), rest...)
	}
	return expr, true
}

func parseRequirementLegacy(s string, f pkgFlavor) (*pyRequirement, bool) {
	p := &legacyParser{src: s}
	name, ok := p.identifier()
	if !ok {
		return nil, false
	}
	r := &pyRequirement{name: name}
	save := p.pos
	if p.lit("[") {
		if e, ok := p.identifier(); ok {
			r.extras = append(r.extras, e)
			for {
				s2 := p.pos
				if !p.lit(",") {
					break
				}
				e, ok := p.identifier()
				if !ok {
					p.pos = s2
					break
				}
				r.extras = append(r.extras, e)
			}
		}
		if !p.lit("]") {
			r.extras = nil
			p.pos = save
		}
	}
	marker := func() bool {
		save := p.pos
		if !p.lit(";") {
			return true
		}
		m, ok := p.markerExpr()
		if !ok {
			p.pos = save
			return true
		}
		r.marker = m
		return true
	}
	specText := ""
	if p.lit("@") {
		p.skipWS()
		n := 0
		for p.pos+n < len(p.src) && p.src[p.pos+n] != ' ' {
			n++
		}
		if n == 0 {
			return nil, false
		}
		r.url = p.src[p.pos : p.pos+n]
		p.pos += n
		marker()
	} else {
		save := p.pos
		matched := false
		if p.lit("(") {
			if v, ok := p.versionMany(); ok && p.lit(")") {
				specText, matched = v, true
			}
		}
		if !matched {
			p.pos = save
			if v, ok := p.versionMany(); ok {
				specText = v
			} else {
				p.pos = save
			}
		}
		marker()
	}
	p.skipWS()
	if p.pos != len(p.src) {
		return nil, false
	}
	if r.url != "" && !pyURLValid(r.url) {
		return nil, false
	}
	spec, ok := parseSpecifierSet(specText, f)
	if !ok {
		return nil, false
	}
	r.spec = spec
	return r, true
}
