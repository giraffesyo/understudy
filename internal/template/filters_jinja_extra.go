package template

import (
	"errors"
	"fmt"
	"html"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// registerJinjaExtraFilters installs the rest of Jinja2's builtin
// filters: items, attr, wordcount, striptags, filesizeformat, urlencode,
// xmlattr, urlize, pprint (escape and its kin are in markup.go).
func registerJinjaExtraFilters(e *Engine) {
	f := e.Filters

	// do_items(value): the (key, value) pairs of a mapping.
	f["items"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		keys, m, ok := orderedMap(in)
		if !ok {
			return nil, errors.New("Can only get item pairs from a mapping.")
		}
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = []any{k, m[k]}
		}
		return out, nil
	}

	// do_attr(environment, obj, name): getattr(obj, name), undefined
	// when obj has no such attribute (a mapping's keys are items, not
	// attributes).
	f["attr"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		nameV, _ := filterArg(args, 0, kwargs, "name")
		name := toStr(nameV)
		if o, ok := Undeprecate(in).(pyObject); ok {
			if v, ok := o.PyAttr(name); ok {
				return v, nil
			}
		}
		if m, ok := lookupMethod(Undeprecate(in), name); ok {
			return m, nil
		}
		return missingItem(in, name), nil
	}

	// do_wordcount(s): len(re.findall(r"\w+", soft_str(s))).
	f["wordcount"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		n := 0
		inWord := false
		for _, r := range s {
			w := isPyWordChar(r)
			if w && !inWord {
				n++
			}
			inWord = w
		}
		return int64(n), nil
	}

	// do_striptags(value): Markup(str(value)).striptags().
	f["striptags"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return markupStriptags(htmlOf(in)), nil
	}

	// do_filesizeformat(value, binary=False).
	f["filesizeformat"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		size, err := pyFloat(Undeprecate(in))
		if err != nil {
			return nil, err
		}
		binary := false
		if v, ok := filterArg(args, 0, kwargs, "binary"); ok {
			binary = truthy(v)
		}
		return filesizeformat(size, binary)
	}

	// do_urlencode(value): a string (or anything not iterable) quoted for
	// a URL path, a dict or iterable of pairs as a query string.
	f["urlencode"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		v := Undeprecate(in)
		if _, isStr := asString(v); isStr {
			return urlQuote(v, false), nil
		}
		var pairs [][2]any
		if keys, m, ok := orderedMap(v); ok {
			for _, k := range keys {
				pairs = append(pairs, [2]any{k, m[k]})
			}
		} else {
			items, err := iterate(v)
			if err != nil {
				return urlQuote(v, false), nil
			}
			for _, item := range items {
				kv, err := unpack2(item)
				if err != nil {
					return nil, err
				}
				pairs = append(pairs, [2]any{kv[0], kv[1]})
			}
		}
		parts := make([]string, len(pairs))
		for i, p := range pairs {
			parts[i] = urlQuote(p[0], true) + "=" + urlQuote(p[1], true)
		}
		return strings.Join(parts, "&"), nil
	}

	// do_xmlattr(eval_ctx, d, autospace=True).
	f["xmlattr"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		keys, m, ok := orderedMap(in)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'items'", pyClassName(in, ec.fromVar(-1)))
		}
		var items []string
		for _, k := range keys {
			v := Undeprecate(m[k])
			if v == nil || isUndefined(v) {
				continue
			}
			if strings.ContainsAny(k, " \t\n\r\f\v/>=") {
				return nil, fmt.Errorf("Invalid character in attribute name: %s", pyStrRepr(k))
			}
			items = append(items, fmt.Sprintf(`%s="%s"`, htmlEscape(k), markupEscape(v)))
		}
		rv := strings.Join(items, " ")
		autospace := true
		if v, ok := filterArg(args, 0, kwargs, "autospace"); ok {
			autospace = truthy(v)
		}
		if autospace && rv != "" {
			rv = " " + rv
		}
		return rv, nil
	}

	f["urlize"] = filterUrlize

	// do_pprint(value): pprint.pformat. ansible-core passes plugins its
	// containers as lazy wrappers, whose own __repr__ pprint does not
	// know: they come out as their repr. Only a long string is wrapped.
	f["pprint"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		v := Undeprecate(in)
		if s, ok := v.(string); ok {
			return pprintStr(s), nil
		}
		if s, ok := asString(v); ok && !isMarkup(v) {
			return pprintStr(s), nil
		}
		return pyRepr(v), nil
	}
}

