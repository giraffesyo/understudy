package template

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerExtraTests installs the rest of Jinja2's and ansible-core's
// builtin tests: the operator aliases, the remaining path tests,
// isnan, issubset/issuperset, lower/upper, escaped, filter/test, the
// async and reachability result tests, uri/url/urn and the vault tests.
func registerExtraTests(e *Engine) {
	t := e.Tests

	// Jinja's operator tests ("==" and so on), usable with select.
	for name, op := range map[string]string{"==": "==", "!=": "!=", "<": "<", "<=": "<=", ">": ">", ">=": ">="} {
		t[name] = cmpTest(op)
	}

	// Aliases of tests implemented elsewhere.
	t["is_dir"] = t["directory"]
	t["is_file"] = t["file"]
	t["is_link"] = t["link"]
	t["change"] = t["changed"]
	t["successful"] = t["success"]
	t["issubset"] = t["subset"]
	t["issuperset"] = t["superset"]

	// os.path.isabs.
	isabs := func(ec *EvalCtx, in any, args []any) (bool, error) {
		p, err := fspath(in)
		if err != nil {
			return false, err
		}
		return strings.HasPrefix(p, "/"), nil
	}
	t["abs"] = isabs
	t["is_abs"] = isabs

	// os.path.lexists.
	t["link_exists"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		p, err := fspath(in)
		if err != nil {
			return false, err
		}
		_, err = os.Lstat(p)
		return err == nil, nil
	}

	// os.path.ismount.
	ismount := func(ec *EvalCtx, in any, args []any) (bool, error) {
		p, err := fspath(in)
		if err != nil {
			return false, err
		}
		return pyIsmount(p), nil
	}
	t["mount"] = ismount
	t["is_mount"] = ismount

	// os.path.samefile(f1, f2).
	samefile := func(ec *EvalCtx, in any, args []any) (bool, error) {
		other, _ := testArg(ec, args, 0, "f2")
		var infos [2]os.FileInfo
		for i, v := range []any{in, other} {
			p, err := fspath(v)
			if err != nil {
				return false, err
			}
			info, err := os.Stat(p)
			if err != nil {
				return false, pyOSError(err, p)
			}
			infos[i] = info
		}
		return os.SameFile(infos[0], infos[1]), nil
	}
	t["same_file"] = samefile
	t["is_same_file"] = samefile

	// isnotanumber: math.isnan(x), False for what is not a real number.
	isnan := func(ec *EvalCtx, in any, args []any) (bool, error) {
		f, ok := Undeprecate(in).(float64)
		return ok && math.IsNaN(f), nil
	}
	t["nan"] = isnan
	t["isnan"] = isnan

	// test_lower, test_upper: str(value).islower(), .isupper().
	t["lower"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pyCasedAll(htmlOf(in), unicode.IsLower, unicode.IsUpper), nil
	}
	t["upper"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pyCasedAll(htmlOf(in), unicode.IsUpper, unicode.IsLower), nil
	}

	// test_escaped: hasattr(value, "__html__").
	t["escaped"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return isMarkup(in), nil
	}

	// test_filter, test_test: value in env.filters / env.tests.
	t["filter"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pluginKnown(ec.engine.Filters, in)
	}
	t["test"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return pluginKnown(ec.engine.Tests, in)
	}

	resultDict := func(name string, in any) (map[string]any, error) {
		m, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("The '%s' test expects a dictionary", name)
		}
		return m, nil
	}
	t["unreachable"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("unreachable", in)
		if err != nil {
			return false, err
		}
		return truthy(m["unreachable"]), nil
	}
	t["reachable"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("unreachable", in)
		if err != nil {
			return false, err
		}
		return !truthy(m["unreachable"]), nil
	}
	// timedout: result.get('timedout') and result['timedout'].get('period').
	t["timedout"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		m, err := resultDict("timedout", in)
		if err != nil {
			return false, err
		}
		to := m["timedout"]
		if !truthy(to) {
			return false, nil
		}
		tm, ok := anyToMap(to)
		if !ok {
			return false, fmt.Errorf("'%s' object has no attribute 'get'", pyClassName(to, ec.fromVar(-1)))
		}
		return truthy(tm["period"]), nil
	}
	asyncTest := func(name string) TestFunc {
		return func(ec *EvalCtx, in any, args []any) (bool, error) {
			m, err := resultDict(name, in)
			if err != nil {
				return false, err
			}
			if v, ok := m[name]; ok {
				return truthy(v), nil
			}
			if ec.engine.Warning != nil {
				ec.engine.Warning(Position{}, fmt.Sprintf("The '%s' test expects an async task, but a non-async task was tested", name))
			}
			return true, nil
		}
	}
	t["finished"] = asyncTest("finished")
	t["started"] = asyncTest("started")

	// is_uri(value, schemes=None), is_url, is_urn.
	t["uri"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		schemes, _ := testArg(ec, args, 0, "schemes")
		return isURI(in, schemes)
	}
	t["url"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		schemes, _ := testArg(ec, args, 0, "schemes")
		ok, err := isURI(in, schemes)
		if !ok || err != nil {
			return ok, err
		}
		scheme, netloc, _, _, _, _ := pyURLSplitParts(htmlOf(in))
		return netloc != "" || scheme == "file", nil
	}
	t["urn"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		return isURI(in, []any{"urn"})
	}

	// vault_encrypted: a vault-encrypted value (a !vault variable).
	t["vault_encrypted"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		_, ok := Undeprecate(in).(yaml.VaultedString)
		return ok, nil
	}
	// vaulted_file: whether the file starts with the vault header.
	t["vaulted_file"] = func(ec *EvalCtx, in any, args []any) (bool, error) {
		p, ok := asString(Undeprecate(in))
		if !ok {
			p = toStr(in)
		}
		cannot := func(err error) error {
			return pyRaiseFrom(fmt.Sprintf("Cannot test if the file %s is a vault.", pyRepr(in)), pyOSErrorBytes(err, p).Error())
		}
		f, err := os.Open(p)
		if err != nil {
			return false, cannot(err)
		}
		defer f.Close()
		head := make([]byte, len("$ANSIBLE_VAULT"))
		n, err := io.ReadFull(f, head)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return false, cannot(err)
		}
		return string(head[:n]) == "$ANSIBLE_VAULT", nil
	}
}

