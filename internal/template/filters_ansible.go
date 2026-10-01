package template

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerAnsibleFilters installs the Jinja2 + Ansible filter set beyond
// the core registered in filters.go.
func registerAnsibleFilters(e *Engine) {
	f := e.Filters

	// ---- sequences ----
	f["min"] = seqReduce("<")
	f["max"] = seqReduce(">")

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
				if a, err = extractStrict(a, attr); err != nil {
					sortErr = err
					return false
				}
				if b, err = extractStrict(b, attr); err != nil {
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
				if sortErr == nil {
					// Items of a variable's list are its lazy and tagged values.
					v := ec.fromVar(-1) && !byAttr
					sortErr = fmt.Errorf("'<' not supported between instances of '%s' and '%s'", pyClassName(a, v), pyClassName(b, v))
				}
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
		list, ok := Undeprecate(in).([]any)
		if !ok {
			items, err := iterate(in) // for element in mylist
			if err != nil {
				return nil, errNotIterable(in, ec.fromVar(-1))
			}
			list = items
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

	// Set operations (see pyset.go).
	f["union"] = setOp(setUnion)
	f["intersect"] = setOp(setIntersect)
	f["difference"] = setOp(setDifference)
	f["symmetric_difference"] = setOp(setSymmetricDifference)

	// ---- dicts ----
	// key_name and value_name, positional or by keyword.
	kvNames := func(args []any, kwargs map[string]any) (any, any) {
		var keyName, valName any = "key", "value"
		if len(args) > 0 {
			keyName = args[0]
		} else if v, ok := kwargs["key_name"]; ok {
			keyName = v
		}
		if len(args) > 1 {
			valName = args[1]
		} else if v, ok := kwargs["value_name"]; ok {
			valName = v
		}
		return keyName, valName
	}
	f["dict2items"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		keys, m, ok := orderedMap(in)
		if !ok {
			return nil, fmt.Errorf("dict2items requires a dictionary, got %s instead.", pyTypeRepr(in, ec.fromVar(-1)))
		}
		keyName, valName := kvNames(args, kwargs)
		out := make([]any, 0, len(m))
		for _, k := range keys {
			item := yaml.NewOMap()
			item.Set(toStr(keyName), mapKey(in, k))
			item.Set(toStr(valName), m[k])
			out = append(out, item)
		}
		return out, nil
	}

	f["items2dict"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, ok := Undeprecate(in).([]any)
		if !ok {
			return nil, fmt.Errorf("items2dict requires a list, got %s instead.", pyTypeRepr(in, ec.fromVar(-1)))
		}
		keyName, valName := kvNames(args, kwargs)
		out := yaml.NewOMap()
		for _, item := range items {
			m, ok := anyToMap(item)
			if !ok {
				// item[key_name] on a non-mapping: TypeError.
				return nil, whileHandling("items2dict requires a list of dictionaries, got %s instead.", toStr(in))
			}
			k, kOK := m[toStr(keyName)]
			v, vOK := m[toStr(valName)]
			if !kOK || !vOK {
				return nil, whileHandling("items2dict requires each dictionary in the list to contain the keys '%s' and '%s', got %s instead.",
					toStr(keyName), toStr(valName), toStr(in))
			}
			ks, ok := asString(k)
			if !ok {
				ks = toStr(k)
			}
			out.Set(ks, v)
		}
		return out, nil
	}

	f["combine"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		for k := range kwargs {
			if k != "recursive" && k != "list_merge" {
				return nil, fmt.Errorf("'recursive' and 'list_merge' are the only valid keyword arguments")
			}
		}
		recursive := truthy(kwargs["recursive"])
		listMerge := "replace"
		if lm, ok := kwargs["list_merge"]; ok {
			listMerge, _ = asString(lm)
		}
		// dictionaries = flatten(terms, levels=1): a list of dicts
		// combines its items; nulls are skipped.
		type term struct {
			v       any
			fromVar bool
		}
		var dicts []term
		for i, t := range append([]any{in}, args...) {
			fromVar := ec.fromVar(i - 1)
			if isFlattenNull(t) {
				continue
			}
			if list, ok := Undeprecate(t).([]any); ok {
				for _, item := range list {
					if !isFlattenNull(item) {
						dicts = append(dicts, term{item, fromVar})
					}
				}
				continue
			}
			dicts = append(dicts, term{t, fromVar})
		}
		switch len(dicts) {
		case 0:
			return yaml.NewOMap(), nil
		case 1:
			return dicts[0].v, nil
		}
		// merge_hash runs from the highest priority (last) down, checking
		// list_merge and then that both sides are dicts.
		for i := len(dicts) - 2; i >= 0; i-- {
			switch listMerge {
			case "replace", "keep", "append", "prepend", "append_rp", "prepend_rp":
			default:
				return nil, fmt.Errorf("merge_hash: 'list_merge' argument can only be equal to 'replace', 'keep', 'append', 'prepend', 'append_rp' or 'prepend_rp'")
			}
			x, y := dicts[i], dicts[i+1]
			_, xok := asOMap(x.v)
			_, yok := asOMap(y.v)
			if i < len(dicts)-2 {
				yok = true // the merged result so far
				y.v, y.fromVar = map[string]any{}, false
			}
			if !xok || !yok {
				return nil, fmt.Errorf("failed to combine variables, expected dicts but got a '%s' and a '%s'.",
					pyClassName(x.v, x.fromVar), pyClassName(y.v, y.fromVar))
			}
		}
		// Build the result as an ordered map so merged keys keep base-then-new
		// insertion order (Ansible's combine preserves it).
		out, _ := asOMap(dicts[0].v)
		for _, d := range dicts[1:] {
			m, _ := asOMap(d.v)
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
			if u, und := k.(Undefined); und {
				if !hasDef {
					return nil, u.useError(ec.pos)
				}
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
		// Jinja's do_dictsort(value, case_sensitive=False, by='key',
		// reverse=False).
		opt := func(i int, name string, def any) any {
			if i < len(args) {
				return args[i]
			}
			if v, ok := kwargs[name]; ok {
				return v
			}
			return def
		}
		caseSensitive := truthy(opt(0, "case_sensitive", false))
		pos := 0
		switch by := opt(1, "by", "key"); toStr(by) {
		case "key":
		case "value":
			pos = 1
		default:
			return nil, fmt.Errorf("You can only sort by either \"key\" or \"value\"")
		}
		reverse := truthy(opt(2, "reverse", false))
		keys, m, ok := orderedMap(in)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'items'", pyClassName(in, ec.fromVar(-1)))
		}
		out := make([]any, 0, len(m))
		for _, k := range keys {
			out = append(out, []any{k, m[k]})
		}
		var sortErr error
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].([]any)[pos], out[j].([]any)[pos]
			if !caseSensitive {
				if as, ok := asString(a); ok {
					a = strings.ToLower(as)
				}
				if bs, ok := asString(b); ok {
					b = strings.ToLower(bs)
				}
			}
			if reverse {
				a, b = b, a
			}
			c, err := compare(a, b)
			if err != nil && sortErr == nil {
				sortErr = err
			}
			return c < 0
		})
		if sortErr != nil {
			return nil, sortErr
		}
		return out, nil
	}

	// Math filters (all return floats, matching ansible).
	// pow, root and log are Python's math functions, their TypeErrors
	// (and root's ValueErrors) raised as the filter's own error.
	mathArg := func(ec *EvalCtx, i int, args []any, kwargs map[string]any, name string, def any) any {
		if i < len(args) {
			return args[i]
		}
		if v, ok := kwargs[name]; ok {
			return v
		}
		return def
	}
	f["pow"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		x, err := pyReal(in, ec.fromVar(-1))
		if err == nil {
			var y float64
			if y, err = pyReal(mathArg(ec, 0, args, kwargs, "y", nil), ec.fromVar(0)); err == nil {
				return pyMathPow(x, y)
			}
		}
		if _, isType := err.(*pyTypeError); isType {
			return nil, fmt.Errorf("pow() can only be used on numbers: %s", err)
		}
		return nil, err
	}
	f["root"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		base := mathArg(ec, 0, args, kwargs, "base", int64(2))
		v, err := func() (any, error) {
			x, err := pyReal(in, ec.fromVar(-1))
			if err != nil {
				return nil, err
			}
			if equal(base, int64(2)) {
				if x < 0 {
					return nil, fmt.Errorf("expected a nonnegative input, got %s", pyFloatRepr(x))
				}
				return math.Sqrt(x), nil
			}
			b, err := pyFloat(base)
			if err != nil {
				return nil, err
			}
			return pyMathPow(x, 1/b)
		}()
		if err != nil {
			return nil, fmt.Errorf("root() can only be used on numbers: %s", err)
		}
		return v, nil
	}
	f["log"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		base := mathArg(ec, 0, args, kwargs, "base", nil)
		x, err := pyReal(in, ec.fromVar(-1))
		if err != nil {
			return nil, fmt.Errorf("log() can only be used on numbers: %s", err)
		}
		if x <= 0 || math.IsNaN(x) {
			if !math.IsNaN(x) {
				return nil, fmt.Errorf("expected a positive input")
			}
		}
		if base == nil {
			return math.Log(x), nil
		}
		if equal(base, int64(10)) {
			return math.Log10(x), nil
		}
		b, err := pyReal(base, ec.fromVar(0))
		if err != nil {
			return nil, fmt.Errorf("log() can only be used on numbers: %s", err)
		}
		if b <= 0 {
			return nil, fmt.Errorf("expected a positive input")
		}
		if b == 1 {
			return nil, fmt.Errorf("division by zero")
		}
		return math.Log(x) / math.Log(b), nil
	}

	// random is ansible-core's rand(end, start=None, step=None,
	// seed=None): randrange for an int, choice for a sequence, with
	// Python's Random (seeded, the same picks as ansible-core's).
	f["random"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		arg := func(i int, name string) any {
			if i < len(args) {
				return args[i]
			}
			return kwargs[name]
		}
		start, step := Undeprecate(arg(0, "start")), Undeprecate(arg(1, "step"))
		rng, err := newPyRandom(arg(2, "seed"))
		if err != nil {
			return nil, err
		}
		switch end := Undeprecate(in).(type) {
		case bool, int64, int, *big.Int:
			if !truthy(start) {
				start = int64(0)
			}
			if !truthy(step) {
				step = int64(1)
			}
			return pyRandrange(rng, start, end, step)
		case nil, float64:
		default:
			if !isMap(end) {
				items, err := iterate(end)
				if err != nil {
					break
				}
				if truthy(start) || truthy(step) {
					return nil, fmt.Errorf("start and step can only be used with integer values")
				}
				if len(items) == 0 {
					return nil, fmt.Errorf("Cannot choose from an empty sequence")
				}
				return items[rng.randbelowInt(len(items))], nil
			}
			keys, _, _ := orderedMap(end)
			if truthy(start) || truthy(step) {
				return nil, fmt.Errorf("start and step can only be used with integer values")
			}
			if len(keys) == 0 {
				return nil, fmt.Errorf("Cannot choose from an empty sequence")
			}
			// seq[i] on a dict: a KeyError for the int.
			return nil, fmt.Errorf("%d", rng.randbelowInt(len(keys)))
		}
		return nil, fmt.Errorf("random can only be used on sequences and integers")
	}

	f["password_hash"] = filterPasswordHash

	// strftime(timestamp): the format string is the input; the epoch seconds
	// are the argument (defaulting to now is unsupported — a timestamp must be
	// given so results stay deterministic). Rendered in local time, like
	// ansible.
	f["strftime"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		format, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("strftime() argument 1 must be str, not %s", pyClassName(in, ec.fromVar(-1)))
		}
		// second=None is now; anything else must convert with float().
		var second any
		if len(args) > 0 {
			second = args[0]
		} else if s, ok := kwargs["second"]; ok {
			second = s
		}
		ts := float64(time.Now().Unix())
		if second != nil {
			f, ok := asFloat(second)
			if s, isStr := asString(second); isStr {
				f, ok = pyParseFloat(s)
			}
			if !ok {
				return nil, whileHandling("Invalid value for epoch value (%s)", toStr(second))
			}
			ts = f
		}
		return strftime(format, int64(ts)), nil
	}

	f["human_readable"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// human_readable(size, isbits=False, unit=None)
		isbits := truthy(mathArg(ec, 0, args, kwargs, "isbits", false))
		unit := mathArg(ec, 1, args, kwargs, "unit", nil)
		out, err := bytesToHuman(Undeprecate(in), isbits, unit)
		if err != nil {
			msg := strings.ReplaceAll(err.Error(), "'str'", "'"+pyClassName(in, ec.fromVar(-1))+"'")
			return nil, fmt.Errorf("human_readable() failed on bad input: %s", msg)
		}
		return out, nil
	}
	f["human_to_bytes"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// human_to_bytes(size, default_unit=None, isbits=False)
		out, err := humanToBytes(in, mathArg(ec, 0, args, kwargs, "default_unit", nil), truthy(mathArg(ec, 1, args, kwargs, "isbits", false)))
		if err != nil {
			return nil, fmt.Errorf("human_to_bytes() can't interpret the input: %s", err)
		}
		return out, nil
	}

	// ---- serialization ----
	// to_json/to_nice_json match Python's json.dumps: "', '" item and "': '"
	// key separators (Go's encoding/json omits the spaces), keys sorted.
	// json.dumps' indent and sort_keys pass through.
	jsonOpts := func(kwargs map[string]any, indent int, sortKeys bool) (int, bool) {
		if v, ok := kwargs["indent"]; ok {
			if n, ok := asInt(v); ok {
				indent = int(n)
			}
		}
		if v, ok := kwargs["sort_keys"]; ok {
			sortKeys = truthy(v)
		}
		return indent, sortKeys
	}
	// The keyword arguments json.dumps (and the filters themselves)
	// accept; any other reaches JSONEncoder and fails there.
	jsonKwargs := func(kwargs map[string]any) error {
		for _, k := range sortedKeys(kwargs) {
			switch k {
			case "skipkeys", "ensure_ascii", "check_circular", "allow_nan", "cls", "indent", "separators",
				"default", "sort_keys", "profile", "vault_to_text", "preprocess_unsafe":
			default:
				return fmt.Errorf("JSONEncoder.__init__() got an unexpected keyword argument '%s'", k)
			}
		}
		return nil
	}
	f["to_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if err := jsonKwargs(kwargs); err != nil {
			return nil, err
		}
		// json.dumps default sort_keys=False: preserve dict insertion order.
		indent, sortKeys := jsonOpts(kwargs, 0, false)
		return pyJSON(in, indent, sortKeys), nil
	}
	f["to_nice_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if err := jsonKwargs(kwargs); err != nil {
			return nil, err
		}
		// Ansible's to_nice_json passes sort_keys=True.
		indent, sortKeys := jsonOpts(kwargs, 4, true)
		return pyJSON(in, indent, sortKeys), nil
	}
	f["from_json"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("the JSON object must be str, bytes or bytearray, not %s", pyClassName(in, false))
		}
		// json.loads(a, cls=..., **kwargs) passes the keywords to the
		// decoder.
		for _, kw := range ec.callKwargs {
			if kw == "profile" || slices.Contains(jsonDecoderKwargs, kw) {
				continue
			}
			msg := fmt.Sprintf("JSONDecoder.__init__() got an unexpected keyword argument '%s'", kw)
			if sugg := pySuggestion(append([]string{"self"}, jsonDecoderKwargs...), kw); sugg != "" {
				msg += fmt.Sprintf(". Did you mean '%s'?", sugg)
			}
			return nil, errors.New(msg)
		}
		// Objects keep their key order and numbers their type, as
		// Python's json.loads builds them.
		return omap.UnmarshalJSON([]byte(s))
	}
	f["to_yaml"] = mkToYAML(false)
	f["to_nice_yaml"] = mkToYAML(true)
	f["from_yaml"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return in, nil // anything but a str is returned as is
		}
		v, err := yaml.Unmarshal([]byte(s), "<from_yaml>")
		var ye *yaml.Error
		if errors.As(err, &ye) {
			return nil, errors.New(ye.PyYAMLString("<unicode string>"))
		}
		return v, err
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
		s, _ := softStr(in) // to_bytes(nonstring='simplerepr')
		return base64.StdEncoding.EncodeToString([]byte(s)), nil
	}
	f["b64decode"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, _ := softStr(in)
		data, err := pyB64Decode(s)
		if err != nil {
			return nil, err
		}
		return string(data), nil
	}

	// hash is get_hash(data, hashtype='sha1'): hashlib.new(hashtype) over
	// to_bytes(data), which is str(data) for anything not a string.
	f["hash"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		algo := any("sha1")
		if len(args) > 0 {
			algo = args[0]
		} else if v, ok := kwargs["hashtype"]; ok {
			algo = v
		}
		name, ok := asString(Undeprecate(algo))
		if !ok {
			return nil, whileHandling("new() argument 'name' must be str, not %s", pyClassName(algo, ec.fromVar(0)))
		}
		var h hash.Hash
		switch strings.ToLower(name) {
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
		case "sha512_224":
			h = sha512.New512_224()
		case "sha512_256":
			h = sha512.New512_256()
		case "sha3_224":
			h = sha3.New224()
		case "sha3_256":
			h = sha3.New256()
		case "sha3_384":
			h = sha3.New384()
		case "sha3_512":
			h = sha3.New512()
		default:
			return nil, whileHandling("unsupported hash type %s", name)
		}
		h.Write([]byte(pyToBytes(in)))
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	// checksum is secure_hash_s(data, hash_func=sha1): a hash_func given
	// is called.
	f["checksum"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		fn, given := kwargs["hash_func"]
		if len(args) > 0 {
			fn, given = args[0], true
		}
		if given {
			return nil, fmt.Errorf("'%s' object is not callable", pyClassName(fn, ec.fromVar(0)))
		}
		return e.Filters["hash"](ec, in, []any{"sha1"}, nil)
	}
	f["sha1"] = f["checksum"]
	// md5 is md5s(data).
	f["md5"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return e.Filters["hash"](ec, in, []any{"md5"}, nil)
	}

	f["format"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// Jinja's do_format: soft_str(value) % (kwargs or args).
		if len(args) > 0 && len(kwargs) > 0 {
			return nil, fmt.Errorf("can't handle positional and keyword arguments at the same time")
		}
		s, _ := softStr(in)
		if len(kwargs) > 0 {
			return percentFormat(in, s, []any{kwargs})
		}
		return percentFormat(in, s, args)
	}

	// quote(a): shlex.quote(to_text(a)), None as ''.
	f["quote"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			s = toStr(in)
			if Undeprecate(in) == nil {
				s = ""
			}
		}
		return shlexQuote(s), nil
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
	f["basename"] = pathFilter(pyBasename)
	f["dirname"] = pathFilter(pyDirname)
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
		if list, ok := Undeprecate(in).([]any); ok {
			for _, p := range list {
				s, ok := asString(p)
				if !ok {
					return nil, fmt.Errorf("join() argument must be str, bytes, or os.PathLike object, not '%s'", pyClassName(p, ec.fromVar(-1)))
				}
				parts = append(parts, s)
			}
		} else if s, ok := asString(in); ok {
			parts = append(parts, s)
		} else {
			return nil, fmt.Errorf("|path_join expects string or sequence, got %s instead.", pyTypeRepr(in, ec.fromVar(-1)))
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
		// ternary(value, true_val, false_val, none_val=None)
		params := []string{"true_val", "false_val", "none_val"}
		vals := make([]any, 3)
		set := make([]bool, 3)
		for i := range params {
			if i < len(args) {
				vals[i], set[i] = args[i], true
			} else if v, ok := kwargs[params[i]]; ok {
				vals[i], set[i] = v, true
			}
		}
		var missing []string
		for i := range 2 {
			if !set[i] {
				missing = append(missing, "'"+params[i]+"'")
			}
		}
		switch len(missing) {
		case 1:
			return nil, fmt.Errorf("ternary() missing 1 required positional argument: %s", missing[0])
		case 2:
			return nil, fmt.Errorf("ternary() missing 2 required positional arguments: %s and %s", missing[0], missing[1])
		}
		if Undeprecate(in) == nil && vals[2] != nil {
			return vals[2], nil
		}
		if truthy(in) {
			return vals[0], nil
		}
		return vals[1], nil
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
		if n, ok := asInt(in); ok && n != math.MinInt64 {
			if n < 0 {
				return -n, nil
			}
			return n, nil
		}
		if b, ok := asBigInt(in); ok {
			return normInt(new(big.Int).Abs(b)), nil
		}
		if fv, ok := Undeprecate(in).(float64); ok {
			return math.Abs(fv), nil
		}
		return nil, fmt.Errorf("bad operand type for abs(): '%s'", pyClassName(in, ec.fromVar(-1)))
	}

	f["round"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
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
		if method != "common" && method != "ceil" && method != "floor" {
			return nil, fmt.Errorf("method must be common, ceil or floor")
		}
		fv, ok := asFloat(in)
		if !ok {
			return nil, fmt.Errorf("type %s doesn't define __round__ method", pyClassName(in, ec.fromVar(-1)))
		}
		if _, isInt := asBigInt(in); isInt && method == "common" && precision >= 0 {
			if _, isBool := Undeprecate(in).(bool); !isBool {
				return Undeprecate(in), nil // round(int, n) is the int
			}
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

// seqReduce is Jinja's do_min/do_max (op "<" or ">"): Python's min/max
// over the items, keyed by attribute= and compared case-insensitively
// unless case_sensitive.
func seqReduce(op string) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, errNotIterable(in, ec.fromVar(-1))
		}
		if len(items) == 0 {
			return Undefined{Name: "aggregated item", Err: &UndefinedError{Hint: "No aggregated item, sequence was empty."}}, nil
		}
		// With attribute=, items compare by it, and the item wins (Jinja's
		// key function).
		keys := items
		if attr, ok := kwargs["attribute"]; ok {
			keys, err = extractAll(items, attr)
			if err != nil {
				return nil, err
			}
		}
		caseSensitive := len(args) > 0 && truthy(args[0]) || truthy(kwargs["case_sensitive"])
		key := func(v any) any {
			if s, ok := asString(v); ok && !caseSensitive {
				return strings.ToLower(s)
			}
			return v
		}
		best := 0
		for i := 1; i < len(items); i++ {
			c, err := compareOp(key(keys[i]), key(keys[best]), op)
			if err != nil {
				return nil, err
			}
			if (op == "<" && c < 0) || (op == ">" && c > 0) {
				best = i
			}
		}
		return items[best], nil
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
	if n, isInt := attr.(int64); isInt && !ok {
		// An int attribute is one item lookup.
		if lst, isList := Undeprecate(item).([]any); isList {
			i := n
			if i < 0 {
				i += int64(len(lst))
			}
			if i >= 0 && i < int64(len(lst)) {
				return lst[i], nil
			}
		}
		return Undefined{Name: fmt.Sprintf("%s[%d]", describeOwner(item), n)}, nil
	}
	if !ok {
		return nil, fmt.Errorf("attribute name must be a string")
	}
	cur := item
	// Jinja's make_attrgetter: a part missing from its container is an
	// undefined naming that container ("object of type 'dict' has no
	// attribute 'x'"), and stays so whatever follows.
	for _, seg := range strings.Split(path, ".") {
		if m, ok := anyToMap(cur); ok {
			next, ok := m[seg]
			if !ok {
				return Undefined{Name: describeOwner(cur) + "." + seg}, nil
			}
			cur = next
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
					return Undefined{Name: describeOwner(cur) + "[" + seg + "]"}, nil
				}
				cur = lst[idx]
				continue
			}
		}
		return Undefined{Name: describeOwner(cur) + "." + seg}, nil
	}
	return cur, nil
}

