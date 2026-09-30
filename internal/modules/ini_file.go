package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
	"github.com/giraffesyo/understudy/internal/modules/fsutil"
)

// This file ports community.general.ini_file.

func init() {
	names := []string{"ini_file", "community.general.ini_file"}
	Register(iniFileModule, names...)
	for _, n := range names {
		specs[n] = iniFileSpec
		pathInfoModules[n] = true
	}
}

var iniFileSpec = args.Spec{
	"path":                   {Required: true, Aliases: []string{"dest"}},
	"section":                {},
	"section_has_values":     {Type: "list"},
	"option":                 {},
	"value":                  {},
	"values":                 {Type: "list"},
	"backup":                 {Type: "bool", Default: false},
	"state":                  {Default: "present", Choices: []string{"absent", "present"}},
	"exclusive":              {Type: "bool", Default: true},
	"no_extra_spaces":        {Type: "bool", Default: false},
	"ignore_spaces":          {Type: "bool", Default: false},
	"allow_no_value":         {Type: "bool", Default: false},
	"modify_inactive_option": {Type: "bool", Default: true},
	"create":                 {Type: "bool", Default: true},
	"follow":                 {Type: "bool", Default: false},
	"mode":                   {Type: "any"},
	"owner":                  {},
	"group":                  {},
	"seuser":                 {},
	"serole":                 {},
	"setype":                 {},
	"selevel":                {},
	"attributes":             {Aliases: []string{"attr"}},
	"unsafe_writes":          {Type: "bool", Default: false},
}

// iniCondition is one section_has_values entry.
type iniCondition struct {
	option string
	values []string
}

// iniOptRe caches the match_opt / match_active_opt patterns per option.
func iniFileOptRe(option string, active bool) *regexp.Regexp {
	comment := `[#;]?`
	lead := `(?: |\t)*`
	if active {
		comment = ``
		lead = ``
	}
	return regexp.MustCompile(`^(?: |\t)*(?P<comment>` + comment + `)` + lead +
		regexp.QuoteMeta(option) + `(?: |\t)*(?P<sep>=|$)(?: |\t)*(?P<value>.*)`)
}

// iniMatch is the result of match_opt: nil when the line does not match.
type iniMatch struct {
	comment, sep, value string
}

// iniMatchLine runs a match_opt pattern the way Python's re.match does on a
// line that still carries its "\n" ("$" also matches before it; "." never
// crosses it).
func iniMatchLine(re *regexp.Regexp, line string) *iniMatch {
	s := strings.TrimSuffix(line, "\n")
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	return &iniMatch{comment: m[1], sep: m[2], value: m[3]}
}

func strIn(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func iniStrList(v []any) []string {
	out := make([]string, 0, len(v))
	for _, e := range v {
		if e == nil {
			continue
		}
		out = append(out, pyStr(e))
	}
	return out
}

// pyStr renders a scalar the way Ansible's str type conversion does.
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return fmt.Sprintf("%v", v)
}