// testArg is a test's positional argument i or keyword name.
func testArg(ec *EvalCtx, args []any, i int, name string) (any, bool) {
	if i < len(args) {
		return args[i], true
	}
	v, ok := ec.testKwargs[name]
	return v, ok
}

// pyCasedAll is str.islower (is, isNot: unicode.IsLower, IsUpper) or
// str.isupper: a cased character, and none of the other case.
func pyCasedAll(s string, is, isNot func(rune) bool) bool {
	cased := false
	for _, r := range s {
		if isNot(r) || unicode.IsTitle(r) {
			return false
		}
		if is(r) {
			cased = true
		}
	}
	return cased
}

// pluginKnown is `name in env.filters` (or tests): a plugin's short or
// ansible.builtin/ansible.legacy name.
func pluginKnown[F any](registry map[string]F, v any) (bool, error) {
	s, ok := asString(Undeprecate(v))
	if !ok {
		if !pyHashable(v) {
			cls := pyOperandClass(v, true)
			return false, fmt.Errorf("cannot use '%s' as a dict key (unhashable type: '%s')", lazyQualname(cls), cls)
		}
		// Loading a plugin by a name that is not a string fails, which
		// counts as found.
		return true, nil
	}
	for _, prefix := range []string{"ansible.builtin.", "ansible.legacy."} {
		s = strings.TrimPrefix(s, prefix)
	}
	_, found := registry[s]
	return found, nil
}

// isURI is is_uri(value, schemes): urlparse succeeds, and the scheme is
// one of schemes when they are given.
func isURI(in any, schemes any) (bool, error) {
	s, ok := asString(Undeprecate(in))
	if !ok {
		return false, nil
	}
	scheme, _, _, _, _, err := pyURLSplitParts(s)
	if err != nil {
		return false, nil
	}
	if !truthy(schemes) {
		return true, nil
	}
	return contains(scheme, Undeprecate(schemes))
}

// pyIsmount is posixpath.ismount.
func pyIsmount(p string) bool {
	s1, err := os.Lstat(p)
	if err != nil || s1.Mode()&os.ModeSymlink != 0 {
		return false
	}
	parent := filepath.Join(p, "..")
	s2, err := os.Lstat(parent)
	if err != nil {
		return false
	}
	st1, ok1 := s1.Sys().(*syscall.Stat_t)
	st2, ok2 := s2.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return st1.Dev != st2.Dev || st1.Ino == st2.Ino
}

// pyOSError is the OSError Python raises for a failed stat of path.
func pyOSError(err error, path string) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("[Errno %d] %s: %s", int(errno), pyStrerror(errno), pyStrRepr(path))
	}
	return err
}

// pyOSErrorBytes is pyOSError for a path given as bytes.
func pyOSErrorBytes(err error, path string) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("[Errno %d] %s: b%s", int(errno), pyStrerror(errno), pyStrRepr(path))
	}
	return err
}

// pyStrerror is strerror(errno) as Python words it.
func pyStrerror(e syscall.Errno) string {
	switch e {
	case syscall.ENOENT:
		return "No such file or directory"
	case syscall.EACCES:
		return "Permission denied"
	case syscall.ENOTDIR:
		return "Not a directory"
	case syscall.EISDIR:
		return "Is a directory"
	}
	s := e.Error()
	return strings.ToUpper(s[:1]) + s[1:]
}

// RawVarGetter is a VarGetter that also gives a variable's value as
// defined, before templating.
type RawVarGetter interface {
	RawVar(name string) (any, bool)
}

// rawValue is the value as defined of a variable reference (a name, and
// its attributes and constant items), before templating.
func (ec *EvalCtx) rawValue(e Expr) (any, bool) {
	switch t := e.(type) {
	case *nameExpr:
		for s := ec; s != nil; s = s.parent {
			if _, ok := s.locals[t.name]; ok {
				return nil, false
			}
		}
		if rg, ok := ec.vars.(RawVarGetter); ok {
			return rg.RawVar(t.name)
		}
	case *getAttrExpr:
		if parent, ok := ec.rawValue(t.x); ok {
			return rawChild(parent, t.name)
		}
	case *getItemExpr:
		lit, isLit := t.index.(*literalExpr)
		if !isLit {
			return nil, false
		}
		if parent, ok := ec.rawValue(t.x); ok {
			if k, isStr := asString(lit.val); isStr {
				return rawChild(parent, k)
			}
			if i, isInt := asInt(lit.val); isInt {
				if l, isList := parent.([]any); isList && i >= 0 && i < int64(len(l)) {
					return l[i], true
				}
			}
		}
	}
	return nil, false
}

func rawChild(parent any, key string) (any, bool) {
	switch p := parent.(type) {
	case map[string]any:
		v, ok := p[key]
		return v, ok
	case *yaml.OMap:
		return p.GetItem(key)
	}
	return nil, false
}
