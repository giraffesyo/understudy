package template

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/modules/pyre"
	"github.com/giraffesyo/understudy/internal/vault"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerExtraFilters installs the rest of ansible-core's builtin
// filters: itertools' product, combinations, permutations and
// zip_longest, paths (normpath, relpath, commonpath, expandvars,
// fileglob, the win_ ones), subelements, rekey_on_member, to_uuid,
// to_datetime, urldecode, vault and unvault.
func registerExtraFilters(e *Engine) {
	f := e.Filters

	// itertools.product(*iterables, repeat=1).
	f["product"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		repeat := int64(1)
		if v, ok := kwargs["repeat"]; ok {
			n, err := pyIndexInt(v)
			if err != nil {
				return nil, err
			}
			if n < 0 {
				return nil, errors.New("repeat argument cannot be negative")
			}
			repeat = n
		}
		var pools [][]any
		for _, it := range append([]any{in}, args...) {
			items, err := iterate(it)
			if err != nil {
				return nil, errNotIterable(it, false)
			}
			pools = append(pools, items)
		}
		var all [][]any
		for range repeat {
			all = append(all, pools...)
		}
		result := [][]any{{}}
		for _, pool := range all {
			var next [][]any
			for _, x := range result {
				for _, y := range pool {
					next = append(next, append(append([]any(nil), x...), y))
				}
			}
			result = next
		}
		out := make([]any, len(result))
		for i, r := range result {
			out[i] = r
		}
		return out, nil
	}

	// itertools.combinations(iterable, r).
	f["combinations"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		rV, _ := filterArg(args, 0, kwargs, "r")
		pool, err := iterate(in)
		if err != nil {
			return nil, errNotIterable(in, false)
		}
		r, err := pyIndexInt(rV)
		if err != nil {
			return nil, err
		}
		if r < 0 {
			return nil, errors.New("r must be non-negative")
		}
		return combinations(pool, int(r)), nil
	}

	// itertools.permutations(iterable, r=None).
	f["permutations"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		pool, err := iterate(in)
		if err != nil {
			return nil, errNotIterable(in, false)
		}
		r := int64(len(pool))
		if rV, ok := filterArg(args, 0, kwargs, "r"); ok && Undeprecate(rV) != nil {
			if r, err = pyIndexInt(rV); err != nil {
				return nil, err
			}
			if r < 0 {
				return nil, errors.New("r must be non-negative")
			}
		}
		return permutations(pool, int(r)), nil
	}

	// itertools.zip_longest(*iterables, fillvalue=None).
	f["zip_longest"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		var lists [][]any
		longest := 0
		for _, it := range append([]any{in}, args...) {
			items, err := iterate(it)
			if err != nil {
				return nil, errNotIterable(it, false)
			}
			lists = append(lists, items)
			longest = max(longest, len(items))
		}
		fill := kwargs["fillvalue"]
		out := make([]any, longest)
		for i := range longest {
			row := make([]any, len(lists))
			for j, l := range lists {
				if i < len(l) {
					row[j] = l[i]
				} else {
					row[j] = fill
				}
			}
			out[i] = row
		}
		return out, nil
	}

	f["normpath"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		p, ok := asString(Undeprecate(in))
		if !ok {
			return nil, fmt.Errorf("_path_normpath: path should be string, bytes or os.PathLike, not %s", pyClassName(in, false))
		}
		return pyNormpath(p), nil
	}
	f["relpath"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		p, err := fspath(in)
		if err != nil {
			return nil, err
		}
		start := "."
		if v, ok := filterArg(args, 0, kwargs, "start"); ok && Undeprecate(v) != nil {
			if start, err = fspath(v); err != nil {
				return nil, err
			}
		}
		return pyRelpath(p, start)
	}
	f["expandvars"] = pathStrFilter(func(p string) (any, error) { return pyExpandvars(p), nil })
	f["win_basename"] = pathStrFilter(func(p string) (any, error) { _, tail := ntSplit(p); return tail, nil })
	f["win_dirname"] = pathStrFilter(func(p string) (any, error) { head, _ := ntSplit(p); return head, nil })
	f["win_splitdrive"] = pathStrFilter(func(p string) (any, error) {
		d, r, t := ntSplitroot(p)
		return []any{d, r + t}, nil
	})

	// commonpath(paths): os.path.commonpath of a sequence.
	f["commonpath"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		v := Undeprecate(in)
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("|commonpath expects sequence, got %s instead.", pyTypeRepr(in, ec.fromVar(-1)))
		}
		paths := make([]string, len(items))
		for i, item := range items {
			p, err := fspath(item)
			if err != nil {
				return nil, err
			}
			paths[i] = p
		}
		return pyCommonpath(paths)
	}

	// fileglob(pathname): the regular files glob.glob matches.
	f["fileglob"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		p, err := fspath(in)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, m := range pyGlob(p) {
			if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
				out = append(out, m)
			}
		}
		return out, nil
	}

	// subelements(obj, subelements, skip_missing=False).
	f["subelements"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		subV, _ := filterArg(args, 0, kwargs, "subelements")
		skipV, _ := filterArg(args, 1, kwargs, "skip_missing")
		var elements []any
		v := Undeprecate(in)
		if keys, m, ok := orderedMap(v); ok {
			for _, k := range keys {
				elements = append(elements, m[k])
			}
		} else if l, ok := v.([]any); ok {
			elements = l
		} else {
			return nil, errors.New("obj must be a list of dicts or a nested dict")
		}
		var path []any
		switch s := Undeprecate(subV).(type) {
		case []any:
			path = s
		default:
			str, ok := asString(s)
			if !ok {
				return nil, errors.New("subelements must be a list or a string")
			}
			for _, p := range strings.Split(str, ".") {
				path = append(path, p)
			}
		}
		out := []any{}
		for _, element := range elements {
			values := Undeprecate(element)
			for _, sub := range path {
				next, err := pySubscript(values, sub)
				if err != nil {
					var ke *pyKeyError
					if errors.As(err, &ke) {
						if truthy(skipV) {
							values = []any{}
							break
						}
						return nil, whileHandling("could not find %s key in iterated item %s", pyRepr(sub), pyRepr(values))
					}
					return nil, pyRaiseFrom(fmt.Sprintf("the key %s should point to a dictionary, got '%s'", toStr(sub), toStr(values)), err.Error())
				}
				values = Undeprecate(next)
			}
			l, ok := values.([]any)
			if !ok {
				return nil, fmt.Errorf("the key %s should point to a list, got %s", pyRepr(path[len(path)-1]), pyRepr(values))
			}
			for _, item := range l {
				out = append(out, []any{element, item})
			}
		}
		return out, nil
	}

	// rekey_on_member(data, key, duplicates='error').
	f["rekey_on_member"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		key, _ := filterArg(args, 0, kwargs, "key")
		dup := any("error")
		if v, ok := filterArg(args, 1, kwargs, "duplicates"); ok {
			dup = Undeprecate(v)
		}
		ds, _ := asString(dup)
		if ds != "error" && ds != "overwrite" {
			return nil, fmt.Errorf("duplicates parameter to rekey_on_member has unknown value %s", pyRepr(dup))
		}
		var items []any
		v := Undeprecate(in)
		if keys, m, ok := orderedMap(v); ok {
			for _, k := range keys {
				items = append(items, m[k])
			}
		} else if l, ok := v.([]any); ok {
			items = l
		} else {
			return nil, errors.New("Type is not a valid list, set, or dict")
		}
		out := yaml.NewOMap()
		for _, item := range items {
			_, m, ok := orderedMap(item)
			if !ok {
				return nil, errors.New("List item is not a valid dict")
			}
			ks, isStr := asString(Undeprecate(key))
			elem, found := m[ks]
			if !isStr || !found {
				return nil, &objError{msg: fmt.Sprintf("Key %s was not found.", pyRepr(key)), value: pyRepr(item)}
			}
			// Keys are strings here: another key is its str().
			k, ok := asString(Undeprecate(elem))
			if !ok {
				k = toStr(elem)
			}
			if prev, ok := out.GetItem(k); ok && truthy(prev) {
				if ds == "error" {
					return nil, fmt.Errorf("Key %s is not unique, cannot convert to dict.", pyRepr(elem))
				}
			}
			out.Set(k, item)
		}
		return out, nil
	}

	// to_uuid(string, namespace=UUID_NAMESPACE_ANSIBLE): uuid5.
	f["to_uuid"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		ns := ansibleUUIDNamespace
		if v, ok := filterArg(args, 0, kwargs, "namespace"); ok {
			v = Undeprecate(v)
			s, isStr := asString(v)
			if !isStr {
				return nil, whileHandling("Invalid value '%s' for 'namespace': '%s' object has no attribute 'replace'", toStr(v), pyClassName(v, false))
			}
			b, err := parseUUID(s)
			if err != nil {
				return nil, whileHandling("Invalid value '%s' for 'namespace': %s", s, err)
			}
			ns = b
		}
		name, ok := asString(Undeprecate(in))
		if !ok {
			name = toStr(in)
		}
		return uuid5(ns, name), nil
	}

	// urldecode is urllib.parse.unquote_plus.
	f["urldecode"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(Undeprecate(in))
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'replace'", pyClassName(in, ec.fromVar(-1)))
		}
		return pyUnquotePlus(s), nil
	}

	// to_datetime(string, format="%Y-%m-%d %H:%M:%S").
	f["to_datetime"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(Undeprecate(in))
		if !ok {
			return nil, fmt.Errorf("strptime() argument 1 must be str, not %s", pyClassName(in, ec.fromVar(-1)))
		}
		format := "%Y-%m-%d %H:%M:%S"
		if v, ok := filterArg(args, 0, kwargs, "format"); ok {
			fs, isStr := asString(Undeprecate(v))
			if !isStr {
				return nil, fmt.Errorf("strptime() argument 2 must be str, not %s", pyClassName(v, ec.fromVar(0)))
			}
			format = fs
		}
		return strptime(s, format)
	}

	// do_vault(data, secret, salt=None, vault_id='filter_default',
	// wrap_object=False).
	f["vault"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		secretV, _ := filterArg(args, 0, kwargs, "secret")
		secret, ok := asString(Undeprecate(secretV))
		if !ok {
			return nil, fmt.Errorf("Secret passed is required to be a string, instead we got %s.", pyTypeRepr(secretV, ec.fromVar(0)))
		}
		data, ok := asString(Undeprecate(in))
		if !ok {
			return nil, fmt.Errorf("Can only vault strings, instead we got %s.", pyTypeRepr(in, ec.fromVar(-1)))
		}
		var salt []byte
		if v, ok := filterArg(args, 1, kwargs, "salt"); ok && Undeprecate(v) != nil {
			if !truthy(v) {
				return nil, pyRaiseFrom("Unable to encrypt.", "Empty or invalid salt passed to encrypt()")
			}
			salt = []byte(htmlOf(v))
		}
		vaultID := "filter_default"
		if v, ok := filterArg(args, 2, kwargs, "vault_id"); ok {
			vaultID = toStr(v)
			if Undeprecate(v) == nil {
				vaultID = ""
			}
		}
		if strings.HasPrefix(data, "$ANSIBLE_VAULT") {
			return nil, pyRaiseFrom("Unable to encrypt.", "input is already encrypted")
		}
		out, err := vault.EncryptWith([]byte(data), secret, salt, vaultID)
		if err != nil {
			return nil, pyRaiseFrom("Unable to encrypt.", err.Error())
		}
		if v, ok := filterArg(args, 3, kwargs, "wrap_object"); ok && truthy(v) {
			// ansible-core tags the secret (not the data) with the
			// ciphertext, and returns it.
			return secretV, nil
		}
		return out, nil
	}

	// do_unvault(vault, secret, vault_id='filter_default').
	f["unvault"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		secretV, _ := filterArg(args, 0, kwargs, "secret")
		secret, ok := asString(Undeprecate(secretV))
		if !ok {
			return nil, fmt.Errorf("Secret passed is required to be as string, instead we got %s.", pyTypeRepr(secretV, ec.fromVar(0)))
		}
		data, ok := asString(Undeprecate(in))
		if !ok {
			return nil, fmt.Errorf("Vault should be in the form of a string, instead we got %s.", pyTypeRepr(in, ec.fromVar(-1)))
		}
		if !strings.HasPrefix(data, "$ANSIBLE_VAULT") {
			return data, nil
		}
		plain, err := vault.Decrypt(data, secret)
		if err != nil {
			return nil, &objError{pre: []string{"Unable to decrypt."}, msg: "Decryption failed (no vault secrets were found that could decrypt).",
				value: pyShorten(data, 120)}
		}
		return pyDecodeUTF8Replace(plain), nil
	}
}

