package modules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Local facts: facts.d (module_utils/facts/system/local.py).

func collectLocal(e *factEnv, _ map[string]any) map[string]any {
	local := map[string]any{}
	out := map[string]any{"local": local}
	factPath := e.factPath
	if factPath == "" || !e.exists(factPath) {
		return out
	}
	// glob.glob(fact_path + '/*.fact'), sorted.
	names, err := e.listDir(factPath)
	if err != nil {
		return out
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".fact") {
			continue
		}
		fn := factPath + "/" + n
		base := strings.ReplaceAll(n, ".fact", "")
		st, err := os.Stat(e.p(fn))
		if err != nil {
			msg := fmt.Sprintf("Could not stat fact (%s): %s", fn, factErrText(err))
			local[base] = msg
			e.warn(msg)
			continue
		}
		var content string
		if st.Mode()&0o100 != 0 {
			rc, stdout, stderr := e.run(e.p(fn))
			var failed string
			if rc == -1 {
				failed = fmt.Sprintf("Could not execute fact script (%s): %s", fn, stderr)
			} else if rc != 0 {
				failed = fmt.Sprintf("Failure executing fact script (%s), rc: %d, err: %s", fn, rc, stderr)
			}
			if failed != "" {
				local[base] = failed
				e.warn(failed)
				continue
			}
			content = stdout
		} else {
			content = e.fileContentOr(fn, "")
		}
		if !utf8.ValidString(content) {
			msg := fmt.Sprintf(`error loading fact - output of running "%s" was not utf-8`, fn)
			local[base] = msg
			e.warn(msg)
			continue
		}
		var fact any
		if v, ok := decodeJSONStrict([]byte(content)); ok {
			fact = v
		} else {
			ini, err := parseINI(content)
			if err != nil {
				fact = "error loading facts as JSON or ini - please check content: " + fn
				e.warn(fact.(string))
			} else if m, ierr := ini.toFacts(); ierr != nil {
				fact = fmt.Sprintf("Failed to convert (%s) to JSON: %s", fn, ierr)
				e.warn(fact.(string))
			} else {
				fact = m
			}
		}
		local[base] = fact
	}
	return out
}

// factErrText is the bare OS error text facts.d failures embed.
func factErrText(err error) string {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}

// decodeJSONStrict parses a complete JSON document (json.loads), keeping
// integers as int64.
func decodeJSONStrict(data []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	// Trailing non-space garbage makes json.loads fail.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, false
	}
	rest := data[dec.InputOffset():]
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, false
	}
	return normalizeJSONNumbers(v), true
}

// decodeJSONValue parses JSON leniently (nil on failure).
func decodeJSONValue(data []byte) any {
	v, _ := decodeJSONStrict(bytes.TrimSpace(data))
	return v
}

func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return n
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeJSONNumbers(x)
		}
	case []any:
		for i, x := range t {
			t[i] = normalizeJSONNumbers(x)
		}
	}
	return v
}

// iniFile is a parsed Python configparser document (default settings:
// '=' and ':' delimiters, full-line '#'/';' comments, lowercased option
// names, strict duplicates, BasicInterpolation, multi-line continuations).
type iniFile struct {
	defaults *iniSection
	sections []*iniSection
}

type iniSection struct {
	name   string
	keys   []string
	values map[string][]string
}

func (s *iniSection) get(k string) (string, bool) {
	v, ok := s.values[k]
	if !ok {
		return "", false
	}
	return strings.TrimRight(strings.Join(v, "\n"), " \t\r\n\f\v"), true
}

var (
	iniSectRe = regexp.MustCompile(`^\[(.+)\]`)
	iniOptRe  = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)
)

