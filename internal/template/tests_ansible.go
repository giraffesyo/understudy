package template

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// registerAnsibleTests installs match/search/version and the task-result
// tests (success/failed/changed/skipped) plus path tests.
func registerAnsibleTests(e *Engine) {
	t := e.Tests

	t["match"] = regexTest("match")
	t["search"] = regexTest("search")
	t["regex"] = regexTest("")

	// version_compare(value, version, operator='eq', strict=None,
	// version_type=None)
	version := func(ec *EvalCtx, in any, args []any) (bool, error) {
		param := func(i int, name string) any {
			if i < len(args) {
				return args[i]
			}
			return ec.testKwargs[name]
		}
		ver, op, strict, vtype := param(0, "version"), param(1, "operator"), param(2, "strict"), param(3, "version_type")
		if op == nil {
			op = "eq"
		}
		if strict != nil && vtype != nil {
			return false, fmt.Errorf("Cannot specify both 'strict' and 'version_type'")
		}
		if !truthy(in) {
			return false, fmt.Errorf("Input version value cannot be empty")
		}
		if !truthy(ver) {
			return false, fmt.Errorf("Version parameter to compare against cannot be empty")
		}
		kind := "loose"
		if truthy(strict) {
			kind = "strict"
		} else if vtype != nil && truthy(vtype) {
			switch t := toStr(vtype); t {
			case "loose", "strict", "semver", "semantic", "pep440":
				kind = t
			default:
				return false, fmt.Errorf("Invalid version type (%s). Must be one of 'loose', 'strict', 'semver', 'semantic', 'pep440'", t)
			}
		}
		ops := map[string]string{"==": "eq", "=": "eq", "eq": "eq", "<": "lt", "lt": "lt", "<=": "le", "le": "le",
			">": "gt", "gt": "gt", ">=": "ge", "ge": "ge", "!=": "ne", "<>": "ne", "ne": "ne"}
		opName, ok := ops[toStr(op)]
		if !ok {
			return false, fmt.Errorf("Invalid operator type (%s). Must be one of '==', '=', 'eq', '<', 'lt', '<=', 'le', '>', 'gt', '>=', 'ge', '!=', '<>', 'ne'", toStr(op))
		}
		c, err := compareVersionsAs(kind, toStr(in), toStr(ver), opName)
		if err != nil {
			return false, whileHandling("Version comparison failed: %s", err)
		}
		switch opName {
		case "lt":
			return c < 0, nil
		case "le":
			return c <= 0, nil
		case "gt":
			return c > 0, nil
		case "ge":
			return c >= 0, nil
		case "eq":
			return c == 0, nil
		}
		return c != 0, nil
	}
	t["version"] = version
	t["version_compare"] = version

	// Task-result tests inspect a result dict.
	resultDict := func(name string, in any) (map[string]any, error) {
		m, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("The '%s' test expects a dictionary", name)
		}
		return m, nil
	}
	failedTest := func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("failed", in)
		if err != nil {
			return false, err
		}
		return truthy(m["failed"]), nil
	}
	t["failed"] = failedTest
	t["failure"] = failedTest
	succTest := func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("failed", in) // success is not failed()
		if err != nil {
			return false, err
		}
		return !truthy(m["failed"]), nil
	}
	t["success"] = succTest
	t["succeeded"] = succTest
	t["changed"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("changed", in)
		if err != nil {
			return false, err
		}
		if results, ok := m["results"].([]any); ok && m["changed"] == nil {
			for _, r := range results {
				if rm, ok := anyToMap(r); ok && truthy(rm["changed"]) {
					return true, nil
				}
			}
			return false, nil
		}
		return truthy(m["changed"]), nil
	}
	skippedTest := func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("skipped", in)
		if err != nil {
			return false, err
		}
		return truthy(m["skipped"]), nil
	}
	t["skipped"] = skippedTest
	t["skip"] = skippedTest

	// Subset/superset on lists.
	t["subset"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return listContainsAll(args, in)
	}
	t["superset"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("superset requires one argument")
		}
		return listContainsAll([]any{in}, args[0])
	}
	t["contains"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		if len(args) != 1 {
			return false, fmt.Errorf("contains requires one argument")
		}
		return contains(args[0], in)
	}
	t["any"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		items, err := iterate(in)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if truthy(item) {
				return true, nil
			}
		}
		return false, nil
	}
	t["all"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		items, err := iterate(in)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if !truthy(item) {
				return false, nil
			}
		}
		return true, nil
	}

	// Control-node path tests.
	pathTest := func(check func(os.FileInfo) bool) TestFunc {
		return func(ec *EvalCtx, in any, args []any) (bool, error) {
			s, ok := asString(in)
			if !ok {
				return false, fmt.Errorf("path tests require a string")
			}
			info, err := os.Stat(s)
			if err != nil {
				return false, nil
			}
			return check(info), nil
		}
	}
	t["exists"] = pathTest(func(os.FileInfo) bool { return true })
	t["directory"] = pathTest(func(i os.FileInfo) bool { return i.IsDir() })
	t["file"] = pathTest(func(i os.FileInfo) bool { return i.Mode().IsRegular() })
	t["link"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		s, ok := asString(in)
		if !ok {
			return false, fmt.Errorf("path tests require a string")
		}
		info, err := os.Lstat(s)
		if err != nil {
			return false, nil
		}
		return info.Mode()&os.ModeSymlink != 0, nil
	}
}