// pyKeyError is a missing key subscripting a mapping.
type pyKeyError struct{ key any }

func (e *pyKeyError) Error() string { return pyRepr(e.key) }

// pySubscript is Python's x[key] on a value: a mapping's KeyError, a
// list's or string's TypeError for a key that is not an int.
func pySubscript(x, key any) (any, error) {
	if _, m, ok := orderedMap(x); ok {
		k, isStr := asString(Undeprecate(key))
		if v, found := m[k]; isStr && found {
			return v, nil
		}
		return nil, &pyKeyError{key}
	}
	switch t := x.(type) {
	case []any:
		i, ok := asInt(Undeprecate(key))
		if !ok {
			return nil, fmt.Errorf("list indices must be integers or slices, not %s", pyClassName(key, false))
		}
		if i < 0 {
			i += int64(len(t))
		}
		if i < 0 || i >= int64(len(t)) {
			return nil, errors.New("list index out of range")
		}
		return t[i], nil
	}
	if _, ok := asString(x); ok {
		return nil, fmt.Errorf("string indices must be integers, not '%s'", pyClassName(key, false))
	}
	return nil, fmt.Errorf("'%s' object is not subscriptable", pyClassName(x, false))
}

// pyIndexInt is operator.index(v) as an int64.
func pyIndexInt(v any) (int64, error) {
	v = Undeprecate(v)
	if _, ok := v.(float64); ok || !isNumber(v) {
		return 0, fmt.Errorf("'%s' object cannot be interpreted as an integer", pyClassName(v, false))
	}
	n, ok := asInt(v)
	if !ok {
		return 0, errors.New("Python int too large to convert to C ssize_t")
	}
	return n, nil
}