func parseINI(content string) (*iniFile, error) {
	ini := &iniFile{defaults: &iniSection{name: "DEFAULT", values: map[string][]string{}}}
	seen := map[string]bool{}
	var cur *iniSection
	var optname string
	indentLevel := 0
	var parseErr error
	lines := strings.Split(content, "\n")
	for lineno, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			continue // full-line comment: never extends a value
		}
		if stripped == "" {
			// empty_lines_in_values: kept inside a multi-line value (and
			// trimmed from its end when joined).
			if cur != nil && optname != "" {
				cur.values[optname] = append(cur.values[optname], "")
			}
			continue
		}
		curIndent := len(line) - len(strings.TrimLeft(line, " \t\f\v"))
		if cur != nil && optname != "" && curIndent > indentLevel {
			cur.values[optname] = append(cur.values[optname], stripped)
			continue
		}
		indentLevel = curIndent
		if m := iniSectRe.FindStringSubmatch(stripped); m != nil {
			name := m[1]
			if seen[name] {
				return nil, fmt.Errorf("While reading from '<???>' [line %2d]: section %q already exists", lineno+1, name)
			}
			seen[name] = true
			if name == "DEFAULT" {
				cur = ini.defaults
			} else {
				cur = &iniSection{name: name, values: map[string][]string{}}
				ini.sections = append(ini.sections, cur)
			}
			optname = ""
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("File contains no section headers.")
		}
		m := iniOptRe.FindStringSubmatch(stripped)
		if m == nil || m[1] == "" {
			if parseErr == nil {
				parseErr = fmt.Errorf("Source contains parsing errors: [line %2d]: %q", lineno+1, line)
			}
			optname = ""
			continue
		}
		optname = strings.ToLower(strings.TrimRight(m[1], " \t"))
		if _, dup := cur.values[optname]; dup {
			return nil, fmt.Errorf("While reading from '<???>' [line %2d]: option %q in section %q already exists", lineno+1, optname, cur.name)
		}
		cur.keys = append(cur.keys, optname)
		cur.values[optname] = []string{strings.TrimSpace(m[3])}
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return ini, nil
}

// toFacts is the sections -> {option: value} conversion local.py does,
// with BasicInterpolation applied by ConfigParser.get.
func (ini *iniFile) toFacts() (map[string]any, error) {
	out := map[string]any{}
	for _, s := range ini.sections {
		sect := map[string]any{}
		keys := append([]string(nil), s.keys...)
		for _, k := range ini.defaults.keys {
			if _, ok := s.values[k]; !ok {
				keys = append(keys, k)
			}
		}
		lookup := func(k string) (string, bool) {
			if v, ok := s.get(k); ok {
				return v, true
			}
			return ini.defaults.get(k)
		}
		for _, k := range keys {
			raw, _ := lookup(k)
			v, err := iniInterpolate(s.name, k, raw, lookup, 1)
			if err != nil {
				return nil, err
			}
			sect[k] = v
		}
		out[s.name] = sect
	}
	return out, nil
}

func iniInterpolate(section, option, value string, lookup func(string) (string, bool), depth int) (string, error) {
	if depth > 10 {
		return "", fmt.Errorf("Recursion limit exceeded in value substitution: option %q in section %q contains an interpolation key which cannot be substituted in 10 steps. Raw value: %q", option, section, value)
	}
	var b strings.Builder
	rest := value
	for {
		i := strings.IndexByte(rest, '%')
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:i])
		rest = rest[i:]
		if len(rest) < 2 {
			return "", fmt.Errorf("'%%' must be followed by '%%' or '(', found: %q", rest)
		}
		switch rest[1] {
		case '%':
			b.WriteByte('%')
			rest = rest[2:]
		case '(':
			end := strings.Index(rest, ")s")
			if end < 0 {
				return "", fmt.Errorf("bad interpolation variable reference %q", rest)
			}
			name := strings.ToLower(rest[2:end])
			v, ok := lookup(name)
			if !ok {
				return "", fmt.Errorf("Bad value substitution: option %q in section %q contains an interpolation key %q which is not a valid option name. Raw value: %q", option, section, name, value)
			}
			if strings.Contains(v, "%") {
				var err error
				v, err = iniInterpolate(section, name, v, lookup, depth+1)
				if err != nil {
					return "", err
				}
			}
			b.WriteString(v)
			rest = rest[end+2:]
		default:
			return "", fmt.Errorf("'%%' must be followed by '%%' or '(', found: %q", rest)
		}
	}
}
