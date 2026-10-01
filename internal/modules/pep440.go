package modules

import (
	"regexp"
	"strings"
)

// PEP 440 versions and version specifiers as the Python `packaging`
// library implements them, which the pip module parses requirements
// with. Its behavior changed across releases (a pyparsing grammar with
// LegacyVersion/LegacySpecifier fallbacks before 22.0, a hand-written
// parser after, quoting and canonicalization fixes in 26.x), so the
// target's installed release selects the rules: see pkgFlavor.

// pkgFlavor is the release of `packaging` whose rules apply.
type pkgFlavor struct{ major, minor int }

// atLeast reports whether the flavor is release major.minor or later.
func (f pkgFlavor) atLeast(major, minor int) bool {
	return f.major > major || f.major == major && f.minor >= minor
}

// legacy is the pyparsing-based packaging (< 22.0), with its
// LegacyVersion and LegacySpecifier fallbacks.
func (f pkgFlavor) legacy() bool { return !f.atLeast(22, 0) }

// pkgFlavorOf parses a packaging release ("24.0", "26.3.dev0"); an
// unreadable one is taken as the newest behavior.
func pkgFlavorOf(version string) pkgFlavor {
	m := regexp.MustCompile(`^\s*(\d+)\.(\d+)`).FindStringSubmatch(version)
	if m == nil {
		return pkgFlavor{99, 0}
	}
	return pkgFlavor{atoiSafe(m[1]), atoiSafe(m[2])}
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' || n > 1<<20 {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// pepVersion is packaging.version.Version.
type pepVersion struct {
	epoch   string   // normalized digits
	release []string // normalized digits
	preL    string   // "a", "b", "rc" or ""
	preN    string
	hasPost bool
	post    string
	hasDev  bool
	dev     string
	local   []string // lower-cased, split on [-_.]
}

var pepVersionRe = regexp.MustCompile(`(?i)^[\s\v]*v?(?:(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
	`([-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` +
	`((?:-([0-9]+))|(?:[-_.]?(post|rev|r)[-_.]?([0-9]+)?))?` +
	`([-_.]?(dev)[-_.]?([0-9]+)?)?)` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?[\s\v]*$`)

// normDigits is int(s) rendered back: leading zeros dropped.
func normDigits(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// parsePEPVersion is Version(s); ok false where it raises InvalidVersion.
func parsePEPVersion(s string) (*pepVersion, bool) {
	m := pepVersionRe.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	v := &pepVersion{epoch: "0"}
	if m[1] != "" {
		v.epoch = normDigits(m[1])
	}
	for _, part := range strings.Split(m[2], ".") {
		v.release = append(v.release, normDigits(part))
	}
	if m[3] != "" {
		switch l := strings.ToLower(m[4]); l {
		case "alpha":
			v.preL = "a"
		case "beta":
			v.preL = "b"
		case "c", "pre", "preview":
			v.preL = "rc"
		default:
			v.preL = l
		}
		v.preN = normDigits(m[5])
	}
	if m[6] != "" {
		v.hasPost = true
		v.post = normDigits(m[7] + m[9])
	}
	if m[10] != "" {
		v.hasDev = true
		v.dev = normDigits(m[12])
	}
	if m[13] != "" {
		v.local = regexp.MustCompile(`[-_.]`).Split(strings.ToLower(m[13]), -1)
	}
	return v, true
}

func (v *pepVersion) String() string {
	var b strings.Builder
	if v.epoch != "0" {
		b.WriteString(v.epoch + "!")
	}
	b.WriteString(strings.Join(v.release, "."))
	if v.preL != "" {
		b.WriteString(v.preL + v.preN)
	}
	if v.hasPost {
		b.WriteString(".post" + v.post)
	}
	if v.hasDev {
		b.WriteString(".dev" + v.dev)
	}
	if len(v.local) > 0 {
		b.WriteString("+" + strings.Join(v.local, "."))
	}
	return b.String()
}

// public is the version without its local label.
func (v *pepVersion) public() *pepVersion {
	c := *v
	c.local = nil
	return &c
}

// base is base_version: epoch and release only.
func (v *pepVersion) base() *pepVersion {
	return &pepVersion{epoch: v.epoch, release: v.release}
}

func (v *pepVersion) isPrerelease() bool  { return v.preL != "" || v.hasDev }
func (v *pepVersion) isPostrelease() bool { return v.hasPost }

// numCmp compares normalized digit strings numerically.
func numCmp(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// trimRelease drops trailing zero components (keeping none, as _cmpkey).
func trimRelease(r []string) []string {
	n := len(r)
	for n > 0 && r[n-1] == "0" {
		n--
	}
	return r[:n]
}

// cmpInf compares (inf, value) pairs: inf -1 is NegativeInfinity, +1
// Infinity, 0 the value (compared by cmp).
func cmpInf(ai, bi int, cmp func() int) int {
	if ai != bi {
		if ai < bi {
			return -1
		}
		return 1
	}
	if ai != 0 {
		return 0
	}
	return cmp()
}

// pepVersionCmp orders versions by packaging's _cmpkey.
func pepVersionCmp(a, b *pepVersion) int {
	if c := numCmp(a.epoch, b.epoch); c != 0 {
		return c
	}
	ra, rb := trimRelease(a.release), trimRelease(b.release)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if c := numCmp(ra[i], rb[i]); c != 0 {
			return c
		}
	}
	if len(ra) != len(rb) {
		if len(ra) < len(rb) {
			return -1
		}
		return 1
	}
	preInf := func(v *pepVersion) int {
		switch {
		case v.preL == "" && !v.hasPost && v.hasDev:
			return -1
		case v.preL == "":
			return 1
		}
		return 0
	}
	if c := cmpInf(preInf(a), preInf(b), func() int {
		if c := strings.Compare(a.preL, b.preL); c != 0 {
			return c
		}
		return numCmp(a.preN, b.preN)
	}); c != 0 {
		return c
	}
	postInf := func(v *pepVersion) int {
		if !v.hasPost {
			return -1
		}
		return 0
	}
	if c := cmpInf(postInf(a), postInf(b), func() int { return numCmp(a.post, b.post) }); c != 0 {
		return c
	}
	devInf := func(v *pepVersion) int {
		if !v.hasDev {
			return 1
		}
		return 0
	}
	if c := cmpInf(devInf(a), devInf(b), func() int { return numCmp(a.dev, b.dev) }); c != 0 {
		return c
	}
	localInf := func(v *pepVersion) int {
		if v.local == nil {
			return -1
		}
		return 0
	}
	return cmpInf(localInf(a), localInf(b), func() int {
		for i := 0; i < len(a.local) && i < len(b.local); i++ {
			x, y := a.local[i], b.local[i]
			xn, yn := isDigits(x), isDigits(y)
			switch {
			case xn && yn:
				if c := numCmp(normDigits(x), normDigits(y)); c != 0 {
					return c
				}
			case xn:
				return 1 // (int, "") > (NegativeInfinity, str)
			case yn:
				return -1
			default:
				if c := strings.Compare(x, y); c != 0 {
					return c
				}
			}
		}
		switch {
		case len(a.local) < len(b.local):
			return -1
		case len(a.local) > len(b.local):
			return 1
		}
		return 0
	})
}

// canonicalizeVersion is packaging.utils.canonicalize_version: the
// normalized version, its release's trailing zeros stripped (keeping one
// component) when stripZero; an invalid version unaltered.
func canonicalizeVersion(s string, stripZero bool) string {
	v, ok := parsePEPVersion(s)
	if !ok {
		return s
	}
	if stripZero {
		c := *v
		r := trimRelease(v.release)
		if len(r) == 0 {
			r = []string{"0"}
		}
		c.release = r
		return c.String()
	}
	return v.String()
}

// legacyVersionKey is packaging (< 22)'s _legacy_cmpkey, for versions
// that are not PEP 440 (LegacyVersion): such a version sorts before
// every PEP 440 one.
func legacyVersionKey(s string) []string {
	var parts []string
	for _, part := range legacyComponentRe.FindAllString(strings.ToLower(s), -1) {
		switch part {
		case "pre", "preview", "rc":
			part = "c"
		case "-":
			part = "final-"
		case "dev":
			part = "@"
		}
		if part == "" || part == "." {
			continue
		}
		if part[0] >= '0' && part[0] <= '9' {
			for len(part) < 8 {
				part = "0" + part
			}
		} else {
			part = "*" + part
		}
		if strings.HasPrefix(part, "*") {
			if part < "*final" {
				for len(parts) > 0 && parts[len(parts)-1] == "*final-" {
					parts = parts[:len(parts)-1]
				}
			}
			for len(parts) > 0 && parts[len(parts)-1] == "00000000" {
				parts = parts[:len(parts)-1]
			}
		}
		parts = append(parts, part)
	}
	// _parse_version_parts always ends with "*final".
	for len(parts) > 0 && parts[len(parts)-1] == "00000000" {
		parts = parts[:len(parts)-1]
	}
	return append(parts, "*final")
}

// legacyComponentRe splits as _legacy_version_component_re.split does:
// the delimiters (digit runs, letter runs, "." and "-") are kept, and
// the text between them (anything else) too.
var legacyComponentRe = regexp.MustCompile(`\d+|[a-z]+|\.|-|[^\da-z.\-]+`)

func legacyKeyCmp(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// pepSpecifier is one packaging.specifiers.Specifier (or, before 22.0,
// a LegacySpecifier).
type pepSpecifier struct {
	op, version string
	legacy      bool
}

func (s pepSpecifier) String() string { return s.op + s.version }

// The Specifier grammar: modern is packaging 23+'s (the requirement
// tokenizer's SPECIFIER token too), legacy20 the pyparsing releases'
// (whose === takes any non-space text), legacySpec their
// LegacySpecifier.
const (
	pepPre      = `(?:[-_.]?(?:alpha|beta|preview|pre|a|b|c|rc)[-_.]?[0-9]*)?`
	pepPost     = `(?:(?:-[0-9]+)|(?:[-_.]?(?:post|rev|r)[-_.]?[0-9]*))?`
	pepDev      = `(?:[-_.]?dev[-_.]?[0-9]*)?`
	pepLocal    = `(?:\+[a-z0-9]+(?:[-_.][a-z0-9]+)*)?`
	pepEqVer    = `[\s\v]*v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*(?:\.\*|` + pepPre + pepPost + pepDev + pepLocal + `)?`
	pepCompat   = `[\s\v]*v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)+` + pepPre + pepPost + pepDev
	pepOrdered  = `[\s\v]*v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*` + pepPre + pepPost + pepDev
	modernSpec  = `(?:===[\s\v]*[^\s\v;)]*|(?:==|!=)` + pepEqVer + `|~=` + pepCompat + `|(?:<=|>=|<|>)` + pepOrdered + `)`
	legacy20Eq  = `[\s\v]*v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*` + pepPre + pepPost + `(?:` + pepDev + pepLocal + `|\.\*)?`
	legacy20    = `(?:===[\s\v]*[^\s\v]*|(?:==|!=)` + legacy20Eq + `|~=` + pepCompat + `|(?:<=|>=|<|>)` + pepOrdered + `)`
	legacySpecX = `(?:==|!=|<=|>=|<|>)[\s\v]*[^,;\s\v)]*`
)

var (
	modernSpecFullRe   = regexp.MustCompile(`(?i)^[\s\v]*` + modernSpec + `[\s\v]*$`)
	modernSpecTokenRe  = regexp.MustCompile(`(?i)^` + modernSpec)
	legacy20SpecFullRe = regexp.MustCompile(`(?i)^[\s\v]*` + legacy20 + `[\s\v]*$`)
	legacy20TokenRe    = regexp.MustCompile(`(?i)^` + legacy20)
	legacySpecFullRe   = regexp.MustCompile(`(?i)^[\s\v]*` + legacySpecX + `[\s\v]*$`)
	legacySpecTokenRe  = regexp.MustCompile(`(?i)^` + legacySpecX)
)

// parseSpecifier is Specifier(s) (falling back to LegacySpecifier before
// 22.0); ok false where it raises InvalidSpecifier.
func parseSpecifier(s string, f pkgFlavor) (pepSpecifier, bool) {
	legacy := false
	if f.legacy() {
		if !legacy20SpecFullRe.MatchString(s) {
			if !legacySpecFullRe.MatchString(s) {
				return pepSpecifier{}, false
			}
			legacy = true
		}
	} else if !modernSpecFullRe.MatchString(s) {
		return pepSpecifier{}, false
	}
	s = strings.TrimSpace(s)
	op := s[:1]
	if strings.HasPrefix(s, "===") {
		op = "==="
	} else if len(s) >= 2 {
		switch s[:2] {
		case "~=", "==", "!=", "<=", ">=":
			op = s[:2]
		}
	}
	return pepSpecifier{op: op, version: strings.TrimSpace(s[len(op):]), legacy: legacy}, true
}

// pepSpecifierSet is packaging.specifiers.SpecifierSet.
type pepSpecifierSet struct {
	specs     []pepSpecifier
	arbitrary bool // the source had "==="
	flavor    pkgFlavor
}

// parseSpecifierSet is SpecifierSet(s): comma-separated specifiers.
func parseSpecifierSet(s string, f pkgFlavor) (*pepSpecifierSet, bool) {
	set := &pepSpecifierSet{flavor: f, arbitrary: strings.Contains(s, "===")}
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		spec, ok := parseSpecifier(strings.TrimSpace(part), f)
		if !ok {
			return nil, false
		}
		set.specs = append(set.specs, spec)
	}
	return set, true
}

// canonical is a specifier's _canonical_spec, which equality and so
// de-duplication use.
func (s pepSpecifier) canonical(f pkgFlavor) string {
	if s.op == "===" || strings.HasSuffix(s.version, ".*") || s.legacy {
		return s.op + "\x00" + s.version
	}
	return s.op + "\x00" + canonicalizeVersion(s.version, f.legacy() || s.op != "~=")
}

// String is str(SpecifierSet): the specifiers de-duplicated and sorted
// (26.1 sorts first, then keeps the first of equal ones; earlier releases
// kept the first given).
func (s *pepSpecifierSet) String() string {
	strs := func(specs []pepSpecifier) []string {
		out := make([]string, len(specs))
		for i, sp := range specs {
			out[i] = sp.String()
		}
		return out
	}
	specs := append([]pepSpecifier(nil), s.specs...)
	dedupe := func(in []pepSpecifier) []pepSpecifier {
		seen := map[string]bool{}
		var out []pepSpecifier
		for _, sp := range in {
			k := sp.canonical(s.flavor)
			if !seen[k] {
				seen[k] = true
				out = append(out, sp)
			}
		}
		return out
	}
	sortSpecs := func(in []pepSpecifier) {
		// sorted(key=str): a stable sort on the strings.
		for i := 1; i < len(in); i++ {
			for j := i; j > 0 && in[j].String() < in[j-1].String(); j-- {
				in[j], in[j-1] = in[j-1], in[j]
			}
		}
	}
	if s.flavor.atLeast(26, 1) {
		sortSpecs(specs)
		specs = dedupe(specs)
	} else {
		specs = dedupe(specs)
		sortSpecs(specs)
	}
	return strings.Join(strs(specs), ",")
}

// errInvalidVersion is the InvalidVersion packaging 22-25 raise when
// asked whether a version that is not PEP 440 is contained.
type errInvalidVersion struct{ version string }

func (e errInvalidVersion) Error() string { return "Invalid version: " + pyStrRepr(e.version) }

// contains is SpecifierSet.contains(item, prereleases=True), the pip
// module's is_satisfied_by.
func (s *pepSpecifierSet) contains(item string) (bool, error) {
	f := s.flavor
	v, ok := parsePEPVersion(item)
	if f.legacy() {
		for _, sp := range s.specs {
			if !sp.containsLegacy(item, v, ok, f) {
				return false, nil
			}
		}
		return true, nil
	}
	if !ok {
		if !f.atLeast(26, 0) {
			return false, errInvalidVersion{item}
		}
		// An unparsable version matches only === specifiers.
		for _, sp := range s.specs {
			if sp.op != "===" || !strings.EqualFold(item, sp.version) {
				return false, nil
			}
		}
		return true, nil
	}
	for _, sp := range s.specs {
		if !sp.matches(v, item, f) {
			return false, nil
		}
	}
	return true, nil
}

// matches is a PEP 440 specifier's operator applied to a valid version
// (raw is the version as given, which === compares from 26.1 on).
func (s pepSpecifier) matches(v *pepVersion, raw string, f pkgFlavor) bool {
	switch s.op {
	case "===":
		if f.atLeast(26, 1) {
			return strings.EqualFold(raw, s.version)
		}
		return strings.EqualFold(v.String(), s.version)
	case "==":
		return pepEqual(v, s.version)
	case "!=":
		return !pepEqual(v, s.version)
	case "~=":
		return pepCompatible(v, s.version, f)
	}
	spec, ok := parsePEPVersion(s.version)
	if !ok {
		return false
	}
	switch s.op {
	case "<=":
		return pepVersionCmp(v.public(), spec) <= 0
	case ">=":
		return pepVersionCmp(v.public(), spec) >= 0
	case "<":
		if pepVersionCmp(v, spec) >= 0 {
			return false
		}
		if !spec.isPrerelease() && v.isPrerelease() && pepVersionCmp(v.base(), spec.base()) == 0 {
			return false
		}
		return true
	case ">":
		if pepVersionCmp(v, spec) <= 0 {
			return false
		}
		if !spec.isPostrelease() && v.isPostrelease() && pepVersionCmp(v.base(), spec.base()) == 0 {
			return false
		}
		if v.local != nil && pepVersionCmp(v.base(), spec.base()) == 0 {
			return false
		}
		return true
	}
	return false
}

// pepEqual is the == operator: a .* prefix match on the release, else
// equality (ignoring the candidate's local label when the specifier has
// none).
func pepEqual(v *pepVersion, spec string) bool {
	if prefix, ok := strings.CutSuffix(spec, ".*"); ok {
		splitSpec := pepVersionSplit(canonicalizeVersion(prefix, false))
		splitV := pepVersionSplit(canonicalizeVersion(v.public().String(), false))
		padded, _ := pepPadVersion(splitV, splitSpec)
		if len(padded) > len(splitSpec) {
			padded = padded[:len(splitSpec)]
		}
		if len(padded) != len(splitSpec) {
			return false
		}
		for i := range padded {
			if padded[i] != splitSpec[i] {
				return false
			}
		}
		return true
	}
	sv, ok := parsePEPVersion(spec)
	if !ok {
		return false
	}
	if sv.local == nil {
		v = v.public()
	}
	return pepVersionCmp(v, sv) == 0
}

// pepCompatible is ~=V: >=V and the release prefix without its last
// component. Before 21.3 the prefix was cut from the version's text
// split at its pre-release suffix as well (so ~=1.4.5a1 meant ==1.4.5.*).
func pepCompatible(v *pepVersion, spec string, f pkgFlavor) bool {
	sv, ok := parsePEPVersion(spec)
	if !ok || pepVersionCmp(v.public(), sv) < 0 {
		return false
	}
	var prefix []string
	if f.atLeast(21, 3) {
		prefix = sv.release[:len(sv.release)-1]
		if sv.epoch != "0" {
			return pepEqual(v, sv.epoch+"!"+strings.Join(prefix, ".")+".*")
		}
		return pepEqual(v, strings.Join(prefix, ".")+".*")
	}
	for _, part := range pepVersionSplit(spec)[1:] {
		if strings.HasPrefix(part, "post") || strings.HasPrefix(part, "dev") {
			break
		}
		prefix = append(prefix, part)
	}
	if len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return pepEqual(v, strings.Join(prefix, ".")+".*")
}

var pepPrefixRe = regexp.MustCompile(`^([0-9]+)((?:a|b|c|rc)[0-9]+)$`)

// pepVersionSplit is _version_split: the epoch, then the dot-separated
// parts with a numeric part's pre-release suffix split off.
func pepVersionSplit(version string) []string {
	epoch, rest := "0", version
	if i := strings.LastIndex(version, "!"); i >= 0 {
		epoch, rest = version[:i], version[i+1:]
		if epoch == "" {
			epoch = "0"
		}
	}
	out := []string{epoch}
	for _, item := range strings.Split(rest, ".") {
		if m := pepPrefixRe.FindStringSubmatch(item); m != nil {
			out = append(out, m[1], m[2])
		} else {
			out = append(out, item)
		}
	}
	return out
}

// pepPadVersion is _pad_version: both release prefixes zero-padded to
// the same length.
func pepPadVersion(left, right []string) ([]string, []string) {
	digits := func(s []string) int {
		n := 0
		for n < len(s) && isDigits(s[n]) {
			n++
		}
		return n
	}
	ln, rn := digits(left), digits(right)
	pad := func(s []string, n, to int) []string {
		out := append([]string{}, s[:n]...)
		for i := n; i < to; i++ {
			out = append(out, "0")
		}
		return append(out, s[n:]...)
	}
	return pad(left, ln, max(ln, rn)), pad(right, rn, max(ln, rn))
}

// containsLegacy is a packaging (< 22) specifier's contains: PEP 440
// operators match only PEP 440 versions (=== compares text), a
// LegacySpecifier compares LegacyVersion keys.
func (s pepSpecifier) containsLegacy(raw string, v *pepVersion, valid bool, f pkgFlavor) bool {
	if s.legacy {
		a := legacyKeyOf(raw, v, valid)
		b := legacyVersionKey(s.version)
		c := legacyKeyCmp(a, b)
		switch s.op {
		case "==":
			return c == 0
		case "!=":
			return c != 0
		case "<=":
			return c <= 0
		case ">=":
			return c >= 0
		case "<":
			return c < 0
		case ">":
			return c > 0
		}
		return false
	}
	if s.op == "===" {
		text := raw
		if valid {
			text = v.String()
		}
		return strings.EqualFold(text, s.version)
	}
	if !valid {
		return false
	}
	return s.matches(v, raw, f)
}

// legacyKeyOf is LegacyVersion(str(version))'s key: a PEP 440 version
// is first rendered normalized.
func legacyKeyOf(raw string, v *pepVersion, valid bool) []string {
	if valid {
		return legacyVersionKey(v.String())
	}
	return legacyVersionKey(raw)
}