func combinations(pool []any, r int) []any {
	out := []any{}
	n := len(pool)
	if r > n {
		return out
	}
	idx := make([]int, r)
	for i := range idx {
		idx[i] = i
	}
	emit := func() {
		row := make([]any, r)
		for i, j := range idx {
			row[i] = pool[j]
		}
		out = append(out, row)
	}
	emit()
	for {
		i := r - 1
		for ; i >= 0 && idx[i] == i+n-r; i-- {
		}
		if i < 0 {
			return out
		}
		idx[i]++
		for j := i + 1; j < r; j++ {
			idx[j] = idx[j-1] + 1
		}
		emit()
	}
}

func permutations(pool []any, r int) []any {
	out := []any{}
	n := len(pool)
	if r > n {
		return out
	}
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	cycles := make([]int, r)
	for i := range cycles {
		cycles[i] = n - i
	}
	emit := func() {
		row := make([]any, r)
		for i := range r {
			row[i] = pool[indices[i]]
		}
		out = append(out, row)
	}
	emit()
	for n > 0 {
		done := true
		for i := r - 1; i >= 0; i-- {
			cycles[i]--
			if cycles[i] == 0 {
				first := indices[i]
				copy(indices[i:], indices[i+1:])
				indices[n-1] = first
				cycles[i] = n - i
				continue
			}
			j := cycles[i]
			indices[i], indices[n-j] = indices[n-j], indices[i]
			emit()
			done = false
			break
		}
		if done {
			return out
		}
	}
	return out
}

