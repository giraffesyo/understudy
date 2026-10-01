package template

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/giraffesyo/understudy/internal/yaml"
)

// registerFilters installs the M2 core filter set. The full ~55-filter
// Ansible set lands in M4; these are the ones the walking skeleton and
// early playbooks need.
func registerFilters(e *Engine) {
	f := e.Filters

	f["default"] = filterDefault
	f["d"] = filterDefault

	f["mandatory"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if u, ok := in.(Undefined); ok {
			if len(args) > 0 && args[0] != nil {
				return nil, fmt.Errorf("%s", toStr(args[0]))
			}
			if m, ok := kwargs["msg"]; ok && m != nil {
				return nil, fmt.Errorf("%s", toStr(m))
			}
			// The undefined's name: the variable, or the attribute that
			// was missing.
			name := u.Name
			if i := strings.Index(name, " object"); i > 0 && !strings.ContainsAny(name[:i], ".[ ") {
				name = strings.TrimLeft(name[i+len(" object"):], ".[")
				if j := strings.IndexAny(name, ".["); j >= 0 {
					name = name[:j]
				}
				name = strings.TrimSuffix(name, "]")
			} else if j := strings.IndexAny(name, ".["); j > 0 {
				name = name[:j]
			}
			if name == "" {
				return nil, fmt.Errorf("Mandatory variable not defined.")
			}
			return nil, fmt.Errorf("Mandatory variable %s not defined.", pyStrRepr(name))
		}
		return in, nil
	}

	f["bool"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		switch t := in.(type) {
		case bool:
			return t, nil
		case string:
			switch strings.ToLower(t) {
			case "yes", "on", "1", "true":
				return true, nil
			}
			return false, nil
		case yaml.UnsafeString:
			return f["bool"](ec, string(t), args, kwargs)
		}
		if n, ok := asFloat(in); ok {
			return n == 1, nil
		}
		return false, nil
	}

	f["int"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// Jinja's do_int(value, default=0, base=10): int(value) (with the
		// base for a str), else int(float(value)), else the default as
		// given. Ints have no size limit.
		var def any = int64(0)
		if len(args) > 0 {
			def = args[0]
		} else if d, ok := kwargs["default"]; ok {
			def = d
		}
		base := int64(10)
		if len(args) > 1 {
			if bi, ok := asInt(args[1]); ok {
				base = bi
			}
		}
		if b, ok := kwargs["base"]; ok {
			if bi, ok := asInt(b); ok {
				base = bi
			}
		}
		in = Undeprecate(in)
		if s, ok := asString(in); ok {
			if v, ok := pyParseInt(s, base); ok {
				return v, nil
			}
			if f, ok := pyParseFloat(s); ok {
				if v, ok := floatToInt(f); ok {
					return v, nil
				}
			}
			return def, nil
		}
		switch t := in.(type) {
		case int64, *big.Int:
			return t, nil
		case int:
			return int64(t), nil
		case bool:
			if t {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			if v, ok := floatToInt(t); ok {
				return v, nil
			}
		}
		return def, nil
	}

	f["float"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// Jinja's do_float(value, default=0.0): float(value), else the
		// default as given.
		var def any = 0.0
		if len(args) > 0 {
			def = args[0]
		} else if d, ok := kwargs["default"]; ok {
			def = d
		}
		in = Undeprecate(in)
		if b, ok := in.(*big.Int); ok {
			return bigToFloat(b) // OverflowError is not caught
		}
		if v, ok := asFloat(in); ok {
			return v, nil
		}
		if s, ok := asString(in); ok {
			if v, ok := pyParseFloat(s); ok {
				return v, nil
			}
		}
		return def, nil
	}

	f["string"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		if _, ok := asString(in); ok {
			return in, nil
		}
		return toStr(in), nil
	}

	lengthFilter := func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		n, err := length(in)
		if err != nil {
			return nil, err
		}
		return int64(n), nil
	}
	f["length"] = lengthFilter
	f["count"] = lengthFilter

	f["upper"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		return strings.ToUpper(s), nil
	}
	f["lower"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		return strings.ToLower(s), nil
	}
	f["capitalize"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		return pyCapitalize(s), nil
	}
	f["title"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		return pyTitle(s), nil
	}
	f["trim"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		return strings.TrimSpace(s), nil
	}

	f["replace"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		old, err := argSoftStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		niu, err := argSoftStr(args, 1, "")
		if err != nil {
			return nil, err
		}
		count, err := argInt(args, 2, -1)
		if err != nil {
			return nil, err
		}
		return strings.Replace(s, old, niu, int(count)), nil
	}

	f["join"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		sep, err := argSoftStr(args, 0, "")
		if err != nil {
			return nil, err
		}
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = toStr(item)
		}
		return strings.Join(parts, sep), nil
	}

	f["split"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		// The filter is str.split itself.
		s, ok := asString(in)
		if !ok {
			return nil, fmt.Errorf("descriptor 'split' for 'str' objects doesn't apply to a '%s' object", pyClassName(in, ec.fromVar(-1)))
		}
		if len(args) > 0 && args[0] != nil {
			if _, ok := asString(args[0]); !ok {
				return nil, fmt.Errorf("must be str or None, not %s", pyClassName(args[0], ec.fromVar(0)))
			}
		}
		return strMethods["split"](s, args)
	}

	f["first"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return Undefined{Name: "first item", Err: &UndefinedError{Hint: "No first item, sequence was empty."}}, nil
		}
		return items[0], nil
	}
	f["last"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return Undefined{Name: "last item", Err: &UndefinedError{Hint: "No last item, sequence was empty."}}, nil
		}
		return items[len(items)-1], nil
	}

	f["list"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return iterate(in)
	}

	f["type_debug"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		return pyTypeName(in), nil
	}

	// center(width=80): pad a string with spaces so it is centered in width.
	f["center"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		width, err := argInt(args, 0, 80)
		if err != nil {
			return nil, err
		}
		n := int64(len([]rune(s)))
		if n >= width {
			return s, nil
		}
		total := int(width - n)
		left := total / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", total-left), nil
	}

	// batch(n, fill_with=None): group a sequence into lists of n, padding the
	// last group with fill_with when given.
	f["batch"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		n, err := argInt(args, 0, 1)
		if err != nil {
			// len(tmp) == linecount never holds: one batch.
			n = int64(len(items))
		}
		if n < 1 {
			n = 1
		}
		fill, hasFill := filterArg(args, 1, kwargs, "fill_with")
		out := []any{}
		for i := 0; i < len(items); i += int(n) {
			end := i + int(n)
			if end > len(items) {
				end = len(items)
			}
			chunk := append([]any{}, items[i:end]...)
			if hasFill {
				for len(chunk) < int(n) {
					chunk = append(chunk, fill)
				}
			}
			out = append(out, chunk)
		}
		return out, nil
	}

	// slice(n, fill_with=None): divide a sequence into n groups as evenly as
	// possible; the first (len % n) groups get one extra item (Jinja2 order).
	f["slice"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		n, err := argInt(args, 0, 1)
		if err != nil {
			return nil, fmt.Errorf("unsupported operand type(s) for //: 'int' and '%s'", pyClassName(args[0], ec.fromVar(0)))
		}
		if n < 1 {
			n = 1
		}
		items, err := iterate(in)
		if err != nil {
			return nil, err
		}
		fill, hasFill := filterArg(args, 1, kwargs, "fill_with")
		slices := int(n)
		perSlice := len(items) / slices
		withExtra := len(items) % slices
		out := make([]any, 0, slices)
		offset := 0
		for s := 0; s < slices; s++ {
			start := offset + s*perSlice
			if s < withExtra {
				offset++
			}
			end := offset + (s+1)*perSlice
			if start > len(items) {
				start = len(items)
			}
			if end > len(items) {
				end = len(items)
			}
			group := append([]any{}, items[start:end]...)
			if hasFill && s >= withExtra {
				group = append(group, fill)
			}
			out = append(out, group)
		}
		return out, nil
	}

	// truncate(length=255, killwords=False, end='...', leeway=5): shorten a
	// string, matching Jinja2's word-aware truncation.
	f["truncate"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		length, err := argInt(args, 0, 255)
		if err != nil {
			return nil, fmt.Errorf("'>=' not supported between instances of '%s' and 'int'", pyClassName(args[0], ec.fromVar(0)))
		}
		killwords := argBoolAt(args, 1, kwargs, "killwords", false)
		end, err := argStrKw(args, 2, kwargs, "end", "...")
		if err != nil {
			return nil, err
		}
		leeway, err := argIntKw(args, 3, kwargs, "leeway", 5)
		if err != nil {
			return nil, err
		}
		runes := []rune(s)
		if int64(len(runes)) <= length+leeway {
			return s, nil
		}
		cut := int(length) - len([]rune(end))
		if cut < 0 {
			cut = 0
		}
		head := string(runes[:cut])
		if !killwords {
			if idx := strings.LastIndex(head, " "); idx >= 0 {
				head = head[:idx]
			}
		}
		return head + end, nil
	}

	// wordwrap(width=79, break_long_words=True, wrapstring="\n"): greedy word
	// wrap matching textwrap for the common cases.
	f["wordwrap"] = func(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
		s, err := softStr(in)
		if err != nil {
			return nil, err
		}
		width, err := argInt(args, 0, 79)
		if err != nil {
			return nil, fmt.Errorf("'<=' not supported between instances of '%s' and 'int'", pyClassName(args[0], ec.fromVar(0)))
		}
		if width < 1 {
			width = 1
		}
		breakLong := argBoolAt(args, 1, kwargs, "break_long_words", true)
		wrapstring, err := argStrKw(args, 2, kwargs, "wrapstring", "\n")
		if err != nil {
			return nil, err
		}
		return wordWrap(s, int(width), breakLong, wrapstring), nil
	}
}

