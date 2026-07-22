package template

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerAnsibleFilters installs the Jinja2 + Ansible filter set beyond
// the core registered in filters.go.
func registerAnsibleFilters(e *Engine) {
	f := e.Filters

	// ---- sequences ----
	f["min"] = seqReduce(func(a, b any) (bool, error) { c, err := compare(a, b); return c < 0, err })
	f["max"] = seqReduce(func(a, b any) (bool, error) { c, err := compare(a, b); return c > 0, err })

	f["sum"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if attr, ok := kwargs["attribute"]; ok {
			items, err = extractAll(items, attr)
			if err != nil {
				return nil, err
			}
		}
		var start any = int64(0)
		if len(args) > 0 {
			start = args[0]
		}
		acc := start
		for _, item := range items {
			acc, err = arith(tokAdd, acc, item)
			if err != nil {
				return nil, err
			}
		}
		return acc, nil
	}

	f["unique"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		var out []any
		for _, item := range items {
			found := false
			for _, seen := range out {
				if equal(seen, item) {
					found = true
					break
				}
			}
			if !found {
				out = append(out, item)
			}
		}
		return out, nil
	}

	f["sort"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		out := append([]any{}, items...)
		reverse := truthy(kwargs["reverse"])
		caseSensitive := truthy(kwargs["case_sensitive"])
		attr, byAttr := kwargs["attribute"]
		var sortErr error
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i], out[j]
			if byAttr {
				var err error
				if a, err = extractAttr(a, attr); err != nil {
					sortErr = err
					return false
				}
				if b, err = extractAttr(b, attr); err != nil {
					sortErr = err
					return false
				}
			}
			if !caseSensitive {
				if as, ok := asString(a); ok {
					if bs, ok2 := asString(b); ok2 {
						a, b = strings.ToLower(as), strings.ToLower(bs)
					}
				}
			}
			c, err := compare(a, b)
			if err != nil {
				sortErr = err
				return false
			}
			if reverse {
				return c > 0
			}
			return c < 0
		})
		if sortErr != nil {
			return nil, sortErr
		}
		return out, nil
	}

	f["reverse"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if s, ok := asString(in); ok {
			runes := []rune(s)
			for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
				runes[i], runes[j] = runes[j], runes[i]
			}
			return string(runes), nil
		}
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[len(items)-1-i] = item
		}
		return out, nil
	}

	f["flatten"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		levels := int64(-1)
		if len(args) > 0 {
			if n, ok := asInt(args[0]); ok {
				levels = n
			}
		}
		if lv, ok := kwargs["levels"]; ok {
			if n, ok := asInt(lv); ok {
				levels = n
			}
		}
		list, ok := in.([]any)
		if !ok {
			return nil, fmt.Errorf("expected a list, got %s", typeName(in))
		}
		return flattenList(list, levels), nil
	}

	f["zip"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		lists := [][]any{}
		first, err := iterate(in)
		if err != nil {
			return nil, err
		}
		lists = append(lists, first)
		minLen := len(first)
		for _, a := range args {
			items, err := iterate(a)
			if err != nil {
				return nil, err
			}
			lists = append(lists, items)
			if len(items) < minLen {
				minLen = len(items)
			}
		}
		out := make([]any, minLen)
		for i := 0; i < minLen; i++ {
			row := make([]any, len(lists))
			for j, l := range lists {
				row[j] = l[i]
			}
			out[i] = row
		}
		return out, nil
	}

	// map/select/reject/selectattr/rejectattr.
	f["map"] = filterMap
	f["select"] = mkSelect(false, false)
	f["reject"] = mkSelect(true, false)
	f["selectattr"] = mkSelect(false, true)
	f["rejectattr"] = mkSelect(true, true)

	// List set operations (first-seen order, like Ansible).
	f["union"] = setOp(func(inA, inB bool) bool { return inA || inB })
	f["intersect"] = setOp(func(inA, inB bool) bool { return inA && inB })
	f["difference"] = setOp(func(inA, inB bool) bool { return inA && !inB })
	f["symmetric_difference"] = setOp(func(inA, inB bool) bool { return inA != inB })

	// ---- dicts ----
	f["dict2items"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		m, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("dict2items requires a dictionary, got %s", typeName(in))
		}
		keyName, valName := "key", "value"
		if v, ok := kwargs["key_name"]; ok {
			keyName, _ = asString(v)
		}
		if v, ok := kwargs["value_name"]; ok {
			valName, _ = asString(v)
		}
		out := make([]any, 0, len(m))
		for _, k := range sortedKeys(m) {
			out = append(out, map[string]any{keyName: k, valName: m[k]})
		}
		return out, nil
	}

	f["items2dict"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, ok := in.([]any)
		if !ok {
			return nil, fmt.Errorf("items2dict requires a list, got %s", typeName(in))
		}
		keyName, valName := "key", "value"
		if v, ok := kwargs["key_name"]; ok {
			keyName, _ = asString(v)
		}
		if v, ok := kwargs["value_name"]; ok {
			valName, _ = asString(v)
		}
		out := make(map[string]any, len(items))
		for _, item := range items {
			m, ok := anyToMap(item)
			if !ok {
				return nil, fmt.Errorf("items2dict entries must be dictionaries")
			}
			k, kOK := m[keyName]
			v, vOK := m[valName]
			if !kOK || !vOK {
				return nil, fmt.Errorf("items2dict entry missing %q or %q", keyName, valName)
			}
			ks, ok := asString(k)
			if !ok {
				ks = toStr(k)
			}
			out[ks] = v
		}
		return out, nil
	}

	f["combine"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		base, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("combine requires dictionaries, got %s", typeName(in))
		}
		recursive := truthy(kwargs["recursive"])
		if lm, ok := kwargs["list_merge"]; ok {
			if s, _ := asString(lm); s != "" && s != "replace" {
				return nil, fmt.Errorf("list_merge=%s is not supported (only 'replace')", s)
			}
		}
		out := copyMap(base)
		for _, a := range args {
			m, ok := anyToMap(a)
			if !ok {
				return nil, fmt.Errorf("combine arguments must be dictionaries, got %s", typeName(a))
			}
			mergeInto(out, m, recursive)
		}
		return out, nil
	}

	f["dictsort"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		m, ok := anyToMap(in)
		if !ok {
			return nil, fmt.Errorf("dictsort requires a dictionary")
		}
		out := make([]any, 0, len(m))
		for _, k := range sortedKeys(m) {
			out = append(out, []any{k, m[k]})
		}
		return out, nil
	}

	// ---- serialization ----
	f["to_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		data, err := json.Marshal(jsonSanitize(in))
		if err != nil {
			return nil, err
		}
		return string(data), nil
	}
	f["to_nice_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		indent := int64(4)
		if v, ok := kwargs["indent"]; ok {
			if n, ok := asInt(v); ok {
				indent = n
			}
		}
		data, err := json.MarshalIndent(jsonSanitize(in), "", strings.Repeat(" ", int(indent)))
		if err != nil {
			return nil, err
		}
		return string(data), nil
	}
	f["from_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("from_json requires a string")
		}
		var out any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, err
		}
		return normalizeJSON(out), nil
	}
	f["to_yaml"] = mkToYAML(2)
	f["to_nice_yaml"] = mkToYAML(4)
	f["from_yaml"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("from_yaml requires a string")
		}
		return yaml.Unmarshal([]byte(s), "<from_yaml>")
	}
	f["from_yaml_all"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("from_yaml_all requires a string")
		}
		file, err := yaml.Parse([]byte(s), "<from_yaml_all>")
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(file.Docs))
		for _, doc := range file.Docs {
			v, err := doc.Decode()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}

	f["b64encode"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("b64encode requires a string")
		}
		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	}
	f["b64decode"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("b64decode requires a string")
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		return string(data), nil
	}

	f["hash"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("hash requires a string")
		}
		algo := "sha1"
		if len(args) > 0 {
			algo, _ = asString(args[0])
		}
		var h hash.Hash
		switch algo {
		case "md5":
			h = md5.New()
		case "sha1":
			h = sha1.New()
		case "sha224":
			h = sha256.New224()
		case "sha256":
			h = sha256.New()
		case "sha384":
			h = sha512.New384()
		case "sha512":
			h = sha512.New()
		default:
			return nil, fmt.Errorf("unsupported hash algorithm %q", algo)
		}
		h.Write([]byte(s))
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	f["checksum"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return e.Filters["hash"](ec, in, []any{"sha1"}, nil)
	}

	f["quote"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'", nil
	}

	f["indent"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("indent requires a string")
		}
		width := int64(4)
		if len(args) > 0 {
			if n, ok := asInt(args[0]); ok {
				width = n
			}
		}
		if v, ok := kwargs["width"]; ok {
			if n, ok := asInt(v); ok {
				width = n
			}
		}
		first := truthy(kwargs["first"]) || (len(args) > 1 && truthy(args[1]))
		pad := strings.Repeat(" ", int(width))
		lines := strings.Split(s, "\n")
		for i := range lines {
			if (i > 0 || first) && lines[i] != "" {
				lines[i] = pad + lines[i]
			}
		}
		return strings.Join(lines, "\n"), nil
	}

	// ---- paths (control-node semantics) ----
	f["basename"] = pathFilter(filepath.Base)
	f["dirname"] = pathFilter(filepath.Dir)
	f["expanduser"] = pathFilter(func(p string) string {
		if strings.HasPrefix(p, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				return home + p[1:]
			}
		}
		return p
	})
	f["realpath"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("realpath requires a string")
		}
		resolved, err := filepath.EvalSymlinks(s)
		if err != nil {
			return filepath.Clean(s), nil
		}
		return resolved, nil
	}
	f["splitext"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("splitext requires a string")
		}
		ext := filepath.Ext(s)
		return []any{strings.TrimSuffix(s, ext), ext}, nil
	}
	f["path_join"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		var parts []string
		if list, ok := in.([]any); ok {
			for _, p := range list {
				s, ok := asString(p)
				if !ok {
					return nil, fmt.Errorf("path_join elements must be strings")
				}
				parts = append(parts, s)
			}
		} else if s, ok := asString(in); ok {
			parts = append(parts, s)
			for _, a := range args {
				as, ok := asString(a)
				if !ok {
					return nil, fmt.Errorf("path_join elements must be strings")
				}
				parts = append(parts, as)
			}
		} else {
			return nil, fmt.Errorf("path_join requires a string or list")
		}
		// Python os.path.join semantics: an absolute component resets.
		out := ""
		for _, p := range parts {
			if strings.HasPrefix(p, "/") || out == "" {
				out = p
			} else {
				out = strings.TrimSuffix(out, "/") + "/" + p
			}
		}
		return out, nil
	}

	// ---- misc ----
	f["ternary"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if len(args) < 2 {
			return nil, fmt.Errorf("ternary requires true and false values")
		}
		if in == nil && len(args) > 2 {
			return args[2], nil
		}
		if truthy(in) {
			return args[0], nil
		}
		return args[1], nil
	}

	f["extract"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if len(args) < 1 {
			return nil, fmt.Errorf("extract requires a container")
		}
		var cur any = args[0]
		keys := []any{in}
		if len(args) > 1 {
			if more, ok := args[1].([]any); ok {
				keys = append(keys, more...)
			} else {
				keys = append(keys, args[1:]...)
			}
		}
		for _, key := range keys {
			var err error
			cur, err = ec.getItem(cur, key, 0)
			if err != nil {
				return nil, err
			}
			if u, ok := cur.(Undefined); ok {
				return Undefined{Name: u.Name}, nil
			}
		}
		return cur, nil
	}

	f["abs"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if n, ok := asInt(in); ok {
			if _, isBool := in.(bool); !isBool {
				if n < 0 {
					return -n, nil
				}
				return n, nil
			}
		}
		if fv, ok := in.(float64); ok {
			if fv < 0 {
				return -fv, nil
			}
			return fv, nil
		}
		return nil, fmt.Errorf("abs requires a number, got %s", typeName(in))
	}

	f["round"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		fv, ok := asFloat(in)
		if !ok {
			return nil, fmt.Errorf("round requires a number")
		}
		precision := int64(0)
		if len(args) > 0 {
			if n, ok := asInt(args[0]); ok {
				precision = n
			}
		}
		method := "common"
		if len(args) > 1 {
			method, _ = asString(args[1])
		}
		mult := 1.0
		for i := int64(0); i < precision; i++ {
			mult *= 10
		}
		v := fv * mult
		switch method {
		case "common":
			if v >= 0 {
				v = float64(int64(v + 0.5))
			} else {
				v = float64(int64(v - 0.5))
			}
		case "floor":
			v = float64(int64(v))
			if fv < 0 && v != fv*mult {
				v--
			}
		case "ceil":
			t := float64(int64(v))
			if v > t {
				v = t + 1
			} else {
				v = t
			}
		default:
			return nil, fmt.Errorf("unknown rounding method %q", method)
		}
		return v / mult, nil
	}
}

