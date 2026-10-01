package template

import (
	"fmt"
	"slices"
	"strings"
)

// Calling a filter or test binds its arguments to the plugin function's
// Python signature first: too many or too few, or a keyword it does not
// take, raise the TypeError CPython words ("do_upper() takes 1
// positional argument but 2 were given"), which ansible-core reports as
// the plugin's failure. pySig is that signature; the tables below are
// ansible-core 2.21's ansible.builtin filters and tests (Jinja2's and
// ansible's own), read off inspect.signature of the functions its Jinja
// environment calls.

type sigStyle int

const (
	pyFunc      sigStyle = iota // a Python function
	cExactlyOne                 // a METH_O builtin (len, abs)
	cOperator                   // an _operator function (eq, lt)
	cClinic                     // an Argument Clinic builtin (combinations, normpath)
	cMethod                     // an Argument Clinic str method (split): its self is the input
	cZipLongest                 // itertools.zip_longest, which names no keyword
	cKeywords                   // a builtin taking *args and keyword-only arguments (zip, product)
)

type pySig struct {
	style    sigStyle
	name     string   // the function's __qualname__
	params   []string // positional parameters, the hidden Jinja one first
	posOnly  int      // leading positional-only parameters
	required int      // leading parameters without a default
	kwOnly   []string // keyword-only parameters (all with defaults)
	varargs  bool     // *args
	varkw    bool     // **kwargs
	hidden   int      // leading parameters Jinja passes (environment, context, eval_ctx)
}

// bindArgs is the TypeError binding a call's arguments to sig raises, or
// nil: npos positional arguments (the input included), keywords in call
// order.
func (s *pySig) bindArgs(npos int, kws []string) error {
	npos += s.hidden
	switch s.style {
	case cExactlyOne:
		if len(kws) > 0 {
			return fmt.Errorf("%s() takes no keyword arguments", s.name)
		}
		if npos != 1 {
			return fmt.Errorf("%s() takes exactly one argument (%d given)", s.name, npos)
		}
		return nil
	case cOperator:
		if len(kws) > 0 {
			return fmt.Errorf("_operator.%s() takes no keyword arguments", s.name)
		}
		if npos != 2 {
			return fmt.Errorf("%s expected 2 arguments, got %d", s.name, npos)
		}
		return nil
	case cMethod:
		return s.bindClinic(npos-1, kws)
	case cClinic:
		return s.bindClinic(npos, kws)
	case cZipLongest:
		if len(kws) > 0 && (len(kws) != 1 || kws[0] != "fillvalue") {
			return fmt.Errorf("zip_longest() got an unexpected keyword argument")
		}
		return nil
	case cKeywords:
		if len(kws) > len(s.kwOnly) {
			return fmt.Errorf("%s() takes at most %d keyword argument%s (%d given)", s.name, len(s.kwOnly), pluralS(len(s.kwOnly) != 1), len(kws))
		}
		for _, k := range kws {
			if !slices.Contains(s.kwOnly, k) {
				return fmt.Errorf("%s() got an unexpected keyword argument '%s'", s.name, k)
			}
		}
		return nil
	}
	return s.bindPython(npos, kws)
}