// isPyWordChar is \w in a Python str pattern (str.isalnum or _).
func isPyWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// filesizeformat is Jinja's do_filesizeformat on float(value).
func filesizeformat(size float64, binary bool) (string, error) {
	base := 1000.0
	prefixes := []string{"kB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}
	if binary {
		base = 1024
		prefixes = []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB", "ZiB", "YiB"}
	}
	switch {
	case size == 1:
		return "1 Byte", nil
	case size < base:
		n, ok := floatToInt(size)
		if !ok {
			if math.IsNaN(size) {
				return "", errors.New("cannot convert float NaN to integer")
			}
			return "", errors.New("cannot convert float infinity to integer")
		}
		return toStr(n) + " Bytes", nil
	}
	var unit float64
	var prefix string
	for i, p := range prefixes {
		unit = math.Pow(base, float64(i+2))
		prefix = p
		if size < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", base*size/unit, prefix), nil
}

// unpack2 is `k, v = item` in Python.
func unpack2(item any) ([2]any, error) {
	v := Undeprecate(item)
	items, err := iterate(v)
	if err != nil {
		return [2]any{}, fmt.Errorf("cannot unpack non-iterable %s object", pyClassName(v, false))
	}
	switch {
	case len(items) < 2:
		return [2]any{}, fmt.Errorf("not enough values to unpack (expected 2, got %d)", len(items))
	case len(items) > 2:
		return [2]any{}, errors.New("too many values to unpack (expected 2)")
	}
	return [2]any{items[0], items[1]}, nil
}

// urlQuote is Jinja's url_quote: str() of the value as UTF-8, quoted by
// urllib's quote_from_bytes ("/" kept, unless forQS, which also turns
// spaces into "+").
func urlQuote(v any, forQS bool) string {
	s, ok := asString(Undeprecate(v))
	if !ok {
		s = toStr(v)
	}
	safe := "/"
	if forQS {
		safe = ""
	}
	rv := pyQuote(s, safe)
	if forQS {
		rv = strings.ReplaceAll(rv, "%20", "+")
	}
	return rv
}

// pyQuote is urllib.parse.quote_from_bytes over s's UTF-8 bytes: ASCII
// letters, digits, "_.-~" and the safe characters kept.
func pyQuote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-', c == '~', strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// pyUnquotePlus is urllib.parse.unquote_plus: "+" is a space, %XX
// sequences are bytes, decoded as UTF-8 with errors replaced.
func pyUnquotePlus(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var raw []byte
	unhex := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s)+0 && i+2 <= len(s)-1 {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				raw = append(raw, hi<<4|lo)
				i += 2
				continue
			}
		}
		raw = append(raw, s[i])
	}
	return pyDecodeUTF8Replace(raw)
}

// pyDecodeUTF8Replace is bytes.decode('utf-8', 'replace').
func pyDecodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// pprintStr is pprint.pformat of a str: its repr, or when that is wider
// than 80 columns, the repr of its lines (and of their words, when a line
// is too wide itself) as adjacent literals in parentheses.
func pprintStr(s string) string {
	rep := pyStrRepr(s)
	if utf8.RuneCountInString(rep) <= 80 || s == "" {
		return rep
	}
	const width = 80
	indent, allowance := 1, 1
	var chunks []string
	lines := pySplitlinesKeep(s)
	maxWidth1 := width - indent
	maxWidth := maxWidth1
	rlen := func(x string) int { return utf8.RuneCountInString(pyStrRepr(x)) }
	for i, line := range lines {
		rep = pyStrRepr(line)
		if i == len(lines)-1 {
			maxWidth1 -= allowance
		}
		if utf8.RuneCountInString(rep) <= maxWidth1 {
			chunks = append(chunks, rep)
			continue
		}
		parts := pyWordSpaceParts(line)
		maxWidth2 := maxWidth
		current := ""
		for j, part := range parts {
			candidate := current + part
			if j == len(parts)-1 && i == len(lines)-1 {
				maxWidth2 -= allowance
			}
			if rlen(candidate) > maxWidth2 {
				if current != "" {
					chunks = append(chunks, pyStrRepr(current))
				}
				current = part
			} else {
				current = candidate
			}
		}
		if current != "" {
			chunks = append(chunks, pyStrRepr(current))
		}
	}
	if len(chunks) == 1 {
		return rep
	}
	return "(" + strings.Join(chunks, "\n"+strings.Repeat(" ", indent)) + ")"
}