func extractAll(items []any, attr any) ([]any, error) {
	out := make([]any, len(items))
	for i, item := range items {
		v, err := extractStrict(item, attr)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// extractStrict is extractAttr for a filter that uses the value: an
// undefined one raises.
func extractStrict(item, attr any) (any, error) {
	v, err := extractAttr(item, attr)
	if u, ok := v.(Undefined); ok && err == nil {
		return nil, u.useError(Position{})
	}
	return v, err
}

// filterMap implements map('filtername', args...) and map(attribute=...).
func filterMap(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	items, err := iterate(in)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		// Jinja prepares the mapping only for a non-empty sequence.
		return []any{}, nil
	}
	if attr, ok := kwargs["attribute"]; ok && len(args) == 0 {
		for k := range kwargs {
			if k != "attribute" && k != "default" {
				return nil, fmt.Errorf("Unexpected keyword argument %s", pyStrRepr(k))
			}
		}
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
					return nil, u.useError(ec.pos)
				}
			}
			out[i] = v
		}
		return out, nil
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("map requires a filter argument")
	}
	name, ok := asString(args[0])
	if !ok {
		return nil, pluginLoadError("filter", args[0])
	}
	fn, ok := ec.engine.Filters[pluginShortName(name)]
	if !ok {
		return nil, fmt.Errorf("No filter named %s.", pyStrRepr(name))
	}
	if err := checkArity(false, pluginShortName(name), name, len(args)-1, kwargsInOrder(ec.callKwargs)); err != nil {
		return nil, ec.pluginError("filter", name, err)
	}
	out := make([]any, len(items))
	for i, item := range items {
		if u, isU := item.(Undefined); isU && !undefinedTolerantFilters[pluginShortName(name)] {
			return nil, u.useError(ec.pos)
		}
		v, err := fn(ec, item, args[1:], kwargs)
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
		if len(items) == 0 {
			// Jinja prepares the test only for a non-empty sequence.
			return []any{}, nil
		}
		var attr any
		if byAttr {
			if len(args) == 0 {
				return nil, fmt.Errorf("Missing parameter for attribute name")
			}
			attr = args[0]
			args = args[1:]
		}
		var test TestFunc
		var testArgs []any
		if len(args) > 0 {
			name, ok := asString(args[0])
			if !ok {
				return nil, pluginLoadError("test", args[0])
			}
			test, ok = ec.engine.Tests[pluginShortName(name)]
			if !ok {
				return nil, fmt.Errorf("No test named %s.", pyStrRepr(name))
			}
			testArgs = args[1:]
		}
		var out []any
		for _, item := range items {
			subject := item
			if u, und := item.(Undefined); und && byAttr {
				// An item that is a marker trips reading its attribute.
				return nil, u.useError(ec.pos)
			}
			if byAttr {
				subject, err = extractAttr(item, attr)
				if err != nil {
					return nil, err
				}
			}
			// An undefined subject raises unless the test takes one.
			if u, und := subject.(Undefined); und && (test == nil || !undefinedTolerantTests[pluginShortName(toStr(args[0]))]) {
				return nil, u.useError(ec.pos)
			}
			var keep bool
			if test != nil {
				keep, err = test(ec, subject, testArgs)
				if err != nil {
					return nil, err
				}
			} else {
				keep = truthy(subject)
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

// strftime formats epoch seconds using Python strftime codes, in local time
// (matching ansible's strftime filter). Unknown codes pass through literally.
func strftime(format string, ts int64) string {
	t := time.Unix(ts, 0)
	return strftimeTime(format, t, t.Location(), false)
}

// sizeRanges is ansible's formatters.SIZE_RANGES, largest first.
var sizeRanges = []struct {
	suffix string
	limit  *big.Int
}{
	{"Y", new(big.Int).Lsh(big.NewInt(1), 80)}, {"Z", new(big.Int).Lsh(big.NewInt(1), 70)},
	{"E", big.NewInt(1 << 60)}, {"P", big.NewInt(1 << 50)}, {"T", big.NewInt(1 << 40)},
	{"G", big.NewInt(1 << 30)}, {"M", big.NewInt(1 << 20)}, {"K", big.NewInt(1 << 10)}, {"B", big.NewInt(1)},
}

// bytesToHuman is formatters.bytes_to_human: "%.2f <unit>" of size over
// the largest range it reaches (or the given unit's).
func bytesToHuman(size any, isbits bool, unit any) (string, error) {
	base := "Bytes"
	if isbits {
		base = "bits"
	}
	var suffix string
	var limit *big.Int
	for _, r := range sizeRanges {
		suffix, limit = r.suffix, r.limit
		if unit == nil {
			c, err := compareOp(size, normInt(limit), ">=")
			if err != nil {
				return "", err
			}
			if c >= 0 {
				break
			}
		} else if strings.ToUpper(toStr(unit)) == suffix[:1] {
			break
		}
	}
	if limit.Cmp(big.NewInt(1)) != 0 {
		suffix += base[:1]
	} else {
		suffix = base
	}
	q, err := numArith(tokDiv, size, normInt(limit))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%.2f %s", q, suffix), nil
}

var humanBytesRe = regexp.MustCompile(`^([0-9]*\.?[0-9]+)(?:\s*([A-Za-z]+))?\s*$`)

// humanToBytes is formatters.human_to_bytes.
func humanToBytes(number any, defaultUnit any, isbits bool) (any, error) {
	s := toStr(number)
	m := humanBytesRe.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("human_to_bytes() can't interpret following string: %s", s)
	}
	num, _ := strconv.ParseFloat(m[1], 64)
	unit := m[2]
	if unit == "" && defaultUnit != nil {
		unit = toStr(defaultUnit)
	}
	if unit == "" {
		v, _ := floatToInt(math.RoundToEven(num))
		return v, nil
	}
	rangeKey := strings.ToUpper(unit[:1])
	var limit *big.Int
	for _, r := range sizeRanges {
		if r.suffix == rangeKey {
			limit = r.limit
		}
	}
	if limit == nil {
		return nil, fmt.Errorf("human_to_bytes() failed to convert %s (unit = %s). The suffix must be one of Y, Z, E, P, T, G, M, K, B", s, unit)
	}
	unitClass, unitClassName := "B", "byte"
	if isbits {
		unitClass, unitClassName = "b", "bit"
	}
	if len(unit) > 1 {
		expect := fmt.Sprintf("expect %s%s or %s", rangeKey, unitClass, rangeKey)
		if rangeKey == "B" {
			expect = fmt.Sprintf("expect %s or %s", unitClass, unitClassName)
		}
		if !strings.Contains(strings.ToLower(unit), unitClassName) && unit[1:2] != unitClass {
			return nil, fmt.Errorf("human_to_bytes() failed to convert %s. Value is not a valid string (%s)", s, expect)
		}
	}
	f, _ := new(big.Float).SetInt(limit).Float64()
	v, _ := floatToInt(math.RoundToEven(num * f))
	return v, nil
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

// mkToYAML builds to_yaml (nice=false) and to_nice_yaml (nice=true):
// ansible-core's yaml.dump(a, Dumper=AnsibleDumper, allow_unicode=True,
// default_flow_style=...) with the caller's yaml.dump keyword arguments.
// to_yaml leaves default_flow_style None (collections of scalars go
// inline); to_nice_yaml defaults to indent=4 (its first positional
// argument) and block style.
func mkToYAML(nice bool) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		opts := yaml.DumpOptions{SortKeys: true, AllowUnicode: true, Normalize: yamlDumpValue}
		if nice {
			opts.Indent = 4
			block := false
			opts.DefaultFlowStyle = &block
			if len(args) > 0 {
				kwargs = withKwarg(kwargs, "indent", args[0])
			}
		}
		for _, k := range sortedKeys(kwargs) {
			v := Undeprecate(kwargs[k])
			var err error
			switch k {
			case "indent":
				err = yamlIntArg(v, &opts.Indent)
			case "width":
				err = yamlIntArg(v, &opts.Width)
			case "default_flow_style":
				if v == nil {
					opts.DefaultFlowStyle = nil
				} else {
					b := truthy(v)
					opts.DefaultFlowStyle = &b
				}
			case "sort_keys":
				opts.SortKeys = truthy(v)
			case "canonical":
				opts.Canonical = v != nil && truthy(v)
			case "explicit_start":
				opts.ExplicitStart = v != nil && truthy(v)
			case "explicit_end":
				opts.ExplicitEnd = v != nil && truthy(v)
			case "default_style":
				opts.DefaultStyle = 0
				if s, ok := asString(v); ok && len(s) == 1 && strings.Contains(`'"|>`, s) {
					opts.DefaultStyle = s[0]
				} else if v != nil && truthy(v) {
					opts.DefaultStyle = 'p' // a style PyYAML emits as plain, but not "no style"
				}
			case "line_break":
				opts.LineBreak = ""
				if s, ok := asString(v); ok && (s == "\r" || s == "\n" || s == "\r\n") {
					opts.LineBreak = s
				}
			case "encoding":
				// The dump comes back as bytes, which render as the same text.
			case "vault_behavior":
				// Vaulted values arrive decrypted, as the default "decrypt" leaves them.
				if s, _ := asString(v); v != nil && truthy(v) &&
					s != "decrypt" && s != "keep_encrypted" && s != "redact" && s != "fail" {
					return nil, fmt.Errorf("The vault parameter must be one of decrypt, keep_encrypted, redact, fail")
				}
			case "allow_unicode":
				return nil, fmt.Errorf("yaml.dump() got multiple values for keyword argument 'allow_unicode'")
			default:
				return nil, fmt.Errorf("dump_all() got an unexpected keyword argument '%s'", k)
			}
			if err != nil {
				return nil, err
			}
		}
		return yaml.Dump(in, opts)
	}
}

// withKwarg returns kwargs with key set (a copy; the caller's map is left
// alone).
func withKwarg(kwargs map[string]any, key string, v any) map[string]any {
	out := make(map[string]any, len(kwargs)+1)
	for k, val := range kwargs {
		out[k] = val
	}
	out[key] = v
	return out
}

// yamlIntArg converts an indent/width argument as the C emitter's int
// parameters do: None leaves the default, bools and floats truncate to
// ints, anything else is a TypeError.
func yamlIntArg(v any, dst *int) error {
	switch t := v.(type) {
	case nil:
		return nil
	case float64:
		*dst = int(t)
		return nil
	}
	if n, ok := asInt(v); ok {
		*dst = int(n)
		return nil
	}
	return fmt.Errorf("an integer is required")
}

// yamlDumpValue unwraps engine values for yaml.Dump, leaving containers
// that are shared by reference as they are so the dump aliases them as
// PyYAML does.
func yamlDumpValue(v any) any {
	switch t := v.(type) {
	case Deprecated:
		return yamlDumpValue(t.Value)
	case *yaml.OMap:
		return t
	case Mapping:
		m := yaml.NewOMap()
		for _, k := range t.Keys() {
			val, _ := t.GetItem(k)
			m.Set(k, val)
		}
		return m
	case pyTime, *pyTZ, pyTimedelta:
		return yamlUnrepresentable{pyRepr(t)}
	case *rangeValue:
		repr := fmt.Sprintf("range(%d, %d)", t.start, t.stop)
		if t.step != 1 {
			repr = fmt.Sprintf("range(%d, %d, %d)", t.start, t.stop, t.step)
		}
		return yamlUnrepresentable{repr}
	}
	return v
}

// yamlUnrepresentable is a value PyYAML's representer rejects.
type yamlUnrepresentable struct{ repr string }

func (u yamlUnrepresentable) PyRepr() string { return u.repr }

func pathFilter(fn func(string) string) FilterFunc {
	return func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyClassName(in, ec.fromVar(-1)))
		}
		return fn(s), nil
	}
}