// ---- helpers ----

func seqReduce(better func(a, b any) (bool, error)) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if attr, ok := kwargs["attribute"]; ok {
			items, err = extractAll(items, attr)
			if err != nil {
				return nil, err
			}
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("sequence is empty")
		}
		best := items[0]
		for _, item := range items[1:] {
			b, err := better(item, best)
			if err != nil {
				return nil, err
			}
			if b {
				best = item
			}
		}
		return best, nil
	}
}

func flattenList(list []any, levels int64) []any {
	var out []any
	for _, item := range list {
		if item == nil {
			continue // Ansible's flatten skips None
		}
		if sub, ok := item.([]any); ok && levels != 0 {
			out = append(out, flattenList(sub, levels-1)...)
			continue
		}
		out = append(out, item)
	}
	return out
}

// extractAttr follows a dotted attribute path into maps.
func extractAttr(item, attr any) (any, error) {
	path, ok := asString(attr)
	if !ok {
		return nil, fmt.Errorf("attribute name must be a string")
	}
	cur := item
	for _, seg := range strings.Split(path, ".") {
		m, ok := anyToMap(cur)
		if !ok {
			return nil, fmt.Errorf("cannot access attribute %q on %s", seg, typeName(cur))
		}
		cur, ok = m[seg]
		if !ok {
			return Undefined{Name: path}, nil
		}
	}
	return cur, nil
}

