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
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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
		keys, m, ok := orderedMap(in)
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
		for _, k := range keys {
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
		recursive := truthy(kwargs["recursive"])
		listMerge := "replace"
		if lm, ok := kwargs["list_merge"]; ok {
			s, _ := asString(lm)
			switch s {
			case "", "replace", "keep", "append", "prepend", "append_rp", "prepend_rp":
				if s != "" {
					listMerge = s
				}
			default:
				return nil, fmt.Errorf("combine: unsupported list_merge %q", s)
			}
		}
		// Build the result as an ordered map so merged keys keep base-then-new
		// insertion order (Ansible's combine preserves it).
		out, ok := asOMap(in)
		if !ok {
			return nil, fmt.Errorf("combine requires dictionaries, got %s", typeName(in))
		}
		for _, a := range args {
			m, ok := asOMap(a)
			if !ok {
				return nil, fmt.Errorf("combine arguments must be dictionaries, got %s", typeName(a))
			}
			mergeOMap(out, m, recursive, listMerge)
		}
		return out, nil
	}

	f["groupby"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		var attr any
		switch {
		case len(args) > 0:
			attr = args[0]
		case kwargs["attribute"] != nil:
			attr = kwargs["attribute"]
		default:
			return nil, fmt.Errorf("groupby requires an attribute")
		}
		// Jinja default is case-insensitive grouping; case_sensitive=true keeps
		// distinct-case keys apart.
		caseSensitive := truthy(kwargs["case_sensitive"])
		def, hasDef := kwargs["default"]
		type group struct {
			key   any
			items []any
		}
		var order []*group
		index := map[string]*group{}
		for _, item := range items {
			k, err := extractAttr(item, attr)
			if err != nil {
				return nil, err
			}
			if _, und := k.(Undefined); und && hasDef {
				k = def
			}
			ck := groupKey(k, caseSensitive)
			g, ok := index[ck]
			if !ok {
				// The grouper is the first-seen actual value, not folded.
				g = &group{key: k}
				index[ck] = g
				order = append(order, g)
			}
			g.items = append(g.items, item)
		}
		sort.SliceStable(order, func(i, j int) bool {
			a, b := order[i].key, order[j].key
			if !caseSensitive {
				if as, ok := asString(a); ok {
					if bs, ok := asString(b); ok {
						a, b = strings.ToLower(as), strings.ToLower(bs)
					}
				}
			}
			c, err := compare(a, b)
			if err != nil {
				return false
			}
			return c < 0
		})
		out := make([]any, len(order))
		for i, g := range order {
			out[i] = []any{g.key, g.items}
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

	// Math filters (all return floats, matching ansible).
	f["pow"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		base, ok := asFloat(in)
		exp, ok2 := asFloatArg(args, 0)
		if !ok || !ok2 {
			return nil, fmt.Errorf("pow requires numbers")
		}
		return math.Pow(base, exp), nil
	}
	f["root"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		x, ok := asFloat(in)
		if !ok {
			return nil, fmt.Errorf("root requires a number")
		}
		n := 2.0
		if v, ok := asFloatArg(args, 0); ok {
			n = v
		}
		if n == 2 {
			return math.Sqrt(x), nil // exact for the common square-root case
		}
		return math.Pow(x, 1/n), nil
	}
	f["log"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		x, ok := asFloat(in)
		if !ok {
			return nil, fmt.Errorf("log requires a number")
		}
		if base, ok := asFloatArg(args, 0); ok {
			return math.Log(x) / math.Log(base), nil
		}
		return math.Log(x), nil
	}

	// random: choose a random element of a list, or a random int in [0, N).
	// A seed makes it deterministic within understudy (the sequence does not
	// match Ansible's Python PRNG, so seeded values are not cross-checked).
	f["random"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		rng := newRand(kwargs["seed"])
		if items, ok := in.([]any); ok {
			if len(items) == 0 {
				return nil, nil
			}
			return items[rng.Intn(len(items))], nil
		}
		n, ok := asInt(in)
		if !ok {
			return nil, fmt.Errorf("random requires a list or an integer, got %s", typeName(in))
		}
		if n <= 0 {
			return int64(0), nil
		}
		return rng.Int63n(n), nil
	}

	// password_hash(scheme, salt=None, rounds=None): glibc crypt(3) hash of a
	// password. Only sha256/sha512 are supported (the schemes real playbooks
	// use); a salt must be given for a deterministic result.
	f["password_hash"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		pw, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("password_hash requires a string")
		}
		scheme, err := argStr(args, 0, "sha512")
		if err != nil {
			return nil, err
		}
		use512 := scheme == "sha512"
		if !use512 && scheme != "sha256" {
			return nil, fmt.Errorf("password_hash: unsupported scheme %q (sha256/sha512 only)", scheme)
		}
		salt, _ := asString(args1(args, 1))
		if s, ok := kwargs["salt"]; ok {
			salt, _ = asString(s)
		}
		if salt == "" {
			return nil, fmt.Errorf("password_hash: a salt is required (random salts are not supported)")
		}
		// Ansible's password_hash uses passlib, whose default rounds differ by
		// scheme (sha512=656000, sha256=535000) — not glibc's 5000. Match it so
		// unqualified hashes agree.
		rounds := 535000
		if use512 {
			rounds = 656000
		}
		if v, ok := kwargs["rounds"]; ok {
			if n, ok := asInt(v); ok {
				rounds = int(n)
			}
		} else if n, ok := asInt(args1(args, 2)); ok {
			rounds = int(n)
		}
		return shaCrypt(pw, salt, rounds, use512), nil
	}

	// strftime(timestamp): the format string is the input; the epoch seconds
	// are the argument (defaulting to now is unsupported — a timestamp must be
	// given so results stay deterministic). Rendered in local time, like
	// ansible.
	f["strftime"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		format, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("strftime requires a format string")
		}
		ts, ok := asFloatArg(args, 0)
		if !ok {
			return nil, fmt.Errorf("strftime requires an epoch-seconds argument")
		}
		return strftime(format, int64(ts)), nil
	}

	f["human_readable"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		n, ok := asFloat(in)
		if !ok {
			if s, sok := asString(in); sok {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err != nil {
					return nil, fmt.Errorf("human_readable requires a number, got %q", s)
				}
				n = parsed
			} else {
				return nil, fmt.Errorf("human_readable requires a number, got %s", typeName(in))
			}
		}
		return humanReadable(n, truthy(kwargs["isbits"])), nil
	}
	f["human_to_bytes"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if n, ok := asInt(in); ok {
			return n, nil
		}
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
		}
		return humanToBytes(s)
	}

	// ---- serialization ----
	// to_json/to_nice_json match Python's json.dumps: "', '" item and "': '"
	// key separators (Go's encoding/json omits the spaces), keys sorted.
	f["to_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// json.dumps default sort_keys=False: preserve dict insertion order.
		return pyJSON(in, 0, false), nil
	}
	f["to_nice_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		indent := 4
		if v, ok := kwargs["indent"]; ok {
			if n, ok := asInt(v); ok {
				indent = int(n)
			}
		}
		// Ansible's to_nice_json passes sort_keys=True.
		return pyJSON(in, indent, true), nil
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

	f["format"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("format requires a string, got %s", typeName(in))
		}
		// Jinja2's format filter is Python's % operator; Go's fmt verbs
		// cover the common specifiers (%s %d %f %x %o %e %g, width/precision).
		return fmt.Sprintf(s, args...), nil
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
		mult := math.Pow(10, float64(precision))
		v := fv * mult
		switch method {
		case "common":
			// Jinja2's 'common' method calls Python's round(), which is
			// banker's rounding (half to even): round(2.5)==2, round(3.5)==4.
			v = math.RoundToEven(v)
		case "floor":
			v = math.Floor(v)
		case "ceil":
			v = math.Ceil(v)
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

// groupKey canonicalizes a grouper value into a map key. Case-insensitive
// grouping folds string keys; the type prefix keeps e.g. int 1 and string "1"
// in separate groups.
func groupKey(k any, caseSensitive bool) string {
	if s, ok := asString(k); ok {
		if !caseSensitive {
			s = strings.ToLower(s)
		}
		return "s:" + s
	}
	return typeName(k) + ":" + toStr(k)
}

// extractAttr follows a dotted attribute path into maps.
func extractAttr(item, attr any) (any, error) {
	path, ok := asString(attr)
	if !ok {
		return nil, fmt.Errorf("attribute name must be a string")
	}
	cur := item
	for _, seg := range strings.Split(path, ".") {
		if m, ok := anyToMap(cur); ok {
			cur, ok = m[seg]
			if !ok {
				return Undefined{Name: path}, nil
			}
			continue
		}
		// A numeric segment indexes a list (Jinja's getitem), so
		// attribute='0' works on a groupby pair or any sequence.
		if lst, ok := cur.([]any); ok {
			if idx, err := strconv.Atoi(seg); err == nil {
				if idx < 0 {
					idx += len(lst)
				}
				if idx < 0 || idx >= len(lst) {
					return Undefined{Name: path}, nil
				}
				cur = lst[idx]
				continue
			}
		}
		return nil, fmt.Errorf("cannot access attribute %q on %s", seg, typeName(cur))
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

func asFloatArg(args []any, i int) (float64, bool) {
	if i >= len(args) {
		return 0, false
	}
	return asFloat(args[i])
}

// args1 returns the i-th positional argument, or nil if absent.
func args1(args []any, i int) any {
	if i >= len(args) {
		return nil
	}
	return args[i]
}

// newRand returns a PRNG seeded from the given value (deterministic) or from
// the clock when seed is nil.
func newRand(seed any) *rand.Rand {
	if seed == nil {
		return rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	h := fnv.New64a()
	h.Write([]byte(toStr(seed)))
	return rand.New(rand.NewSource(int64(h.Sum64())))
}

// strftime formats epoch seconds using Python strftime codes, in local time
// (matching ansible's strftime filter). Unknown codes pass through literally.
func strftime(format string, ts int64) string {
	t := time.Unix(ts, 0)
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			b.WriteString(t.Format("2006"))
		case 'y':
			b.WriteString(t.Format("06"))
		case 'm':
			b.WriteString(t.Format("01"))
		case 'd':
			b.WriteString(t.Format("02"))
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			b.WriteString(t.Format("15"))
		case 'I':
			b.WriteString(t.Format("03"))
		case 'M':
			b.WriteString(t.Format("04"))
		case 'S':
			b.WriteString(t.Format("05"))
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'w':
			b.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case 'z':
			b.WriteString(t.Format("-0700"))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

// humanReadable formats a byte (or bit) count with base-1024 units and two
// decimals, matching ansible's human_readable filter ("1.00 MB").
func humanReadable(size float64, isbits bool) string {
	units := []string{"Bytes", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}
	if isbits {
		units = []string{"bit", "Kb", "Mb", "Gb", "Tb", "Pb", "Eb", "Zb", "Yb"}
	}
	n, i := size, 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", n, units[i])
}

// humanToBytes parses a size like "1 MB", "2MB", "1.5 GB", or "1024" into a
// byte count (base 1024), matching ansible's human_to_bytes filter.
func humanToBytes(s string) (any, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] == '.' || s[i] == '+' || s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, err := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	if err != nil {
		return nil, fmt.Errorf("human_to_bytes: cannot parse number in %q", s)
	}
	exp := 0
	if unit := strings.TrimSpace(s[i:]); unit != "" {
		switch unit[0] {
		case 'B', 'b':
			exp = 0
		case 'K', 'k':
			exp = 1
		case 'M', 'm':
			exp = 2
		case 'G', 'g':
			exp = 3
		case 'T', 't':
			exp = 4
		case 'P', 'p':
			exp = 5
		case 'E', 'e':
			exp = 6
		case 'Z', 'z':
			exp = 7
		case 'Y', 'y':
			exp = 8
		default:
			return nil, fmt.Errorf("human_to_bytes: unknown unit %q", unit)
		}
	}
	return int64(num * math.Pow(1024, float64(exp))), nil
}

// asOMap returns an ordered-map copy of a dict value: *OMap/Mapping keep their
// own key order, a plain Go map is ordered by sorted keys. The copy is shallow
// (values are shared), so callers may mutate the returned map's key set without
// disturbing the original — used by combine to stay non-destructive.
func asOMap(v any) (*yaml.OMap, bool) {
	keys, m, ok := orderedMap(v)
	if !ok {
		return nil, false
	}
	out := yaml.NewOMap()
	for _, k := range keys {
		out.Set(k, m[k])
	}
	return out, true
}

// mergeOMap merges src into dst in place: src keys are visited in order, new
// keys append, existing keys update. With recursive=true, two dict values at
// the same key merge instead of the src value replacing the dst value.
// listMerge controls what happens when both values at a key are lists:
// "replace" (default), "keep", "append", "prepend", "append_rp", "prepend_rp".
func mergeOMap(dst, src *yaml.OMap, recursive bool, listMerge string) {
	for _, k := range src.Keys() {
		v := src.Get(k)
		existing, has := dst.GetItem(k)
		if recursive {
			if dstChild, ok := asOMap(existing); ok {
				if srcChild, ok := asOMap(v); ok {
					mergeOMap(dstChild, srcChild, true, listMerge)
					dst.Set(k, dstChild)
					continue
				}
			}
		}
		if has && listMerge != "replace" {
			if da, ok := existing.([]any); ok {
				if sa, ok := v.([]any); ok {
					dst.Set(k, mergeLists(da, sa, listMerge))
					continue
				}
			}
		}
		dst.Set(k, v)
	}
}

// mergeLists combines two lists per combine's list_merge strategy. The *_rp
// ("remove present") variants drop elements of the base that reappear in the
// override before joining.
func mergeLists(base, over []any, strategy string) []any {
	switch strategy {
	case "keep":
		return base
	case "append":
		return append(append([]any{}, base...), over...)
	case "prepend":
		return append(append([]any{}, over...), base...)
	case "append_rp":
		out := []any{}
		for _, x := range base {
			if !listContains(over, x) {
				out = append(out, x)
			}
		}
		return append(out, over...)
	case "prepend_rp":
		out := append([]any{}, over...)
		for _, x := range base {
			if !listContains(over, x) {
				out = append(out, x)
			}
		}
		return out
	default: // replace
		return over
	}
}

func listContains(list []any, v any) bool {
	for _, x := range list {
		if equal(x, v) {
			return true
		}
	}
	return false
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

// pyJSON serializes a value the way Python's json.dumps does — the format
// Ansible's to_json/to_nice_json produce. indent==0 yields the compact form
// with ", " and ": " separators; indent>0 pretty-prints. sortKeys mirrors
// json.dumps' sort_keys: to_json passes false (dict insertion order kept),
// to_nice_json passes true. Plain Go maps have no inherent order and are
// always sorted; *OMap/Mapping honor sortKeys.
func pyJSON(v any, indent int, sortKeys bool) string {
	var b strings.Builder
	writePyJSON(&b, v, indent, 0, sortKeys)
	return b.String()
}

func writePyJSON(b *strings.Builder, v any, indent, depth int, sortKeys bool) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(jsonQuote(t))
	case yaml.UnsafeString:
		b.WriteString(jsonQuote(string(t)))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case int:
		b.WriteString(strconv.Itoa(t))
	case float64:
		b.WriteString(pyFloatStr(t))
	case *rangeValue:
		writePyJSON(b, t.materialize(), indent, depth, sortKeys)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range t {
			pyJSONSep(b, i, indent, depth+1)
			writePyJSON(b, item, indent, depth+1, sortKeys)
		}
		pyJSONClose(b, ']', indent, depth)
	case map[string]any:
		writePyJSONObject(b, sortedKeys(t), func(k string) any { return t[k] }, len(t), indent, depth, sortKeys)
	case Mapping:
		keys := t.Keys()
		if sortKeys {
			keys = append([]string(nil), keys...)
			sort.Strings(keys)
		}
		writePyJSONObject(b, keys, func(k string) any { v, _ := t.GetItem(k); return v }, t.Len(), indent, depth, sortKeys)
	default:
		b.WriteString(jsonQuote(toStr(v)))
	}
}

// writePyJSONObject renders a JSON object given keys in the desired order and
// a value accessor. Plain-map keys arrive pre-sorted; Mapping keys arrive in
// the order chosen by the caller (insertion or sorted).
func writePyJSONObject(b *strings.Builder, keys []string, get func(string) any, n, indent, depth int, sortKeys bool) {
	if n == 0 {
		b.WriteString("{}")
		return
	}
	b.WriteByte('{')
	for i, k := range keys {
		pyJSONSep(b, i, indent, depth+1)
		b.WriteString(jsonQuote(k))
		b.WriteString(": ")
		writePyJSON(b, get(k), indent, depth+1, sortKeys)
	}
	pyJSONClose(b, '}', indent, depth)
}

func pyJSONSep(b *strings.Builder, i, indent, depth int) {
	if i > 0 {
		if indent > 0 {
			b.WriteByte(',')
		} else {
			b.WriteString(", ")
		}
	}
	if indent > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", indent*depth))
	}
}

func pyJSONClose(b *strings.Builder, closer byte, indent, depth int) {
	if indent > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", indent*depth))
	}
	b.WriteByte(closer)
}

// jsonQuote quotes a string like encoding/json (escapes match Python's
// default ensure_ascii=False for the common printable case).
func jsonQuote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
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