// pyBasename is os.path.basename.
func pyBasename(p string) string {
	return p[strings.LastIndexByte(p, '/')+1:]
}

// pyDirname is os.path.dirname.
func pyDirname(p string) string {
	head := p[:strings.LastIndexByte(p, '/')+1]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// pyJSON serializes a value the way Python's json.dumps does — the format
// Ansible's to_json/to_nice_json produce (json.dumps' default
// ensure_ascii=True). indent==0 yields the compact form with ", " and ": "
// separators; indent>0 pretty-prints. sortKeys mirrors json.dumps'
// sort_keys: to_json passes false (dict insertion order kept), to_nice_json
// passes true. Plain Go maps have no inherent order and are always sorted;
// *OMap/Mapping honor sortKeys.
func pyJSON(v any, indent int, sortKeys bool) string {
	return PyJSON(v, indent, sortKeys, true)
}

// PyJSON is Python's json.dumps(v, indent=indent or None,
// sort_keys=sortKeys, ensure_ascii=ensureASCII) over engine values. The
// output callback uses it with ensureASCII=false, as Ansible's does.
func PyJSON(v any, indent int, sortKeys, ensureASCII bool) string {
	e := pyJSONEncoder{indent: indent, sortKeys: sortKeys, ensureASCII: ensureASCII}
	e.write(v, 0)
	return e.b.String()
}

// Plain converts engine-internal values into plain JSON-shaped Go values
// without a lossy encoding/json round trip.
func Plain(v any) any { return jsonSanitize(v) }

// PyStr renders a value like Python's str() (what "%s" and loop labels show).
func PyStr(v any) string { return toStr(v) }

// PyRepr is Python repr() of a value.
func PyRepr(v any) string { return pyRepr(v) }

type pyJSONEncoder struct {
	b           strings.Builder
	indent      int
	sortKeys    bool
	ensureASCII bool
	// pretty is json.dumps given an indent, even 0 (newlines, no
	// spaces); indentStr is a str indent.
	pretty    bool
	indentStr string
}

// newline starts a line at depth when the encoder pretty-prints.
func (e *pyJSONEncoder) newline(depth int) {
	if e.indent > 0 || e.pretty {
		e.b.WriteByte('\n')
		if e.indentStr != "" {
			e.b.WriteString(strings.Repeat(e.indentStr, depth))
		} else {
			e.b.WriteString(strings.Repeat(" ", e.indent*depth))
		}
	}
}

func (e *pyJSONEncoder) write(v any, depth int) {
	b := &e.b
	switch t := v.(type) {
	case Deprecated:
		e.write(t.Value, depth)
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(pyJSONQuote(t, e.ensureASCII))
	case yaml.UnsafeString:
		b.WriteString(pyJSONQuote(string(t), e.ensureASCII))
	case *big.Int:
		b.WriteString(t.String())
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case int:
		b.WriteString(strconv.Itoa(t))
	case float64:
		b.WriteString(pyJSONFloat(t))
	case *rangeValue:
		e.write(t.materialize(), depth)
	case pyDatetime:
		b.WriteString(pyJSONQuote(t.Isoformat("T"), e.ensureASCII))
	case pyDate:
		b.WriteString(pyJSONQuote(t.Isoformat(), e.ensureASCII))
	case pyTime:
		b.WriteString(pyJSONQuote(t.Isoformat(), e.ensureASCII))
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, item := range t {
			e.sep(i, depth+1)
			e.write(item, depth+1)
		}
		e.close(']', depth)
	case map[string]any:
		e.object(sortedKeys(t), func(k string) any { return t[k] }, len(t), depth)
	case Mapping:
		keys := t.Keys()
		if e.sortKeys {
			keys = append([]string(nil), keys...)
			sort.Strings(keys)
		}
		e.object(keys, func(k string) any { v, _ := t.GetItem(k); return v }, t.Len(), depth)
	default:
		b.WriteString(pyJSONQuote(toStr(v), e.ensureASCII))
	}
}

// object renders a JSON object given keys in the desired order and a value
// accessor. Plain-map keys arrive pre-sorted; Mapping keys arrive in the
// order chosen by the caller (insertion or sorted).
func (e *pyJSONEncoder) object(keys []string, get func(string) any, n, depth int) {
	if n == 0 {
		e.b.WriteString("{}")
		return
	}
	e.b.WriteByte('{')
	for i, k := range keys {
		e.sep(i, depth+1)
		e.b.WriteString(pyJSONQuote(k, e.ensureASCII))
		e.b.WriteString(": ")
		e.write(get(k), depth+1)
	}
	e.close('}', depth)
}

func (e *pyJSONEncoder) sep(i, depth int) {
	if i > 0 {
		if e.indent > 0 || e.pretty {
			e.b.WriteByte(',')
		} else {
			e.b.WriteString(", ")
		}
	}
	e.newline(depth)
}

func (e *pyJSONEncoder) close(closer byte, depth int) {
	e.newline(depth)
	e.b.WriteByte(closer)
}

// pyJSONFloat is Python's float repr as json.dumps emits it (inf/nan become
// Infinity/NaN, which json.dumps allows by default).
func pyJSONFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	return pyFloatStr(f)
}