func extractAll(items []any, attr any) ([]any, error) {
	out := make([]any, len(items))
	for i, item := range items {
		v, err := extractAttr(item, attr)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// filterMap implements map('filtername', args...) and map(attribute=...).
func filterMap(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	items, err := iterate(in)
	if err != nil {
		return nil, err
	}
	if attr, ok := kwargs["attribute"]; ok {
		out := make([]any, len(items))
		for i, item := range items {
			v, err := extractAttr(item, attr)
			if err != nil {
				return nil, err
			}
			if u, isU := v.(Undefined); isU {
				if def, hasDef := kwargs["default"]; hasDef {
					v = def
				} else {
					return nil, &UndefinedError{Pos: ec.pos, Name: u.Name}
				}
			}
			out[i] = v
		}
		return out, nil
	}
	if len(args) == 0 {
		return items, nil
	}
	name, ok := asString(args[0])
	if !ok {
		return nil, fmt.Errorf("map requires a filter name")
	}
	fn, ok := ec.engine.Filters[name]
	if !ok {
		return nil, fmt.Errorf("no filter named %q", name)
	}
	out := make([]any, len(items))
	for i, item := range items {
		v, err := fn(ec, item, args[1:], nil)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// mkSelect builds select/reject/selectattr/rejectattr.
func mkSelect(negate, byAttr bool) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		var attr any
		if byAttr {
			if len(args) == 0 {
				return nil, fmt.Errorf("selectattr requires an attribute name")
			}
			attr = args[0]
			args = args[1:]
		}
		var test TestFunc
		var testArgs []any
		if len(args) > 0 {
			name, ok := asString(args[0])
			if !ok {
				return nil, fmt.Errorf("test name must be a string")
			}
			test, ok = ec.engine.Tests[name]
			if !ok {
				return nil, fmt.Errorf("no test named %q", name)
			}
			testArgs = args[1:]
		}
		var out []any
		for _, item := range items {
			subject := item
			if byAttr {
				subject, err = extractAttr(item, attr)
				if err != nil {
					return nil, err
				}
			}
			var keep bool
			if test != nil {
				keep, err = test(ec, subject, testArgs)
				if err != nil {
					return nil, err
				}
			} else {
				if isUndefined(subject) {
					keep = false
				} else {
					keep = truthy(subject)
				}
			}
			if keep != negate {
				out = append(out, item)
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
}

func setOp(keep func(inA, inB bool) bool) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("set operation requires exactly one argument")
		}
		a, ok := in.([]any)
		if !ok {
			return nil, fmt.Errorf("set operations require lists, got %s", typeName(in))
		}
		b, ok := args[0].([]any)
		if !ok {
			return nil, fmt.Errorf("set operations require lists, got %s", typeName(args[0]))
		}
		contains := func(list []any, v any) bool {
			for _, item := range list {
				if equal(item, v) {
					return true
				}
			}
			return false
		}
		var out []any
		appendUnique := func(v any) {
			for _, seen := range out {
				if equal(seen, v) {
					return
				}
			}
			out = append(out, v)
		}
		for _, v := range a {
			if keep(true, contains(b, v)) {
				appendUnique(v)
			}
		}
		for _, v := range b {
			if keep(contains(a, v), true) && keep(false, true) {
				appendUnique(v)
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mergeInto(dst, src map[string]any, recursive bool) {
	for k, v := range src {
		if recursive {
			if dstMap, ok := anyToMap(dst[k]); ok {
				if srcMap, ok := anyToMap(v); ok {
					merged := copyMap(dstMap)
					mergeInto(merged, srcMap, true)
					dst[k] = merged
					continue
				}
			}
		}
		dst[k] = v
	}
}

func mkToYAML(indent int) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		n := int64(indent)
		if v, ok := kwargs["indent"]; ok {
			if i, ok := asInt(v); ok {
				n = i
			}
		}
		data, err := yaml.Marshal(jsonSanitize(in), int(n))
		if err != nil {
			return nil, err
		}
		return string(data), nil
	}
}

func pathFilter(fn func(string) string) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("expected a string path, got %s", typeName(in))
		}
		return fn(s), nil
	}
}

// jsonSanitize converts engine-internal values into plain JSON-encodable
// values (UnsafeString -> string, Mapping -> map, lazy range -> list).
func jsonSanitize(v any) any {
	switch t := v.(type) {
	case yaml.UnsafeString:
		return string(t)
	case Mapping:
		return jsonSanitize(mappingToMap(t))
	case *rangeValue:
		return t.materialize()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = jsonSanitize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = jsonSanitize(val)
		}
		return out
	}
	return v
}

// normalizeJSON converts json.Unmarshal output (float64 numbers) to the
// engine's int64-preferring value model.
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = normalizeJSON(item)
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSON(val)
		}
		return t
	}
	return v
}