// regexTest is ansible's regex(value, pattern=”, ignorecase=False,
// multiline=False, match_type) test: re.compile(pattern, flags) and its
// search, match or fullmatch (match_type is regex's own argument).
func regexTest(matchType string) TestFunc {
	return func(ec *EvalCtx, in any, args []any) (bool, error) {
		arg := func(i int, name string) (any, bool) {
			return filterArg(args, i, ec.testKwargs, name)
		}
		mt := matchType
		if mt == "" {
			mt = "search"
			if v, ok := arg(3, "match_type"); ok {
				mt, _ = asString(Undeprecate(v))
				switch mt {
				case "search", "match", "fullmatch":
				default:
					return false, errors.New("Invalid match_type specified. Expected one of: search, match, fullmatch.")
				}
			}
		}
		s := regexSubject(in)
		var pattern any = ""
		if v, ok := arg(0, "pattern"); ok {
			pattern = v
		}
		ic, _ := arg(1, "ignorecase")
		ml, _ := arg(2, "multiline")
		re, err := pyRegexCompile(pattern, truthy(ic), truthy(ml))
		if err != nil {
			return false, err
		}
		switch mt {
		case "match":
			return re.Match(s, 0, -1) != nil, nil
		case "fullmatch":
			return re.FullMatch(s, 0, -1) != nil, nil
		}
		return re.Search(s, 0, -1) != nil, nil
	}
}

// looseComponentRe is LooseVersion.component_re.
var looseComponentRe = regexp.MustCompile(`\d+|[a-z]+|\.`)

// looseVersion is LooseVersion.parse: re.split on component_re keeps
// the components and what lies between them; numbers become ints.
func looseVersion(s string) []any {
	var parts []string
	last := 0
	for _, m := range looseComponentRe.FindAllStringIndex(s, -1) {
		parts = append(parts, s[last:m[0]], s[m[0]:m[1]])
		last = m[1]
	}
	parts = append(parts, s[last:])
	var out []any
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if n, ok := pyParseInt(p, 10); ok {
			out = append(out, n)
		} else {
			out = append(out, p)
		}
	}
	return out
}

// strictVersionRe is StrictVersion.version_re.
var strictVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)(\.(\d+))?([ab](\d+))?\n?$`)

// strictVersion is StrictVersion.parse: (major, minor, patch) and the
// pre-release (nil for none).
func strictVersion(s string) ([]any, []any, error) {
	m := strictVersionRe.FindStringSubmatch(s)
	if m == nil {
		return nil, nil, fmt.Errorf("invalid version number '%s'", s)
	}
	num := func(d string) any { n, _ := pyParseInt(d, 10); return n }
	v := []any{num(m[1]), num(m[2]), int64(0)}
	if m[4] != "" {
		v[2] = num(m[4])
	}
	var pre []any
	if m[5] != "" {
		pre = []any{m[5][:1], num(m[6])}
	}
	return v, pre, nil
}

// comparePyLists is Python's list comparison: the first unequal items
// decide (an int and a str cannot be ordered), else the shorter is less.
func comparePyLists(a, b []any) (int, error) {
	for i := 0; i < len(a) && i < len(b); i++ {
		if equal(a[i], b[i]) {
			continue
		}
		return compareOp(a[i], b[i], "<")
	}
	switch {
	case len(a) < len(b):
		return -1, nil
	case len(a) > len(b):
		return 1, nil
	}
	return 0, nil
}

// compareVersionsAs compares two versions as ansible-core's version
// classes do (LooseVersion, StrictVersion; semantic and pep440 versions
// compare as loose ones).
func compareVersionsAs(kind, a, b, op string) (int, error) {
	if kind == "strict" {
		av, apre, err := strictVersion(a)
		if err != nil {
			return 0, err
		}
		bv, bpre, err := strictVersion(b)
		if err != nil {
			return 0, err
		}
		if c, _ := comparePyLists(av, bv); c != 0 {
			return c, nil
		}
		switch {
		case apre == nil && bpre == nil:
			return 0, nil
		case bpre == nil:
			return -1, nil
		case apre == nil:
			return 1, nil
		}
		return comparePyLists(apre, bpre)
	}
	av, bv := looseVersion(a), looseVersion(b)
	if len(av) == len(bv) {
		same := true
		for i := range av {
			if !equal(av[i], bv[i]) {
				same = false
				break
			}
		}
		if same {
			return 0, nil
		}
	}
	return comparePyLists(av, bv)
}

func listContainsAll(container []any, needles any) (bool, error) {
	if len(container) != 1 {
		return false, fmt.Errorf("subset requires one argument")
	}
	// set(a) <= set(b): the tested value's set is built first.
	small, err := pySetItems(needles)
	if err != nil {
		return false, err
	}
	big, err := pySetItems(container[0])
	if err != nil {
		return false, err
	}
	for _, n := range small {
		found := false
		for _, b := range big {
			if equal(b, n) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// pySetItems are the items set(v) holds, or the TypeError building it
// raises: v not iterable, or an item (lazy, as plugin arguments are)
// unhashable.
func pySetItems(v any) ([]any, error) {
	items, err := iterate(v)
	if err != nil {
		return nil, errNotIterable(v, false)
	}
	for _, it := range items {
		if !pyHashable(it) {
			cls := pyOperandClass(it, true)
			return nil, fmt.Errorf("cannot use '%s' as a set element (unhashable type: '%s')", lazyQualname(cls), cls)
		}
	}
	return items, nil
}

// lazyQualname is a lazy container class's module-qualified name.
func lazyQualname(cls string) string {
	if strings.HasPrefix(cls, "_AnsibleLazy") {
		return "ansible._internal._templating._lazy_containers." + cls
	}
	return cls
}