// pyJSONQuote quotes a string exactly as Python's json encoder does: only
// '"', '\\', and control characters are escaped (\b \f \n \r \t by name,
// others as \u00XX) — unlike encoding/json, never '<', '>', '&', U+2028 or
// U+2029. With ensureASCII every non-ASCII rune is \uXXXX-escaped, using
// UTF-16 surrogate pairs above the BMP.
func pyJSONQuote(s string, ensureASCII bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || (ensureASCII && r == 0x7f):
				fmt.Fprintf(&b, `\u%04x`, r)
			case ensureASCII && r > 0x7f:
				if r > 0xffff {
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsonSanitize converts engine-internal values into plain JSON-encodable
// values (UnsafeString -> string, Mapping -> map, lazy range -> list).
func jsonSanitize(v any) any {
	switch t := v.(type) {
	case Deprecated:
		return jsonSanitize(t.Value)
	case yaml.UnsafeString:
		return string(t)
	case Markup:
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

// jsonDecoderKwargs are json.JSONDecoder's keyword arguments.
var jsonDecoderKwargs = []string{"object_hook", "parse_float", "parse_int", "parse_constant", "strict", "object_pairs_hook"}

// pluginLoadError is ansible-core's failure loading a filter or test
// named by something not a string (map(1), select(none)).
func pluginLoadError(kind string, name any) error {
	return fmt.Errorf("The %s plugin %s failed to load: '%s' object has no attribute 'removeprefix'", kind, pyRepr(name), pyClassName(name, false))
}

// kwargsInOrder are keyword arguments by name, for checkArity.
func kwargsInOrder(names []string) []kwarg {
	out := make([]kwarg, len(names))
	for i, n := range names {
		out[i] = kwarg{name: n}
	}
	return out
}

// shlexQuote is Python's shlex.quote: a string of only safe characters
// (ASCII \w and @%+=:,./-) as it is, else single-quoted.
func shlexQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