// bindPython is CPython's initialize_locals: positional arguments bind
// first, then keywords (an unknown one, one already bound), then the
// count of positional arguments and the required ones missing are
// checked.
func (s *pySig) bindPython(npos int, kws []string) error {
	n := len(s.params)
	bound := make([]bool, n)
	for i := 0; i < min(npos, n); i++ {
		bound[i] = true
	}
	kwOnlyGiven := 0
	for _, k := range kws {
		j := -1
		for i := s.posOnly; i < n; i++ {
			if s.params[i] == k {
				j = i
				break
			}
		}
		if j < 0 {
			isKwOnly := false
			for _, ko := range s.kwOnly {
				if ko == k {
					isKwOnly = true
				}
			}
			if isKwOnly {
				kwOnlyGiven++
				continue
			}
			if s.varkw {
				continue
			}
			var posOnly []string
			for _, kk := range kws {
				for i := 0; i < s.posOnly; i++ {
					if s.params[i] == kk {
						posOnly = append(posOnly, kk)
					}
				}
			}
			if len(posOnly) > 0 {
				return fmt.Errorf("%s() got some positional-only arguments passed as keyword arguments: '%s'", s.name, strings.Join(posOnly, ", "))
			}
			msg := fmt.Sprintf("%s() got an unexpected keyword argument '%s'", s.name, k)
			if sugg := pySuggestion(append(append([]string{}, s.params...), s.kwOnly...), k); sugg != "" {
				msg += fmt.Sprintf(". Did you mean '%s'?", sugg)
			}
			return fmt.Errorf("%s", msg)
		}
		if bound[j] {
			return fmt.Errorf("%s() got multiple values for argument '%s'", s.name, k)
		}
		bound[j] = true
	}
	if npos > n && !s.varargs {
		var sig string
		plural := true
		if s.required < n {
			sig = fmt.Sprintf("from %d to %d", s.required, n)
		} else {
			sig = fmt.Sprintf("%d", n)
			plural = n != 1
		}
		kwSig := ""
		if kwOnlyGiven > 0 {
			kwSig = fmt.Sprintf(" positional argument%s (and %d keyword-only argument%s)", pluralS(npos != 1), kwOnlyGiven, pluralS(kwOnlyGiven != 1))
		}
		verb := "were"
		if npos == 1 && kwOnlyGiven == 0 {
			verb = "was"
		}
		return fmt.Errorf("%s() takes %s positional argument%s but %d%s %s given", s.name, sig, pluralS(plural), npos, kwSig, verb)
	}
	var missing []string
	for i := npos; i < s.required; i++ {
		if !bound[i] {
			missing = append(missing, "'"+s.params[i]+"'")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s() missing %d required positional argument%s: %s", s.name, len(missing), pluralS(len(missing) != 1), pyJoinNames(missing))
	}
	return nil
}

// bindClinic is Argument Clinic's _PyArg_UnpackKeywords.
func (s *pySig) bindClinic(npos int, kws []string) error {
	maxArgs := len(s.params)
	if s.style == cClinic && maxArgs == 0 {
		maxArgs = 1
	}
	if npos+len(kws) > maxArgs {
		kw := ""
		if npos == 0 {
			kw = "keyword "
		}
		return fmt.Errorf("%s() takes at most %d %sargument%s (%d given)", s.name, maxArgs, kw, pluralS(maxArgs != 1), npos+len(kws))
	}
	given := map[string]bool{}
	for _, k := range kws {
		given[k] = true
	}
	for i := npos; i < len(s.params); i++ {
		if given[s.params[i]] {
			continue
		}
		if i < s.required {
			return fmt.Errorf("%s() missing required argument '%s' (pos %d)", s.name, s.params[i], i+1)
		}
	}
	for i := 0; i < min(npos, len(s.params)); i++ {
		if given[s.params[i]] {
			return fmt.Errorf("argument for %s() given by name ('%s') and position (%d)", s.name, s.params[i], i+1)
		}
	}
	for _, k := range kws {
		known := false
		for _, p := range s.params {
			known = known || p == k
		}
		if !known {
			return fmt.Errorf("%s() got an unexpected keyword argument '%s'", s.name, k)
		}
	}
	return nil
}

func pluralS(plural bool) string {
	if plural {
		return "s"
	}
	return ""
}

// pyJoinNames is CPython's format_missing: 'a', 'a' and 'b', 'a', 'b',
// and 'c'.
func pyJoinNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

// pySuggestion is CPython's _Py_CalculateSuggestions: the candidate
// closest to name by its Levenshtein distance (a case change costs less),
// within a third of the characters involved; "" when none is.
func pySuggestion(candidates []string, name string) string {
	const moveCost = 2
	best, bestDist := "", -1
	for _, c := range candidates {
		if c == name {
			continue
		}
		maxDist := (len(name) + len(c) + 3) * moveCost / 6
		if bestDist >= 0 {
			maxDist = min(maxDist, bestDist-1)
		}
		d := pyLevenshtein(name, c, maxDist)
		if d > maxDist {
			continue
		}
		if best == "" || d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func pyLevenshtein(a, b string, maxCost int) int {
	const moveCost, caseCost, maxSize = 2, 1, 40
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	if len(a) == 0 || len(b) == 0 {
		return (len(a) + len(b)) * moveCost
	}
	if len(a) > maxSize || len(b) > maxSize {
		return maxCost + 1
	}
	if len(b) < len(a) {
		a, b = b, a
	}
	if (len(b)-len(a))*moveCost > maxCost {
		return maxCost + 1
	}
	subst := func(x, y byte) int {
		if x&31 != y&31 {
			return moveCost
		}
		if x == y {
			return 0
		}
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x == y {
			return caseCost
		}
		return moveCost
	}
	buf := make([]int, len(a))
	for i := range buf {
		buf[i] = (i + 1) * moveCost
	}
	result := 0
	for bi := 0; bi < len(b); bi++ {
		code := b[bi]
		distance := bi * moveCost
		result = distance
		minimum := -1
		for i := 0; i < len(a); i++ {
			substitute := distance + subst(code, a[i])
			distance = buf[i]
			insertDelete := min(result, distance) + moveCost
			result = min(insertDelete, substitute)
			buf[i] = result
			if minimum < 0 || result < minimum {
				minimum = result
			}
		}
		if minimum > maxCost {
			return maxCost + 1
		}
	}
	return result
}

// builtinPlugin reports whether a filter or test written as full is one
// of ansible-core's own (the signatures above are theirs).
func builtinPlugin(full string) bool {
	return !strings.Contains(full, ".") || strings.HasPrefix(full, "ansible.builtin.") || strings.HasPrefix(full, "ansible.legacy.")
}

// checkArity is the TypeError calling the filter or test (a test when
// isTest) with these arguments raises, nil when they bind.
func checkArity(isTest bool, name, full string, nargs int, kwargs []kwarg) error {
	if !builtinPlugin(full) {
		return nil
	}
	table := filterSignatures
	if isTest {
		table = testSignatures
	}
	sig, ok := table[name]
	if !ok {
		return nil
	}
	kws := make([]string, len(kwargs))
	for i, k := range kwargs {
		kws[i] = k.name
	}
	return sig.bindArgs(nargs+1, kws)
}

var filterSignatures = map[string]pySig{
	"abs":                  {style: cExactlyOne, name: "abs"},
	"attr":                 {name: "do_attr", params: []string{"environment", "obj", "name"}, required: 3, hidden: 1},
	"b64decode":            {name: "b64decode", params: []string{"string", "encoding", "urlsafe"}, required: 1},
	"b64encode":            {name: "b64encode", params: []string{"string", "encoding", "urlsafe"}, required: 1},
	"basename":             {name: "basename", params: []string{"p"}, required: 1},
	"batch":                {name: "do_batch", params: []string{"value", "linecount", "fill_with"}, required: 2},
	"bool":                 {name: "to_bool", params: []string{"value"}, required: 1},
	"capitalize":           {name: "do_capitalize", params: []string{"s"}, required: 1},
	"center":               {name: "do_center", params: []string{"value", "width"}, required: 1},
	"checksum":             {name: "secure_hash_s", params: []string{"data", "hash_func"}, required: 1},
	"combinations":         {style: cClinic, name: "combinations", params: []string{"iterable", "r"}, required: 2},
	"combine":              {name: "combine", varargs: true, varkw: true},
	"comment":              {name: "comment", params: []string{"text", "style"}, required: 1, varkw: true},
	"commonpath":           {name: "commonpath", params: []string{"paths"}, required: 1},
	"count":                {style: cExactlyOne, name: "len"},
	"d":                    {name: "ansible_default", params: []string{"value", "default_value", "boolean"}, required: 1},
	"default":              {name: "ansible_default", params: []string{"value", "default_value", "boolean"}, required: 1},
	"dict2items":           {name: "dict_to_list_of_dict_key_value_elements", params: []string{"mydict", "key_name", "value_name"}, required: 1},
	"dictsort":             {name: "do_dictsort", params: []string{"value", "case_sensitive", "by", "reverse"}, required: 1},
	"difference":           {name: "difference", params: []string{"environment", "a", "b"}, required: 3, hidden: 1},
	"dirname":              {name: "dirname", params: []string{"p"}, required: 1},
	"e":                    {name: "escape", params: []string{"s"}, posOnly: 1, required: 1},
	"escape":               {name: "escape", params: []string{"s"}, posOnly: 1, required: 1},
	"expanduser":           {name: "expanduser", params: []string{"path"}, required: 1},
	"expandvars":           {name: "expandvars", params: []string{"path"}, required: 1},
	"extract":              {name: "extract", params: []string{"environment", "item", "container", "morekeys"}, required: 3, hidden: 1},
	"fileglob":             {name: "fileglob", params: []string{"pathname"}, required: 1},
	"filesizeformat":       {name: "do_filesizeformat", params: []string{"value", "binary"}, required: 1},
	"first":                {name: "sync_do_first", params: []string{"environment", "seq"}, required: 2, hidden: 1},
	"flatten":              {name: "flatten", params: []string{"mylist", "levels", "skip_nulls"}, required: 1},
	"float":                {name: "do_float", params: []string{"value", "default"}, required: 1},
	"forceescape":          {name: "do_forceescape", params: []string{"value"}, required: 1},
	"format":               {name: "do_format", params: []string{"value"}, required: 1, varargs: true, varkw: true},
	"from_json":            {name: "from_json", params: []string{"a", "profile"}, required: 1, varkw: true},
	"from_yaml":            {name: "from_yaml", params: []string{"data"}, required: 1},
	"from_yaml_all":        {name: "from_yaml_all", params: []string{"data"}, required: 1},
	"groupby":              {name: "sync_do_groupby", params: []string{"environment", "value", "attribute", "default", "case_sensitive"}, required: 3, hidden: 1},
	"hash":                 {name: "get_hash", params: []string{"data", "hashtype"}, required: 1},
	"human_readable":       {name: "human_readable", params: []string{"size", "isbits", "unit"}, required: 1},
	"human_to_bytes":       {name: "human_to_bytes", params: []string{"size", "default_unit", "isbits"}, required: 1},
	"indent":               {name: "do_indent", params: []string{"s", "width", "first", "blank"}, required: 1},
	"int":                  {name: "do_int", params: []string{"value", "default", "base"}, required: 1},
	"intersect":            {name: "intersect", params: []string{"environment", "a", "b"}, required: 3, hidden: 1},
	"items":                {name: "do_items", params: []string{"value"}, required: 1},
	"items2dict":           {name: "list_of_dict_key_value_elements_to_dict", params: []string{"mylist", "key_name", "value_name"}, required: 1},
	"join":                 {name: "sync_do_join", params: []string{"eval_ctx", "value", "d", "attribute"}, required: 2, hidden: 1},
	"last":                 {name: "do_last", params: []string{"environment", "seq"}, required: 2, hidden: 1},
	"length":               {style: cExactlyOne, name: "len"},
	"list":                 {name: "sync_do_list", params: []string{"value"}, required: 1},
	"log":                  {name: "logarithm", params: []string{"x", "base"}, required: 1},
	"lower":                {name: "do_lower", params: []string{"s"}, required: 1},
	"mandatory":            {name: "mandatory", params: []string{"a", "msg"}, required: 1},
	"map":                  {name: "sync_do_map", params: []string{"context", "value"}, required: 2, varargs: true, varkw: true, hidden: 1},
	"max":                  {name: "do_max", params: []string{"environment", "value", "case_sensitive", "attribute"}, required: 2, hidden: 1},
	"md5":                  {name: "md5s", params: []string{"data"}, required: 1},
	"min":                  {name: "do_min", params: []string{"environment", "value", "case_sensitive", "attribute"}, required: 2, hidden: 1},
	"normpath":             {style: cClinic, name: "_path_normpath", params: []string{"path"}, required: 1},
	"password_hash":        {name: "get_encrypted_password", params: []string{"password", "hashtype", "salt", "salt_size", "rounds", "ident"}, required: 1},
	"path_join":            {name: "path_join", params: []string{"paths"}, required: 1},
	"permutations":         {style: cClinic, name: "permutations", params: []string{"iterable", "r"}, required: 1},
	"pow":                  {name: "power", params: []string{"x", "y"}, required: 2},
	"pprint":               {name: "do_pprint", params: []string{"value"}, required: 1},
	"product":              {style: cKeywords, name: "product", kwOnly: []string{"repeat"}},
	"quote":                {name: "quote", params: []string{"a"}, required: 1},
	"random":               {name: "rand", params: []string{"environment", "end", "start", "step", "seed"}, required: 2, hidden: 1},
	"realpath":             {name: "realpath", params: []string{"filename"}, required: 1, kwOnly: []string{"strict"}},
	"regex_escape":         {name: "regex_escape", params: []string{"string", "re_type"}, required: 1},
	"regex_findall":        {name: "regex_findall", params: []string{"value", "regex", "multiline", "ignorecase"}, required: 2},
	"regex_replace":        {name: "regex_replace", params: []string{"value", "pattern", "replacement", "ignorecase", "multiline", "count", "mandatory_count"}},
	"regex_search":         {name: "regex_search", params: []string{"value", "regex"}, required: 2, varargs: true, varkw: true},
	"reject":               {name: "sync_do_reject", params: []string{"context", "value"}, required: 2, varargs: true, varkw: true, hidden: 1},
	"rejectattr":           {name: "sync_do_rejectattr", params: []string{"context", "value"}, required: 2, varargs: true, varkw: true, hidden: 1},
	"rekey_on_member":      {name: "rekey_on_member", params: []string{"data", "key", "duplicates"}, required: 2},
	"relpath":              {name: "relpath", params: []string{"path", "start"}, required: 1},
	"replace":              {name: "do_replace", params: []string{"eval_ctx", "s", "old", "new", "count"}, required: 4, hidden: 1},
	"reverse":              {name: "do_reverse", params: []string{"value"}, required: 1},
	"root":                 {name: "inversepower", params: []string{"x", "base"}, required: 1},
	"round":                {name: "do_round", params: []string{"value", "precision", "method"}, required: 1},
	"safe":                 {name: "do_mark_safe", params: []string{"value"}, required: 1},
	"select":               {name: "sync_do_select", params: []string{"context", "value"}, required: 2, varargs: true, varkw: true, hidden: 1},
	"selectattr":           {name: "sync_do_selectattr", params: []string{"context", "value"}, required: 2, varargs: true, varkw: true, hidden: 1},
	"sha1":                 {name: "secure_hash_s", params: []string{"data", "hash_func"}, required: 1},
	"shuffle":              {name: "randomize_list", params: []string{"mylist", "seed"}, required: 1},
	"slice":                {name: "sync_do_slice", params: []string{"value", "slices", "fill_with"}, required: 2},
	"sort":                 {name: "do_sort", params: []string{"environment", "value", "reverse", "case_sensitive", "attribute"}, required: 2, hidden: 1},
	"split":                {style: cMethod, name: "split", params: []string{"sep", "maxsplit"}},
	"splitext":             {name: "splitext", params: []string{"p"}, required: 1},
	"strftime":             {name: "strftime", params: []string{"string_format", "second", "utc"}, required: 1},
	"string":               {name: "soft_str", params: []string{"s"}, posOnly: 1, required: 1},
	"striptags":            {name: "do_striptags", params: []string{"value"}, required: 1},
	"subelements":          {name: "subelements", params: []string{"obj", "subelements", "skip_missing"}, required: 2},
	"sum":                  {name: "sync_do_sum", params: []string{"environment", "iterable", "attribute", "start"}, required: 2, hidden: 1},
	"symmetric_difference": {name: "symmetric_difference", params: []string{"environment", "a", "b"}, required: 3, hidden: 1},
	"ternary":              {name: "ternary", params: []string{"value", "true_val", "false_val", "none_val"}, required: 3},
	"title":                {name: "do_title", params: []string{"s"}, required: 1},
	"to_datetime":          {name: "to_datetime", params: []string{"string", "format"}, required: 1},
	"to_json":              {name: "to_json", params: []string{"a", "profile", "vault_to_text", "preprocess_unsafe"}, required: 1, varkw: true},
	"to_nice_json":         {name: "to_nice_json", params: []string{"a", "indent", "sort_keys"}, required: 1, varkw: true},
	"to_nice_yaml":         {name: "to_nice_yaml", params: []string{"a", "indent"}, required: 1, kwOnly: []string{"default_flow_style"}, varargs: true, varkw: true},
	"to_uuid":              {name: "to_uuid", params: []string{"string", "namespace"}, required: 1},
	"to_yaml":              {name: "to_yaml", params: []string{"a"}, required: 1, kwOnly: []string{"default_flow_style", "vault_behavior"}, varargs: true, varkw: true},
	"tojson":               {name: "do_tojson", params: []string{"eval_ctx", "value", "indent"}, required: 2, hidden: 1},
	"trim":                 {name: "do_trim", params: []string{"value", "chars"}, required: 1},
	"truncate":             {name: "do_truncate", params: []string{"env", "s", "length", "killwords", "end", "leeway"}, required: 2, hidden: 1},
	"type_debug":           {name: "type_debug", params: []string{"obj"}, required: 1},
	"union":                {name: "union", params: []string{"environment", "a", "b"}, required: 3, hidden: 1},
	"unique":               {name: "unique", params: []string{"environment", "a", "case_sensitive", "attribute"}, required: 2, hidden: 1},
	"unvault":              {name: "do_unvault", params: []string{"vault", "secret", "vault_id"}, required: 2},
	"upper":                {name: "do_upper", params: []string{"s"}, required: 1},
	"urldecode":            {name: "unquote_plus", params: []string{"string", "encoding", "errors"}, required: 1},
	"urlencode":            {name: "do_urlencode", params: []string{"value"}, required: 1},
	"urlize":               {name: "do_urlize", params: []string{"eval_ctx", "value", "trim_url_limit", "nofollow", "target", "rel", "extra_schemes"}, required: 2, hidden: 1},
	"urlsplit":             {name: "split_url", params: []string{"value", "query", "alias"}, required: 1},
	"vault":                {name: "do_vault", params: []string{"data", "secret", "salt", "vault_id", "wrap_object"}, required: 2},
	"win_basename":         {name: "basename", params: []string{"p"}, required: 1},
	"win_dirname":          {name: "dirname", params: []string{"p"}, required: 1},
	"win_splitdrive":       {name: "splitdrive", params: []string{"p"}, required: 1},
	"wordcount":            {name: "do_wordcount", params: []string{"s"}, required: 1},
	"wordwrap":             {name: "do_wordwrap", params: []string{"environment", "s", "width", "break_long_words", "wrapstring", "break_on_hyphens"}, required: 2, hidden: 1},
	"xmlattr":              {name: "do_xmlattr", params: []string{"eval_ctx", "d", "autospace"}, required: 2, hidden: 1},
	"zip":                  {style: cKeywords, name: "zip", kwOnly: []string{"strict"}},
	"zip_longest":          {style: cZipLongest, name: "zip_longest"},
}

var testSignatures = map[string]pySig{
	"!=":              {style: cOperator, name: "ne"},
	"<":               {style: cOperator, name: "lt"},
	"<=":              {style: cOperator, name: "le"},
	"==":              {style: cOperator, name: "eq"},
	">":               {style: cOperator, name: "gt"},
	">=":              {style: cOperator, name: "ge"},
	"abs":             {name: "isabs", params: []string{"s"}, required: 1},
	"all":             {style: cExactlyOne, name: "all"},
	"any":             {style: cExactlyOne, name: "any"},
	"boolean":         {name: "test_boolean", params: []string{"value"}, required: 1},
	"callable":        {style: cExactlyOne, name: "callable"},
	"change":          {name: "changed", params: []string{"result"}, required: 1},
	"changed":         {name: "changed", params: []string{"result"}, required: 1},
	"contains":        {name: "contains", params: []string{"seq", "value"}, required: 2},
	"defined":         {name: "test_defined", params: []string{"value"}, required: 1},
	"directory":       {name: "isdir", params: []string{"s"}, required: 1},
	"divisibleby":     {name: "test_divisibleby", params: []string{"value", "num"}, required: 2},
	"eq":              {style: cOperator, name: "eq"},
	"equalto":         {style: cOperator, name: "eq"},
	"escaped":         {name: "test_escaped", params: []string{"value"}, required: 1},
	"even":            {name: "test_even", params: []string{"value"}, required: 1},
	"exists":          {name: "exists", params: []string{"path"}, required: 1},
	"failed":          {name: "failed", params: []string{"result"}, required: 1},
	"failure":         {name: "failed", params: []string{"result"}, required: 1},
	"false":           {name: "test_false", params: []string{"value"}, required: 1},
	"falsy":           {name: "falsy", params: []string{"value", "convert_bool"}, required: 1},
	"file":            {name: "isfile", params: []string{"path"}, required: 1},
	"filter":          {name: "test_filter", params: []string{"env", "value"}, required: 2, hidden: 1},
	"finished":        {name: "finished", params: []string{"result"}, required: 1},
	"float":           {name: "test_float", params: []string{"value"}, required: 1},
	"ge":              {style: cOperator, name: "ge"},
	"greaterthan":     {style: cOperator, name: "gt"},
	"gt":              {style: cOperator, name: "gt"},
	"in":              {name: "test_in", params: []string{"value", "seq"}, required: 2},
	"integer":         {name: "test_integer", params: []string{"value"}, required: 1},
	"is_abs":          {name: "isabs", params: []string{"s"}, required: 1},
	"is_dir":          {name: "isdir", params: []string{"s"}, required: 1},
	"is_file":         {name: "isfile", params: []string{"path"}, required: 1},
	"is_link":         {name: "islink", params: []string{"path"}, required: 1},
	"is_mount":        {name: "ismount", params: []string{"path"}, required: 1},
	"is_same_file":    {name: "samefile", params: []string{"f1", "f2"}, required: 2},
	"isnan":           {name: "isnotanumber", params: []string{"x"}, required: 1},
	"issubset":        {name: "issubset", params: []string{"a", "b"}, required: 2},
	"issuperset":      {name: "issuperset", params: []string{"a", "b"}, required: 2},
	"iterable":        {name: "test_iterable", params: []string{"value"}, required: 1},
	"le":              {style: cOperator, name: "le"},
	"lessthan":        {style: cOperator, name: "lt"},
	"link":            {name: "islink", params: []string{"path"}, required: 1},
	"link_exists":     {name: "lexists", params: []string{"path"}, required: 1},
	"lower":           {name: "test_lower", params: []string{"value"}, required: 1},
	"lt":              {style: cOperator, name: "lt"},
	"mapping":         {name: "test_mapping", params: []string{"value"}, required: 1},
	"match":           {name: "match", params: []string{"value", "pattern", "ignorecase", "multiline"}, required: 1},
	"mount":           {name: "ismount", params: []string{"path"}, required: 1},
	"nan":             {name: "isnotanumber", params: []string{"x"}, required: 1},
	"ne":              {style: cOperator, name: "ne"},
	"none":            {name: "test_none", params: []string{"value"}, required: 1},
	"number":          {name: "test_number", params: []string{"value"}, required: 1},
	"odd":             {name: "test_odd", params: []string{"value"}, required: 1},
	"reachable":       {name: "reachable", params: []string{"result"}, required: 1},
	"regex":           {name: "regex", params: []string{"value", "pattern", "ignorecase", "multiline", "match_type"}},
	"same_file":       {name: "samefile", params: []string{"f1", "f2"}, required: 2},
	"sameas":          {name: "test_sameas", params: []string{"value", "other"}, required: 2},
	"search":          {name: "search", params: []string{"value", "pattern", "ignorecase", "multiline"}, required: 1},
	"sequence":        {name: "test_sequence", params: []string{"value"}, required: 1},
	"skip":            {name: "skipped", params: []string{"result"}, required: 1},
	"skipped":         {name: "skipped", params: []string{"result"}, required: 1},
	"started":         {name: "started", params: []string{"result"}, required: 1},
	"string":          {name: "test_string", params: []string{"value"}, required: 1},
	"subset":          {name: "issubset", params: []string{"a", "b"}, required: 2},
	"succeeded":       {name: "success", params: []string{"result"}, required: 1},
	"success":         {name: "success", params: []string{"result"}, required: 1},
	"successful":      {name: "success", params: []string{"result"}, required: 1},
	"superset":        {name: "issuperset", params: []string{"a", "b"}, required: 2},
	"test":            {name: "test_test", params: []string{"env", "value"}, required: 2, hidden: 1},
	"timedout":        {name: "timedout", params: []string{"result"}, required: 1},
	"true":            {name: "test_true", params: []string{"value"}, required: 1},
	"truthy":          {name: "truthy", params: []string{"value", "convert_bool"}, required: 1},
	"undefined":       {name: "test_undefined", params: []string{"value"}, required: 1},
	"unreachable":     {name: "unreachable", params: []string{"result"}, required: 1},
	"upper":           {name: "test_upper", params: []string{"value"}, required: 1},
	"uri":             {name: "is_uri", params: []string{"value", "schemes"}, required: 1},
	"url":             {name: "is_url", params: []string{"value", "schemes"}, required: 1},
	"urn":             {name: "is_urn", params: []string{"value"}, required: 1},
	"vault_encrypted": {name: "vault_encrypted", params: []string{"value"}, required: 1},
	"vaulted_file":    {name: "vaulted_file", params: []string{"value"}, required: 1},
	"version":         {name: "version_compare", params: []string{"value", "version", "operator", "strict", "version_type"}, required: 2},
	"version_compare": {name: "version_compare", params: []string{"value", "version", "operator", "strict", "version_type"}, required: 2},
}