// pyWordSpaceParts is re.findall(r'\S*\s*', s) less its final empty
// match.
func pyWordSpaceParts(s string) []string {
	var parts []string
	r := []rune(s)
	i := 0
	for i < len(r) {
		start := i
		for i < len(r) && !isPySpace(r[i]) {
			i++
		}
		for i < len(r) && isPySpace(r[i]) {
			i++
		}
		parts = append(parts, string(r[start:i]))
	}
	return parts
}

// pySplitlinesKeep is str.splitlines(True).
func pySplitlinesKeep(s string) []string {
	var out []string
	r := []rune(s)
	start := 0
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case '\r':
			if i+1 < len(r) && r[i+1] == '\n' {
				i++
			}
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			continue
		}
		out = append(out, string(r[start:i+1]))
		start = i + 1
	}
	if start < len(r) {
		out = append(out, string(r[start:]))
	}
	return out
}

// urlize's patterns (jinja2.utils), with Python's Unicode \w.
var (
	urlizeHTTPRe = regexp.MustCompile(`(?i)^(` +
		`(https?://|www\.)(([` + pyWordClass + `%-]+\.)+)?([a-z]{2,63}|xn--[` + pyWordClass + `%]{2,59})` +
		`|([` + pyWordClass + `%-]{2,63}\.)+(com|net|int|edu|gov|org|info|mil)` +
		`|(https?://)((([\d]{1,3})(\.[\d]{1,3}){3})|(\[([\da-f]{0,4}:){2}([\da-f]{0,4}:?){1,6}]))` +
		`)(?::[\d]{1,5})?(?:[/?#][^` + pySpaceClass + `]*)?$`)
	urlizeEmailRe  = regexp.MustCompile(`^[^` + pySpaceClass + `]+@[` + pyWordClass + `][` + pyWordClass + `.-]*\.[` + pyWordClass + `]+$`)
	urlizeHeadRe   = regexp.MustCompile(`^([(<]|&lt;)+`)
	urlizeTailRe   = regexp.MustCompile(`([)>.,\n]|&gt;)+$`)
	urlizeSchemeRe = regexp.MustCompile(`^([` + pyWordClass + `.+-]{2,}:(/){0,2})$`)
	urlizeSpaceRe  = regexp.MustCompile(`[` + pySpaceClass + `]+`)
)

// pyWordClass and pySpaceClass are the contents of a character class
// matching Python's Unicode \w and \s.
const (
	pyWordClass  = `\p{L}\p{N}_`
	pySpaceClass = `\t\n\v\f\r\x1c-\x1f \x85\xa0\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`
)

// filterUrlize is do_urlize(eval_ctx, value, trim_url_limit=None,
// nofollow=False, target=None, rel=None, extra_schemes=None), with
// Jinja's default policies (rel "noopener", no target or extra
// schemes).
func filterUrlize(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	param := func(i int, name string) any {
		v, _ := filterArg(args, i, kwargs, name)
		return Undeprecate(v)
	}
	trimV, nofollow, targetV, relV, schemesV := param(0, "trim_url_limit"), param(1, "nofollow"), param(2, "target"), param(3, "rel"), param(4, "extra_schemes")
	relParts := map[string]bool{}
	if relV != nil {
		for _, p := range strings.Fields(toStr(relV)) {
			relParts[p] = true
		}
	}
	if truthy(nofollow) {
		relParts["nofollow"] = true
	}
	relParts["noopener"] = true
	var rel []string
	for p := range relParts {
		rel = append(rel, p)
	}
	sort.Strings(rel)
	var schemes []string
	hasSchemes := false
	if schemesV != nil {
		hasSchemes = true
		items, err := iterate(schemesV)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			s, ok := asString(it)
			if !ok {
				return nil, fmt.Errorf("expected string or bytes-like object, got '%s'", pyClassName(it, false))
			}
			if !urlizeSchemeRe.MatchString(s) {
				return nil, fmt.Errorf("%s is not a valid URI scheme prefix.", pyStrRepr(s))
			}
			schemes = append(schemes, s)
		}
	}
	trim := -1
	if trimV != nil {
		n, ok := asInt(trimV)
		if !ok {
			return nil, fmt.Errorf("'>' not supported between instances of 'int' and '%s'", pyClassName(trimV, ec.fromVar(0)))
		}
		trim = int(n)
	}
	target := ""
	if targetV != nil {
		target = toStr(targetV)
	}
	return urlize(htmlEscape(htmlOf(in)), trim, strings.Join(rel, " "), target, schemes, hasSchemes), nil
}

