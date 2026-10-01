package template

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
)

// registerAnsibleTests installs match/search/version and the task-result
// tests (success/failed/changed/skipped) plus path tests.
func registerAnsibleTests(e *Engine) {
	t := e.Tests

	t["match"] = regexTest(true)
	t["search"] = regexTest(false)
	t["regex"] = regexTest(false)

	version := func(ec *EvalCtx, in any, args []any) (bool, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		if len(args) < 1 {
			return false, fmt.Errorf("version test requires a version to compare against")
		}
		other, _ := asString(args[0])
		op := ">="
		if len(args) > 1 {
			op, _ = asString(args[1])
		}
		c := compareVersions(s, other)
		switch op {
		case "<", "lt":
			return c < 0, nil
		case "<=", "le":
			return c <= 0, nil
		case ">", "gt":
			return c > 0, nil
		case ">=", "ge":
			return c >= 0, nil
		case "==", "=", "eq":
			return c == 0, nil
		case "!=", "<>", "ne":
			return c != 0, nil
		}
		return false, fmt.Errorf("unknown version comparison operator %q", op)
	}
	t["version"] = version
	t["version_compare"] = version

	// Task-result tests inspect a result dict.
	resultDict := func(name string, in any) (map[string]any, error) {
		m, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("the %q test expects a task-result dictionary, got %s", name, typeName(in))
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
		m, err := resultDict("success", in)
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

func regexTest(anchored bool) TestFunc {
	return func(ec *EvalCtx, in any, args []any) (bool, error) {
		s, ok := asString(in)
		if !ok {
			return false, fmt.Errorf("regex tests require a string, got %s", typeName(in))
		}
		if len(args) < 1 {
			return false, fmt.Errorf("regex test requires a pattern")
		}
		pattern, _ := asString(args[0])
		ignorecase := len(args) > 1 && truthy(args[1])
		if msg := pyre.SyntaxError(pattern); msg != "" {
			return false, errors.New(msg) // re.error
		}
		if anchored {
			// Python re.match anchors at the start only.
			pattern = `\A(?:` + pattern + `)`
		}
		re, err := pyRegexCompile(pattern, ignorecase, false)
		if err != nil {
			return false, err
		}
		return re.MatchString(s), nil
	}
}

// compareVersions compares dotted version strings: numeric segments compare
// numerically, non-numeric lexically, missing segments count as zero.
func compareVersions(a, b string) int {
	as := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' })
	bs := strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' })
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := "0", "0"
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aNum := strconv.Atoi(av)
		bi, bNum := strconv.Atoi(bv)
		switch {
		case aNum == nil && bNum == nil:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case aNum == nil:
			return 1 // numeric > alpha (1.0 > 1.rc1, matching loose semantics)
		case bNum == nil:
			return -1
		default:
			if c := strings.Compare(av, bv); c != 0 {
				return c
			}
		}
	}
	return 0
}

func listContainsAll(container []any, needles any) (bool, error) {
	if len(container) != 1 {
		return false, fmt.Errorf("subset requires one argument")
	}
	big, ok := container[0].([]any)
	if !ok {
		return false, fmt.Errorf("subset/superset require lists")
	}
	small, ok := needles.([]any)
	if !ok {
		return false, fmt.Errorf("subset/superset require lists")
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