func iniFileModule(env *RunEnv, raw map[string]any) *agentproto.Result {
	if err := iniFileSpec.MutuallyExclusive(raw, []string{"value", "values"}); err != nil {
		return agentproto.Fail("%v", err)
	}
	p, err := iniFileSpec.Parse(raw)
	if err != nil {
		return agentproto.Fail("%v", err)
	}
	path := pyExpandPath(p.Str("path"))
	state := p.Str("state")
	allowNoValue := p.Bool("allow_no_value")
	if state == "present" && !allowNoValue && !p.Has("value") && len(p.List("values")) == 0 {
		return agentproto.Fail("Parameter 'value(s)' must be defined if state=present and allow_no_value=False.")
	}
	var values []string
	if p.Has("value") {
		values = []string{pyStrArg(raw, p, "value")}
	} else {
		values = iniStrList(p.List("values"))
	}
	var conds []iniCondition
	for _, c := range p.List("section_has_values") {
		m, ok := c.(map[string]any)
		if !ok {
			return agentproto.Fail("Elements value for option 'section_has_values' is of type %s and we were unable to convert to dict", pyTypeName(c))
		}
		for k := range m {
			if k != "option" && k != "value" && k != "values" {
				return agentproto.Fail("Unsupported parameters for (section_has_values) option: %s. Supported parameters include: option, value, values.", k)
			}
		}
		opt, ok := m["option"]
		if !ok || opt == nil {
			return agentproto.Fail("missing required arguments: option found in section_has_values")
		}
		if m["value"] != nil && m["values"] != nil {
			return agentproto.Fail("parameters are mutually exclusive: value|values found in section_has_values")
		}
		cond := iniCondition{option: pyStr(opt)}
		if v := m["value"]; v != nil {
			cond.values = []string{pyStr(v)}
		} else if l, ok := m["values"].([]any); ok {
			cond.values = iniStrList(l)
		} else if s, ok := m["values"].(string); ok {
			cond.values = iniStrList(listFromString(s))
		} else {
			cond.values = []string{}
		}
		conds = append(conds, cond)
	}
	var section, option *string
	if p.Has("section") {
		s := pyStrArg(raw, p, "section")
		section = &s
	}
	if p.Has("option") {
		s := pyStrArg(raw, p, "option")
		option = &s
	}

	r := &iniRun{env: env, p: p, filename: path, section: section, option: option,
		values: values, conds: conds, hasConds: p.Has("section_has_values"), state: state}
	res := r.do()
	if res.Failed {
		return res
	}
	if !env.CheckMode && pathExists(path) {
		fa := loadFileAttrs(p, path, p.Bool("follow"))
		changed, fail := setFSAttrs(env, fa, res.Changed, nil)
		if fail != nil {
			return fail
		}
		res.Changed = changed
	}
	return res
}