// filterArg returns a positional (index i) or keyword filter argument and
// whether it was supplied.
func filterArg(args []any, i int, kwargs map[string]any, name string) (any, bool) {
	if v, ok := kwargs[name]; ok {
		return v, true
	}
	if i < len(args) {
		return args[i], true
	}
	return nil, false
}

func argBoolAt(args []any, i int, kwargs map[string]any, name string, def bool) bool {
	if v, ok := filterArg(args, i, kwargs, name); ok {
		return truthy(v)
	}
	return def
}

func argStrKw(args []any, i int, kwargs map[string]any, name, def string) (string, error) {
	if v, ok := filterArg(args, i, kwargs, name); ok {
		s, ok := asString(v)
		if !ok {
			return "", fmt.Errorf("%s must be a string", name)
		}
		return s, nil
	}
	return def, nil
}

func argIntKw(args []any, i int, kwargs map[string]any, name string, def int64) (int64, error) {
	if v, ok := filterArg(args, i, kwargs, name); ok {
		n, ok := asInt(v)
		if !ok {
			return 0, fmt.Errorf("%s must be an integer", name)
		}
		return n, nil
	}
	return def, nil
}

// wordWrap greedily wraps text to width, breaking on spaces and (when
// breakLong is set) splitting words longer than width. Existing newlines in
// the input are preserved as paragraph breaks.
func wordWrap(s string, width int, breakLong bool, wrapstring string) string {
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		cur := ""
		flush := func() {
			lines = append(lines, cur)
			cur = ""
		}
		for _, w := range words {
			for breakLong && len([]rune(w)) > width {
				space := width - len([]rune(cur))
				if cur != "" {
					space--
				}
				if space <= 0 {
					flush()
					continue
				}
				wr := []rune(w)
				if cur != "" {
					cur += " "
				}
				cur += string(wr[:space])
				w = string(wr[space:])
				flush()
			}
			switch {
			case cur == "":
				cur = w
			case len([]rune(cur))+1+len([]rune(w)) <= width:
				cur += " " + w
			default:
				flush()
				cur = w
			}
		}
		if cur != "" {
			flush()
		}
	}
	return strings.Join(lines, wrapstring)
}