// fspath is os.fspath(v) for a path function's argument.
func fspath(v any) (string, error) {
	s, ok := asString(Undeprecate(v))
	if !ok {
		return "", fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyClassName(v, false))
	}
	return s, nil
}

func pathStrFilter(fn func(string) (any, error)) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		p, err := fspath(in)
		if err != nil {
			return nil, err
		}
		return fn(p)
	}
}

// pyNormpath is posixpath.normpath.
func pyNormpath(path string) string {
	if path == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(path, "/") {
		initial = 1
		if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(path, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, c)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// pyAbspath is posixpath.abspath.
func pyAbspath(p string) string {
	if !strings.HasPrefix(p, "/") {
		cwd, _ := os.Getwd()
		if strings.HasSuffix(cwd, "/") || cwd == "" {
			p = cwd + p
		} else {
			p = cwd + "/" + p
		}
	}
	return pyNormpath(p)
}

// pyRelpath is posixpath.relpath.
func pyRelpath(path, start string) (any, error) {
	if path == "" {
		return nil, errors.New("no path specified")
	}
	split := func(p string) []string {
		tail := strings.TrimLeft(pyAbspath(p), "/")
		if tail == "" {
			return nil
		}
		return strings.Split(tail, "/")
	}
	sl, pl := split(start), split(path)
	i := 0
	for i < len(sl) && i < len(pl) && sl[i] == pl[i] {
		i++
	}
	var rel []string
	for range len(sl) - i {
		rel = append(rel, "..")
	}
	rel = append(rel, pl[i:]...)
	if len(rel) == 0 {
		return ".", nil
	}
	return strings.Join(rel, "/"), nil
}

// pyCommonpath is posixpath.commonpath.
func pyCommonpath(paths []string) (any, error) {
	if len(paths) == 0 {
		return nil, errors.New("commonpath() arg is an empty sequence")
	}
	abs := strings.HasPrefix(paths[0], "/")
	for _, p := range paths[1:] {
		if strings.HasPrefix(p, "/") != abs {
			return nil, errors.New("Can't mix absolute and relative paths")
		}
	}
	split := make([][]string, len(paths))
	for i, p := range paths {
		for _, c := range strings.Split(p, "/") {
			if c != "" && c != "." {
				split[i] = append(split[i], c)
			}
		}
	}
	less := func(a, b []string) bool {
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				return a[i] < b[i]
			}
		}
		return len(a) < len(b)
	}
	s1, s2 := split[0], split[0]
	for _, s := range split[1:] {
		if less(s, s1) {
			s1 = s
		}
		if less(s2, s) {
			s2 = s
		}
	}
	common := s1
	for i, c := range s1 {
		if c != s2[i] {
			common = s1[:i]
			break
		}
	}
	prefix := ""
	if abs {
		prefix = "/"
	}
	return prefix + strings.Join(common, "/"), nil
}