// urlize is jinja2.utils.urlize over already escaped text.
func urlize(text string, trimLimit int, rel, target string, schemes []string, hasSchemes bool) string {
	trimURL := func(x string) string {
		if trimLimit >= 0 && utf8.RuneCountInString(x) > trimLimit {
			return string([]rune(x)[:trimLimit]) + "..."
		}
		return x
	}
	relAttr, targetAttr := "", ""
	if rel != "" {
		relAttr = fmt.Sprintf(` rel="%s"`, htmlEscape(rel))
	}
	if target != "" {
		targetAttr = fmt.Sprintf(` target="%s"`, htmlEscape(target))
	}
	// re.split(r"(\s+)", text): words and the whitespace between them.
	var words []string
	last := 0
	for _, m := range urlizeSpaceRe.FindAllStringIndex(text, -1) {
		words = append(words, text[last:m[0]], text[m[0]:m[1]])
		last = m[1]
	}
	words = append(words, text[last:])
	for i, word := range words {
		head, middle, tail := "", word, ""
		if m := urlizeHeadRe.FindString(middle); m != "" {
			head = m
			middle = middle[len(m):]
		}
		for _, suf := range []string{")", ">", ".", ",", "\n", "&gt;"} {
			if strings.HasSuffix(middle, suf) {
				if loc := urlizeTailRe.FindStringIndex(middle); loc != nil {
					tail = middle[loc[0]:]
					middle = middle[:loc[0]]
				}
				break
			}
		}
		for _, pair := range [][2]string{{"(", ")"}, {"<", ">"}, {"&lt;", "&gt;"}} {
			startChar, endChar := pair[0], pair[1]
			startCount := strings.Count(middle, startChar)
			if startCount <= strings.Count(middle, endChar) {
				continue
			}
			for n := min(startCount, strings.Count(tail, endChar)); n > 0; n-- {
				end := strings.Index(tail, endChar) + len(endChar)
				middle += tail[:end]
				tail = tail[end:]
			}
		}
		switch {
		case urlizeHTTPRe.MatchString(middle):
			if strings.HasPrefix(middle, "https://") || strings.HasPrefix(middle, "http://") {
				middle = fmt.Sprintf(`<a href="%s"%s%s>%s</a>`, middle, relAttr, targetAttr, trimURL(middle))
			} else {
				middle = fmt.Sprintf(`<a href="https://%s"%s%s>%s</a>`, middle, relAttr, targetAttr, trimURL(middle))
			}
		case strings.HasPrefix(middle, "mailto:") && urlizeEmailRe.MatchString(middle[7:]):
			middle = fmt.Sprintf(`<a href="%s">%s</a>`, middle, middle[7:])
		case strings.Contains(middle, "@") && !strings.HasPrefix(middle, "www.") && !strings.HasPrefix(middle, "@") &&
			!strings.Contains(middle, ":") && urlizeEmailRe.MatchString(middle):
			middle = fmt.Sprintf(`<a href="mailto:%s">%s</a>`, middle, middle)
		case hasSchemes:
			for _, scheme := range schemes {
				if middle != scheme && strings.HasPrefix(middle, scheme) {
					middle = fmt.Sprintf(`<a href="%s"%s%s>%s</a>`, middle, relAttr, targetAttr, middle)
				}
			}
		}
		words[i] = head + middle + tail
	}
	return strings.Join(words, "")
}

// markupStriptags is markupsafe's Markup.striptags: comments, then tags
// removed, whitespace collapsed, entities unescaped.
func markupStriptags(value string) string {
	for {
		start := strings.Index(value, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(value[start:], "-->")
		if end < 0 {
			break
		}
		value = value[:start] + value[start+end+3:]
	}
	for {
		start := strings.Index(value, "<")
		if start < 0 {
			break
		}
		end := strings.Index(value[start:], ">")
		if end < 0 {
			break
		}
		value = value[:start] + value[start+end+1:]
	}
	value = strings.Join(pyStrSplit(value), " ")
	return html.UnescapeString(value)
}

// pyStrSplit is str.split() with no separator: runs of whitespace.
func pyStrSplit(s string) []string {
	return strings.FieldsFunc(s, isPySpace)
}