func listFromString(s string) []any {
	var out []any
	for _, part := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

type iniRun struct {
	env      *RunEnv
	p        *args.Parsed
	filename string
	section  *string
	option   *string
	values   []string
	conds    []iniCondition
	hasConds bool
	state    string
}

// checkSectionHasValues is check_section_has_values.
func (r *iniRun) checkSectionHasValues(lines []string) bool {
	if !r.hasConds {
		return true
	}
	for _, c := range r.conds {
		re := iniFileOptRe(c.option, false)
		found := false
		for _, line := range lines {
			if m := iniMatchLine(re, line); m != nil && (len(c.values) == 0 || strIn(m.value, c.values)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// do is do_ini.
func (r *iniRun) do() *agentproto.Result {
	env, p := r.env, r.p
	exclusive := p.Bool("exclusive")
	allowNoValue := p.Bool("allow_no_value")
	ignoreSpaces := p.Bool("ignore_spaces")

	// Deduplicate values, keeping order.
	var values []string
	for _, v := range r.values {
		if !strIn(v, values) {
			values = append(values, v)
		}
	}

	filename := r.filename
	diff := map[string]any{"before": "", "after": "",
		"before_header": filename + " (content)", "after_header": filename + " (content)"}

	target := filename
	if p.Bool("follow") {
		if info, err := os.Lstat(filename); err == nil && info.Mode()&os.ModeSymlink != 0 {
			target = pyRealpath(filename)
		}
	}

	var lines []string
	if !pathExists(target) {
		if !p.Bool("create") {
			return &agentproto.Result{Failed: true, Msg: fmt.Sprintf("Destination %s does not exist!", target),
				Extra: map[string]any{"rc": int64(257)}}
		}
		dest := dirOf(target)
		if dest != "" && !pathExists(dest) && !env.CheckMode {
			if err := os.MkdirAll(dest, 0o777); err != nil {
				return moduleCrash(err)
			}
		}
	} else {
		var err error
		if lines, err = readLines(target); err != nil {
			return moduleCrash(err)
		}
		if len(lines) > 0 {
			lines[0] = strings.TrimPrefix(lines[0], "\ufeff")
			if lines[0] == "" {
				lines = lines[1:]
			}
		}
	}
	if env.DiffMode {
		diff["before"] = strings.Join(lines, "")
	}

	changed := false
	if len(lines) == 0 {
		lines = append(lines, "\n")
	}
	if last := lines[len(lines)-1]; last == "" || last[len(last)-1] != '\n' {
		lines[len(lines)-1] += "\n"
		changed = true
	}

	const fake = "ad01e11446efb704fcdbdb21f2c43757423d91c5"
	lines = append([]string{"[" + fake + "]"}, lines...)
	lines = append(lines, "[")

	section := fake
	if r.section != nil && *r.section != "" {
		section = *r.section
	}
	withinSection := false
	sectionStart, sectionEnd := 0, 0
	msg := "OK"
	sep := " = "
	if p.Bool("no_extra_spaces") {
		sep = "="
	}
	optionNoValuePresent := false
	nonBlankNonComment := regexp.MustCompile(`^[ \t]*([#;].*)?$`)
	sectionRe := regexp.MustCompile(`^\[\s*` + regexp.QuoteMeta(pyStrip(section)) + `\s*]`)

	for index, line := range lines {
		if withinSection && strings.HasPrefix(line, "[") {
			if r.checkSectionHasValues(lines[sectionStart:index]) {
				sectionEnd = index
				break
			}
			withinSection = false
			sectionStart, sectionEnd = 0, 0
		}
		if sectionRe.MatchString(line) {
			withinSection = true
			sectionStart = index
		}
	}

	before := append([]string{}, lines[:sectionStart]...)
	sectionLines := append([]string{}, lines[sectionStart:sectionEnd]...)
	after := append([]string{}, lines[sectionEnd:]...)
	changedLines := make([]int, len(sectionLines))

	option := ""
	hasOption := r.option != nil && *r.option != ""
	if r.option != nil {
		option = *r.option
	}
	optRe := iniFileOptRe(option, !p.Bool("modify_inactive_option"))
	activeRe := iniFileOptRe(option, true)
	fullRe := iniFileOptRe(option, false)

	update := func(index int, newline string) {
		var optionChanged *bool
		if ignoreSpaces {
			old := iniMatchLine(fullRe, sectionLines[index])
			if old.comment == "" {
				nm := iniMatchLine(fullRe, newline)
				c := nm == nil || old.value != nm.value
				optionChanged = &c
			}
		}
		if optionChanged == nil {
			c := sectionLines[index] != newline
			optionChanged = &c
		}
		if *optionChanged {
			sectionLines[index] = newline
			changed = true
			msg = "option changed"
		}
		changedLines[index] = 1
	}
	removeValue := func(v string) {
		for i, x := range values {
			if x == v {
				values = append(values[:i], values[i+1:]...)
				return
			}
		}
	}

	if r.state == "present" && hasOption {
		for index, line := range sectionLines {
			m := iniMatchLine(optRe, line)
			if m == nil {
				continue
			}
			if len(values) > 0 && strIn(m.value, values) {
				matched := m.value
				var newline string
				if matched == "" && allowNoValue {
					newline = option + "\n"
					optionNoValuePresent = true
				} else {
					newline = option + sep + matched + "\n"
				}
				update(index, newline)
				removeValue(matched)
			} else if len(values) == 0 && allowNoValue {
				update(index, option+"\n")
				optionNoValuePresent = true
				break
			}
		}
	}

	if r.state == "present" && exclusive && !allowNoValue {
		if len(values) > 0 {
			for index, line := range sectionLines {
				var m *iniMatch
				if changedLines[index] == 0 {
					m = iniMatchLine(optRe, line)
				}
				if m != nil && !(m.comment != "" && m.sep == "") {
					v := values[0]
					values = values[1:]
					update(index, option+sep+v+"\n")
					if len(values) == 0 {
						break
					}
				}
			}
		}
		for index := len(sectionLines) - 1; index > 0; index-- {
			var m *iniMatch
			if changedLines[index] == 0 {
				m = iniMatchLine(optRe, sectionLines[index])
			}
			if m != nil && !(m.comment != "" && m.sep == "") {
				sectionLines = append(sectionLines[:index], sectionLines[index+1:]...)
				changedLines = append(changedLines[:index], changedLines[index+1:]...)
				changed = true
				msg = "option changed"
			}
		}
	}

	insertAt := func(i int, s string) {
		sectionLines = append(sectionLines[:i], append([]string{s}, sectionLines[i:]...)...)
	}
	if r.state == "present" {
		for index := len(sectionLines); index > 0; index-- {
			if !nonBlankNonComment.MatchString(strings.TrimSuffix(sectionLines[index-1], "\n")) {
				if hasOption && len(values) > 0 {
					for i := len(values) - 1; i >= 0; i-- {
						insertAt(index, option+sep+values[i]+"\n")
						msg = "option added"
						changed = true
					}
				} else if hasOption && len(values) == 0 && allowNoValue && !optionNoValuePresent {
					insertAt(index, option+"\n")
					msg = "option added"
					changed = true
				}
				break
			}
		}
	}

	if r.state == "absent" {
		if hasOption {
			if exclusive {
				var kept []string
				for _, l := range sectionLines {
					if iniMatchLine(activeRe, l) == nil {
						kept = append(kept, l)
					}
				}
				if len(kept) != len(sectionLines) {
					changed = true
					msg = "option changed"
					sectionLines = kept
				}
			} else if len(values) > 0 {
				var kept []string
				for _, l := range sectionLines {
					if m := iniMatchLine(activeRe, l); !(m != nil && strIn(m.value, values)) {
						kept = append(kept, l)
					}
				}
				if len(kept) != len(sectionLines) {
					changed = true
					msg = "option changed"
					sectionLines = kept
				}
			}
		} else if len(sectionLines) > 0 {
			sectionLines = nil
			msg = "section removed"
			changed = true
		}
	}

	out := append(append(before, sectionLines...), after...)
	out = out[1 : len(out)-1]

	if !withinSection && r.state == "present" {
		out = append(out, "["+section+"]\n")
		msg = "section and option added"
		if r.hasConds {
			for _, c := range r.conds {
				if c.option != option {
					if len(c.values) > 0 {
						for _, v := range c.values {
							out = append(out, c.option+sep+v+"\n")
						}
					} else if allowNoValue {
						out = append(out, c.option+"\n")
					}
				} else if !exclusive {
					for _, v := range c.values {
						if !strIn(v, values) {
							values = append(values, v)
						}
					}
				}
			}
		}
		if hasOption && len(values) > 0 {
			for _, v := range values {
				out = append(out, option+sep+v+"\n")
			}
		} else if hasOption && len(values) == 0 && allowNoValue {
			out = append(out, option+"\n")
		} else {
			msg = "only section added"
		}
		changed = true
	}

	content := strings.Join(out, "")
	if env.DiffMode {
		diff["after"] = content
	}

	res := &agentproto.Result{Changed: changed, Msg: msg, Diff: diff,
		Extra: map[string]any{"path": filename}}
	if changed && !env.CheckMode {
		if p.Bool("backup") {
			b, err := fsutil.Backup(target)
			if err != nil {
				return moduleCrash(err)
			}
			res.Extra["backup_file"] = b
		}
		tmp, err := os.CreateTemp("", "tmp")
		if err != nil {
			return agentproto.Fail("Unable to create temporary file %%s")
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := tmp.WriteString(content); err != nil {
			tmp.Close()
			return agentproto.Fail("Unable to create temporary file %%s")
		}
		tmp.Close()
		abs, _ := filepath.Abs(target)
		if err := fsutil.AtomicMove(tmpName, abs, true); err != nil {
			return moduleCrash(err)
		}
	}
	return res
}

// pyStrip is str.strip(): surrounding whitespace removed.
func pyStrip(s string) string {
	return strings.TrimSpace(s)
}

// pyStrArg is a type=str parameter as AnsibleModule converts it: a YAML
// boolean becomes "True"/"False" (str(value)), unlike the args package's
// "yes"/"no" coercion.
func pyStrArg(raw map[string]any, p *args.Parsed, name string) string {
	if b, ok := raw[name].(bool); ok {
		return pyStr(b)
	}
	return p.Str(name)
}