var expandvarsRe = regexp.MustCompile(`\$([A-Za-z0-9_]+|\{[^}]*\})`)

// pyExpandvars is posixpath.expandvars over the environment.
func pyExpandvars(path string) string {
	if !strings.Contains(path, "$") {
		return path
	}
	i := 0
	for {
		loc := expandvarsRe.FindStringSubmatchIndex(path[i:])
		if loc == nil {
			return path
		}
		start, end := i+loc[0], i+loc[1]
		name := path[i+loc[2] : i+loc[3]]
		if strings.HasPrefix(name, "{") && strings.HasSuffix(name, "}") {
			name = name[1 : len(name)-1]
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			i = end
			continue
		}
		tail := path[end:]
		path = path[:start] + value
		i = len(path)
		path += tail
	}
}

// ntSplitroot is ntpath.splitroot.
func ntSplitroot(p string) (drive, root, tail string) {
	normp := strings.ReplaceAll(p, "/", `\`)
	switch {
	case strings.HasPrefix(normp, `\`):
		if strings.HasPrefix(normp[1:], `\`) {
			start := 2
			if len(normp) >= 8 && strings.ToUpper(normp[:8]) == `\\?\UNC\` {
				start = 8
			}
			index := strings.Index(normp[min(start, len(normp)):], `\`)
			if index < 0 {
				return p, "", ""
			}
			index += start
			index2 := strings.Index(normp[index+1:], `\`)
			if index2 < 0 {
				return p, "", ""
			}
			index2 += index + 1
			return p[:index2], p[index2 : index2+1], p[index2+1:]
		}
		return "", p[:1], p[1:]
	case len(normp) >= 2 && normp[1] == ':':
		if len(normp) >= 3 && normp[2] == '\\' {
			return p[:2], p[2:3], p[3:]
		}
		return p[:2], "", p[2:]
	}
	return "", "", p
}

// ntSplit is ntpath.split.
func ntSplit(p string) (head, tail string) {
	d, r, rest := ntSplitroot(p)
	i := len(rest)
	for i > 0 && rest[i-1] != '/' && rest[i-1] != '\\' {
		i--
	}
	return d + r + strings.TrimRight(rest[:i], `/\`), rest[i:]
}

// PyGlob is glob.glob(pathname), for the fileglob lookup.
func PyGlob(pathname string) []string { return pyGlob(pathname) }

func hasMagic(s string) bool { return strings.ContainsAny(s, "*?[") }

// pyGlob is glob.glob(pathname): directories are read in the order the
// file system lists them (os.scandir's), and a wildcard does not match a
// hidden name.
func pyGlob(pathname string) []string {
	dir, base := pySplit(pathname)
	if !hasMagic(pathname) {
		if base != "" {
			if _, err := os.Lstat(pathname); err == nil {
				return []string{pathname}
			}
		} else if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return []string{pathname}
		}
		return nil
	}
	if dir == "" {
		return globIn("", base, false)
	}
	dirs := []string{dir}
	if dir != pathname && hasMagic(dir) {
		dirs = pyGlobDirs(dir)
	}
	var out []string
	for _, d := range dirs {
		var names []string
		if hasMagic(base) {
			names = globIn(d, base, false)
		} else if base != "" {
			if _, err := os.Lstat(filepath.Join(d, base)); err == nil {
				names = []string{base}
			}
		} else if info, err := os.Stat(d); err == nil && info.IsDir() {
			names = []string{base}
		}
		for _, n := range names {
			out = append(out, pyJoin(d, n))
		}
	}
	return out
}

// pyGlobDirs is _iglob with dironly: the directories a pattern matches.
func pyGlobDirs(pattern string) []string {
	var out []string
	for _, m := range pyGlob(pattern) {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			out = append(out, m)
		}
	}
	return out
}

// globIn is glob._glob1: the names in dir pattern matches.
func globIn(dir, pattern string, dironly bool) []string {
	d := dir
	if d == "" {
		d = "."
	}
	f, err := os.Open(d)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	f.Close()
	re, err := pyre.CompileFnmatch(pattern)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, ".") && !strings.HasPrefix(pattern, ".") {
			continue
		}
		if re.Match(n, 0, -1) != nil {
			out = append(out, n)
		}
	}
	return out
}

// pySplit is posixpath.split.
func pySplit(p string) (head, tail string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail = p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// pyJoin is posixpath.join of two parts.
func pyJoin(a, b string) string {
	switch {
	case strings.HasPrefix(b, "/"):
		return b
	case a == "" || strings.HasSuffix(a, "/"):
		return a + b
	}
	return a + "/" + b
}

// ansibleUUIDNamespace is UUID_NAMESPACE_ANSIBLE.
var ansibleUUIDNamespace, _ = parseUUID("361E6D51-FAEC-444A-9079-341386DA8E2E")

// parseUUID is uuid.UUID(hex).
func parseUUID(s string) ([]byte, error) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "urn:", ""), "uuid:", "")
	s = strings.ReplaceAll(strings.Trim(s, "{}"), "-", "")
	if len([]rune(s)) != 32 {
		return nil, errors.New("badly formed hexadecimal UUID string")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid literal for int() with base 16: %s", pyStrRepr(s))
	}
	return b, nil
}

// uuid5 is str(uuid.uuid5(namespace, name)).
func uuid5(ns []byte, name string) string {
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	b := h.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