// filterDefault implements default/d: replace Undefined (or, with the
// second arg true, any falsy value).
func filterDefault(ec *EvalCtx, in any, args []any, kwargs map[string]any) (any, error) {
	if len(args) == 0 {
		if isUndefined(in) {
			return "", nil
		}
		return in, nil
	}
	useOnFalsy := false
	if len(args) > 1 {
		useOnFalsy = truthy(args[1])
	}
	if isUndefined(in) || (useOnFalsy && !truthy(in)) {
		return args[0], nil
	}
	return in, nil
}

// argSoftStr is argument i as str() makes it (def when absent).
func argSoftStr(args []any, i int, def string) (string, error) {
	if i >= len(args) {
		return def, nil
	}
	return softStr(args[i])
}

// softStr is Jinja's soft_str: a string filter works on str() of any
// value.
func softStr(in any) (string, error) {
	if s, ok := asString(in); ok {
		return s, nil
	}
	return toStr(in), nil
}

// pyTypeName matches Ansible's type_debug output (Python type names).
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int64, int, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case yaml.UnsafeString:
		return "AnsibleUnsafeText"
	case []any:
		return "list"
	case map[string]any, Mapping:
		return "dict"
	case Undefined:
		return "AnsibleUndefined"
	}
	return fmt.Sprintf("%T", v)
}
