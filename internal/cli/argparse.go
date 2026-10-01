package cli

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// This file ports what ansible-core's command line relies on from
// Python's argparse (3.14): option matching (abbreviations, combined
// short options, attached values, "--"), its errors, and the usage and
// help text of HelpFormatter (wrapping, sorted options, colors), so that
// ansible-playbook and ansible fail and explain themselves as ansible's do.

// argAction is one argparse action.
type argAction struct {
	opts    []string // option strings; none for a positional
	dest    string
	metavar string // "" = dest (upper-cased for an option)
	help    string
	// nargs: 0 takes no value, 1 one value, -1 one or more (a positional).
	nargs int
	// isInt converts the value as type=int does (ansible's wrapper names
	// it tag_value in errors).
	isInt bool
	// deprecated, when set, is the DeprecatedArgument: the option string
	// it applies to ("" = all), the version removing it and alternatives.
	deprecated *argDeprecation
	// apply takes the action with its (converted) values, under the option
	// string used ("" for a positional).
	apply func(p *parsedArgs, values []string, opt string) error
	// exits: -h and --version print and end parsing.
	exits func(ap *argParser) int
}

type argDeprecation struct{ option, version, alternatives string }

// argGroup is an argument group of the help: its title and description.
type argGroup struct {
	title, desc string
	actions     []*argAction
}

// argParser is an ArgumentParser with ansible's SortingHelpFormatter.
type argParser struct {
	prog, desc, epilog string
	actions            []*argAction // in the order added (usage order)
	groups             []*argGroup  // positionals, options, then the named groups
	mutex              [][]*argAction
	optMap             map[string]*argAction
	optOrder           []string // option strings in the order registered
	deprecationsOn     bool
}

func newArgParser(prog, desc, epilog string) *argParser {
	return &argParser{prog: prog, desc: desc, epilog: epilog, optMap: map[string]*argAction{},
		groups: []*argGroup{{title: "positional arguments"}, {title: "options"}}}
}

// add adds an action to a group (nil = options, or positional arguments
// for a positional).
func (ap *argParser) add(g *argGroup, a *argAction) *argAction {
	if g == nil {
		g = ap.groups[1]
		if len(a.opts) == 0 {
			g = ap.groups[0]
		}
	}
	ap.actions = append(ap.actions, a)
	g.actions = append(g.actions, a)
	for _, o := range a.opts {
		if _, dup := ap.optMap[o]; !dup {
			ap.optOrder = append(ap.optOrder, o)
		}
		ap.optMap[o] = a
	}
	return a
}

func (ap *argParser) group(title, desc string) *argGroup {
	g := &argGroup{title: title, desc: desc}
	ap.groups = append(ap.groups, g)
	return g
}

// addMutex adds a mutually exclusive group of options (to options).
func (ap *argParser) addMutex(actions ...*argAction) {
	for _, a := range actions {
		ap.add(nil, a)
	}
	ap.mutex = append(ap.mutex, actions)
}

// argError is argparse's ArgumentError: the action it is about (nil =
// none) and the message.
type argError struct {
	action *argAction
	msg    string
}

func (e *argError) Error() string {
	if e.action == nil {
		return e.msg
	}
	return fmt.Sprintf("argument %s: %s", actionName(e.action), e.msg)
}

// actionName is _get_action_name.
func actionName(a *argAction) string {
	if len(a.opts) > 0 {
		return strings.Join(a.opts, "/")
	}
	if a.metavar != "" {
		return a.metavar
	}
	return a.dest
}

type optTuple struct {
	action   *argAction
	opt      string
	sep      *string // nil = no separator
	explicit *string // nil = no explicit argument
}

func strp(s string) *string { return &s }

var negativeNumberRe = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// parseOptional is _parse_optional: nil for a positional, else the
// options the string could be (an unknown one has a nil action).
func (ap *argParser) parseOptional(arg string) []optTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a, ok := ap.optMap[arg]; ok {
		return []optTuple{{action: a, opt: arg}}
	}
	if len(arg) == 1 {
		return nil
	}
	if opt, explicit, ok := strings.Cut(arg, "="); ok {
		if a, found := ap.optMap[opt]; found {
			return []optTuple{{action: a, opt: opt, sep: strp("="), explicit: strp(explicit)}}
		}
	}
	if tuples := ap.optionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if negativeNumberRe.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return []optTuple{{opt: arg}}
}

// optionTuples is _get_option_tuples (allow_abbrev).
func (ap *argParser) optionTuples(arg string) []optTuple {
	var out []optTuple
	prefix, explicit, hasSep := strings.Cut(arg, "=")
	var sep, exp *string
	if hasSep {
		sep, exp = strp("="), strp(explicit)
	}
	if strings.HasPrefix(arg, "--") {
		for _, o := range ap.optOrder {
			if strings.HasPrefix(o, prefix) {
				out = append(out, optTuple{action: ap.optMap[o], opt: o, sep: sep, explicit: exp})
			}
		}
		return out
	}
	short, shortExplicit := arg[:2], arg[2:]
	for _, o := range ap.optOrder {
		if o == short {
			out = append(out, optTuple{action: ap.optMap[o], opt: o, sep: strp(""), explicit: strp(shortExplicit)})
		} else if strings.HasPrefix(o, prefix) {
			out = append(out, optTuple{action: ap.optMap[o], opt: o, sep: sep, explicit: exp})
		}
	}
	return out
}

// nargsPattern is _get_nargs_pattern.
func nargsPattern(a *argAction) string {
	switch {
	case a.nargs == 0 && len(a.opts) > 0:
		return "([AO]{0})"
	case a.nargs == 1 && len(a.opts) > 0:
		return "([A])"
	case a.nargs == 1:
		return "(-*A-*)"
	default: // one or more, a positional
		return "(-*A[A-]*)"
	}
}

// matchArgument is _match_argument: how many of the strings ahead
// (their pattern) the action takes.
func matchArgument(a *argAction, pattern string) (int, error) {
	m := regexp.MustCompile("^" + nargsPattern(a)).FindStringSubmatch(pattern)
	if m == nil {
		msg := "expected one argument"
		if a.nargs == -1 {
			msg = "expected at least one argument"
		}
		return 0, &argError{a, msg}
	}
	return len(m[1]), nil
}

// parseResult is how parsing ended: the parsed arguments, or an exit
// (help, version, an error) whose output is already shown.
type parseResult struct {
	exit  bool
	code  int
	apply error // a value an action could not take (reported by the caller)
}

// parse is parse_args under ansible's CLI.parse: an argparse error shows
// the usage, "<prog>: error: <message>" and then the whole help, all on
// stderr, and exits 2.
func (ap *argParser) parse(args []string, p *parsedArgs) parseResult {
	res, err := ap.parseKnown(args, p)
	if err != nil {
		ap.fail(err.Error(), true)
		return parseResult{exit: true, code: 2}
	}
	return res
}

// fail is ArgumentParser.error: the usage and the error on stderr; with
// help, ansible's " \n" and the help follow.
func (ap *argParser) fail(msg string, help bool) {
	fmt.Fprint(os.Stderr, ap.formatUsage())
	fmt.Fprintf(os.Stderr, "%s: error: %s\n", ap.prog, msg)
	if help {
		fmt.Fprint(os.Stderr, " \n"+ap.formatHelp())
	}
}

func (ap *argParser) parseKnown(args []string, p *parsedArgs) (parseResult, error) {
	conflicts := map[*argAction][]*argAction{}
	for _, g := range ap.mutex {
		for i, a := range g {
			conflicts[a] = append(append(conflicts[a], g[:i]...), g[i+1:]...)
		}
	}
	optIndices := map[int][]optTuple{}
	var pat strings.Builder
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pat.WriteByte('-')
			for range args[i+1:] {
				pat.WriteByte('A')
			}
			break
		}
		if t := ap.parseOptional(args[i]); t != nil {
			optIndices[i] = t
			pat.WriteByte('O')
		} else {
			pat.WriteByte('A')
		}
	}
	pattern := pat.String()

	seen := map[*argAction]bool{}
	seenNonDefault := map[*argAction]bool{}
	warned := map[string]bool{}
	var result parseResult
	takeAction := func(a *argAction, values []string, opt string) error {
		seen[a] = true
		if a.isInt {
			for _, v := range values {
				if _, ok := pyInt(v); !ok {
					return &argError{a, "invalid tag_value value: " + pyStrRepr(v)}
				}
			}
		}
		if len(a.opts) > 0 || len(values) > 0 {
			seenNonDefault[a] = true
			for _, c := range conflicts[a] {
				if seenNonDefault[c] {
					return &argError{a, "not allowed with argument " + actionName(c)}
				}
			}
		}
		if d := a.deprecated; d != nil && (d.option == "" || d.option == opt) && ap.deprecationsOn {
			msg := fmt.Sprintf("The '%s' argument is deprecated. This feature will be removed from ansible-core version %s.", opt, d.version)
			if d.alternatives != "" {
				msg += " Use " + d.alternatives + " instead."
			}
			showDeprecation("[DEPRECATION WARNING]: " + msg + "\n")
		}
		if a.exits != nil {
			result = parseResult{exit: true, code: a.exits(ap)}
			return errExit
		}
		if a.apply != nil {
			if err := a.apply(p, values, opt); err != nil && result.apply == nil {
				result.apply = err
			}
		}
		return nil
	}
	var extras []string
	consumeOptional := func(start int) (int, error) {
		tuples := optIndices[start]
		if len(tuples) > 1 {
			var names []string
			for _, t := range tuples {
				names = append(names, t.opt)
			}
			return 0, &argError{nil, fmt.Sprintf("ambiguous option: %s could match %s", args[start], strings.Join(names, ", "))}
		}
		t := tuples[0]
		action, opt, sep, explicit := t.action, t.opt, t.sep, t.explicit
		type taken struct {
			action *argAction
			args   []string
			opt    string
		}
		var actions []taken
		stop := 0
		for {
			if action == nil {
				extras = append(extras, args[start])
				return start + 1, nil
			}
			if explicit != nil {
				count, err := matchArgument(action, "A")
				if err != nil {
					return 0, err
				}
				if count == 0 && opt[1] != '-' && *explicit != "" {
					if sep != nil && *sep != "" || (*explicit)[0] == '-' {
						return 0, &argError{action, "ignored explicit argument " + pyStrRepr(*explicit)}
					}
					actions = append(actions, taken{action, nil, opt})
					opt = "-" + (*explicit)[:1]
					if a, ok := ap.optMap[opt]; ok {
						action = a
						rest := (*explicit)[1:]
						switch {
						case rest == "":
							sep, explicit = nil, nil
						case rest[0] == '=':
							sep, explicit = strp("="), strp(rest[1:])
						default:
							sep, explicit = strp(""), strp(rest)
						}
					} else {
						extras = append(extras, "-"+*explicit)
						stop = start + 1
						break
					}
				} else if count == 1 {
					stop = start + 1
					actions = append(actions, taken{action, []string{*explicit}, opt})
					break
				} else {
					return 0, &argError{action, "ignored explicit argument " + pyStrRepr(*explicit)}
				}
			} else {
				next := start + 1
				count, err := matchArgument(action, pattern[min(next, len(pattern)):])
				if err != nil {
					return 0, err
				}
				stop = next + count
				actions = append(actions, taken{action, args[next:stop], opt})
				break
			}
		}
		for _, t := range actions {
			if err := takeAction(t.action, t.args, t.opt); err != nil {
				return 0, err
			}
		}
		_ = warned
		return stop, nil
	}

	var positionals []*argAction
	for _, a := range ap.actions {
		if len(a.opts) == 0 {
			positionals = append(positionals, a)
		}
	}
	consumePositionals := func(start int) (int, error) {
		counts := matchPartial(positionals, pattern[start:])
		for i, count := range counts {
			a := positionals[i]
			vals := slices.Clone(args[start : start+count])
			if strings.Contains(pattern[start:start+count], "-") {
				if k := slices.Index(vals, "--"); k >= 0 {
					vals = slices.Delete(vals, k, k+1)
				}
			}
			start += count
			if err := takeAction(a, vals, ""); err != nil {
				return 0, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
	}

	maxOpt := -1
	for i := range optIndices {
		maxOpt = max(maxOpt, i)
	}
	start := 0
	for start <= maxOpt {
		next := start
		for next <= maxOpt {
			if _, ok := optIndices[next]; ok {
				break
			}
			next++
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return result, ap.exitOr(err)
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := optIndices[start]; !ok {
			extras = append(extras, args[start:next]...)
			start = next
		}
		end, err := consumeOptional(start)
		if err != nil {
			return result, ap.exitOr(err)
		}
		start = end
	}
	stop, err := consumePositionals(start)
	if err != nil {
		return result, ap.exitOr(err)
	}
	extras = append(extras, args[stop:]...)

	var required []string
	for _, a := range ap.actions {
		if len(a.opts) == 0 && !seen[a] {
			required = append(required, actionName(a))
		}
	}
	if len(required) > 0 {
		return result, &argError{nil, "the following arguments are required: " + strings.Join(required, ", ")}
	}
	if len(extras) > 0 {
		return result, &argError{nil, "unrecognized arguments: " + strings.Join(extras, " ")}
	}
	return result, nil
}

// errExit ends parsing after -h or --version (their output shown).
var errExit = fmt.Errorf("exit")

// exitOr turns the end of parsing that -h/--version asked for into no
// error (parseKnown's result carries the exit).
func (ap *argParser) exitOr(err error) error {
	if err == errExit {
		return nil
	}
	return err
}

// matchPartial is _match_arguments_partial.
func matchPartial(actions []*argAction, pattern string) []int {
	for i := len(actions); i > 0; i-- {
		var re strings.Builder
		re.WriteByte('^')
		for _, a := range actions[:i] {
			re.WriteString(nargsPattern(a))
		}
		m := regexp.MustCompile(re.String()).FindStringSubmatchIndex(pattern)
		if m == nil {
			continue
		}
		var out []int
		for g := 1; g*2 < len(m); g++ {
			out = append(out, m[g*2+1]-m[g*2])
		}
		if m[1] < len(pattern) && pattern[m[1]] == 'O' {
			for len(out) > 0 && out[len(out)-1] == 0 {
				out = out[:len(out)-1]
			}
		}
		return out
	}
	return nil
}

// pyInt is int(s) for a str: surrounding whitespace, a sign, digits
// with single underscores between them.
func pyInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if !regexp.MustCompile(`^[+-]?\d(_?\d)*$`).MatchString(s) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimPrefix(s, "+"), "_", ""))
	return n, err == nil
}

// pyStrRepr is repr() of a str.
func pyStrRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch {
		case string(r) == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// ---- formatting (HelpFormatter) ----

// argTheme is _colorize's argparse theme (all empty without colors).
type argTheme struct {
	usage, prog, heading, summaryLong, summaryShort, summaryLabel, summaryAction,
	long, short, label, action, reset string
}

// argColors is can_colorize(): PYTHON_COLORS, NO_COLOR, FORCE_COLOR,
// TERM=dumb, else whether stdout is a terminal.
func argColors() argTheme {
	on := func() bool {
		switch os.Getenv("PYTHON_COLORS") {
		case "0":
			return false
		case "1":
			return true
		}
		if os.Getenv("NO_COLOR") != "" {
			return false
		}
		if os.Getenv("FORCE_COLOR") != "" {
			return true
		}
		if os.Getenv("TERM") == "dumb" {
			return false
		}
		return term.IsTerminal(int(os.Stdout.Fd()))
	}()
	if !on {
		return argTheme{}
	}
	return argTheme{usage: "\x1b[1;34m", prog: "\x1b[1;35m", heading: "\x1b[1;34m",
		summaryLong: "\x1b[36m", summaryShort: "\x1b[32m", summaryLabel: "\x1b[33m", summaryAction: "\x1b[32m",
		long: "\x1b[1;36m", short: "\x1b[1;32m", label: "\x1b[1;33m", action: "\x1b[1;32m", reset: "\x1b[0m"}
}

var ansiCodeRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// decolor is _colorize.decolor.
func decolor(s string) string { return ansiCodeRe.ReplaceAllString(s, "") }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// helpWidth is HelpFormatter's width: shutil.get_terminal_size().columns
// (COLUMNS, else the terminal on stdout, else 80) less 2.
func helpWidth() int {
	cols := 0
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 {
		cols = v
	} else if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		cols = w
	}
	if cols <= 0 {
		cols = 80
	}
	return cols - 2
}

func (a *argAction) defaultMetavar() string {
	if a.metavar != "" {
		return a.metavar
	}
	if len(a.opts) > 0 {
		return strings.ToUpper(a.dest)
	}
	return a.dest
}

// formatArgs is _format_args.
func (a *argAction) formatArgs() string {
	m := a.defaultMetavar()
	if a.nargs == -1 {
		return m + " [" + m + " ...]"
	}
	return m
}

func isLongOption(s string) bool { return len(s) > 2 }

// usageParts is _get_actions_usage_parts: the parts and where the
// positionals start.
func (ap *argParser) usageParts(t argTheme) ([]string, int) {
	groupOf := map[*argAction]int{}
	for gi, g := range ap.mutex {
		for _, a := range g {
			groupOf[a] = gi + 1
		}
	}
	type entry struct{ actions []*argAction }
	var optionals, positionals []entry
	done := map[*argAction]bool{}
	for _, a := range ap.actions {
		if len(a.opts) == 0 {
			positionals = append(positionals, entry{[]*argAction{a}})
		}
	}
	for _, a := range ap.actions {
		if len(a.opts) == 0 || done[a] {
			continue
		}
		if gi := groupOf[a]; gi > 0 {
			var group []*argAction
			for _, b := range ap.mutex[gi-1] {
				if !done[b] {
					group = append(group, b)
					done[b] = true
				}
			}
			optionals = append(optionals, entry{group})
			continue
		}
		done[a] = true
		optionals = append(optionals, entry{[]*argAction{a}})
	}
	var parts []string
	posStart := -1
	for i, e := range append(optionals, positionals...) {
		start := len(parts)
		if i == len(optionals) {
			posStart = start
		}
		inGroup := len(e.actions) > 1
		for _, a := range e.actions {
			var part string
			if len(a.opts) == 0 {
				part = t.summaryAction + a.formatArgs() + t.reset
			} else {
				color := t.summaryShort
				if isLongOption(a.opts[0]) {
					color = t.summaryLong
				}
				if a.nargs == 0 {
					part = color + a.opts[0] + t.reset
				} else {
					part = color + a.opts[0] + " " + t.summaryLabel + a.formatArgs() + t.reset
				}
				if !inGroup {
					part = "[" + part + "]"
				}
			}
			parts = append(parts, part)
		}
		if inGroup {
			parts[start] = "[" + parts[start]
			for k := start; k < len(parts)-1; k++ {
				parts[k] += " |"
			}
			parts[len(parts)-1] += "]"
		}
	}
	if posStart < 0 {
		posStart = len(parts)
	}
	return parts, posStart
}

// usage is _format_usage: "usage: <prog> <parts>", wrapped.
func (ap *argParser) usage(t argTheme, width int) string {
	prefix := "usage: "
	prog := ap.prog
	parts, posStart := ap.usageParts(t)
	usage := strings.Join(append([]string{prog}, parts...), " ")
	textWidth := width
	if runeLen(prefix)+runeLen(decolor(usage)) > textWidth {
		optParts, posParts := parts[:posStart], parts[posStart:]
		getLines := func(parts []string, indent string, withPrefix bool) []string {
			var lines, line []string
			lineLen := runeLen(indent) - 1
			if withPrefix {
				lineLen = runeLen(prefix) - 1
			}
			for _, part := range parts {
				n := runeLen(decolor(part))
				if lineLen+1+n > textWidth && len(line) > 0 {
					lines = append(lines, indent+strings.Join(line, " "))
					line = nil
					lineLen = runeLen(indent) - 1
				}
				line = append(line, part)
				lineLen += n + 1
			}
			if len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
			}
			if withPrefix && len(lines) > 0 {
				lines[0] = lines[0][len(indent):]
			}
			return lines
		}
		var lines []string
		progLen := runeLen(decolor(prog))
		if float64(runeLen(prefix)+progLen) <= 0.75*float64(textWidth) {
			indent := strings.Repeat(" ", runeLen(prefix)+progLen+1)
			switch {
			case len(optParts) > 0:
				lines = getLines(append([]string{prog}, optParts...), indent, true)
				lines = append(lines, getLines(posParts, indent, false)...)
			case len(posParts) > 0:
				lines = getLines(append([]string{prog}, posParts...), indent, true)
			default:
				lines = []string{prog}
			}
		} else {
			indent := strings.Repeat(" ", runeLen(prefix))
			lines = getLines(append(slices.Clone(optParts), posParts...), indent, false)
			if len(lines) > 1 {
				lines = append(getLines(optParts, indent, false), getLines(posParts, indent, false)...)
			}
			lines = append([]string{prog}, lines...)
		}
		usage = strings.Join(lines, "\n")
	}
	usage = strings.TrimPrefix(usage, prog)
	return t.usage + prefix + t.reset + t.prog + prog + t.reset + usage + "\n\n"
}

// formatUsage is ArgumentParser.format_usage.
func (ap *argParser) formatUsage() string {
	return finishHelp(ap.usage(argColors(), helpWidth()))
}

// finishHelp is HelpFormatter.format_help's last step.
func finishHelp(s string) string {
	s = regexp.MustCompile(`\n\n\n+`).ReplaceAllString(s, "\n\n")
	return strings.Trim(s, "\n") + "\n"
}

// invocation is _format_action_invocation.
func (a *argAction) invocation(t argTheme) string {
	if len(a.opts) == 0 {
		return t.action + a.defaultMetavar() + t.reset
	}
	var opts []string
	for _, o := range a.opts {
		if isLongOption(o) {
			opts = append(opts, t.long+o+t.reset)
		} else {
			opts = append(opts, t.short+o+t.reset)
		}
	}
	s := strings.Join(opts, ", ")
	if a.nargs != 0 {
		s += " " + t.label + a.formatArgs() + t.reset
	}
	return s
}

// formatHelp is ArgumentParser.format_help with SortingHelpFormatter.
func (ap *argParser) formatHelp() string {
	t := argColors()
	width := helpWidth()
	maxHelpPosition := min(24, max(width-20, 4))
	// add_argument: the widest invocation, indented within its section.
	actionMax := 0
	sorted := make([][]*argAction, len(ap.groups))
	for i, g := range ap.groups {
		sorted[i] = slices.Clone(g.actions)
		slices.SortStableFunc(sorted[i], func(x, y *argAction) int { return slices.Compare(x.opts, y.opts) })
		for _, a := range sorted[i] {
			actionMax = max(actionMax, runeLen(decolor(a.invocation(t)))+2)
		}
	}
	text := func(s string, indent int) string {
		if s == "" {
			return ""
		}
		collapsed := strings.Join(strings.Fields(s), " ")
		return pyFill(collapsed, max(width-indent, 11), strings.Repeat(" ", indent)) + "\n\n"
	}
	var b strings.Builder
	b.WriteString(ap.usage(t, width))
	b.WriteString(text(ap.desc, 0))
	for i, g := range ap.groups {
		const indent = 2
		var items strings.Builder
		items.WriteString(text(g.desc, indent))
		for _, a := range sorted[i] {
			items.WriteString(ap.formatAction(a, t, width, indent, min(actionMax+2, maxHelpPosition)))
		}
		if items.Len() == 0 {
			continue
		}
		b.WriteString("\n" + t.heading + g.title + ":" + t.reset + "\n" + items.String() + "\n")
	}
	b.WriteString(text(ap.epilog, 0))
	return finishHelp(b.String())
}

// formatAction is _format_action.
func (ap *argParser) formatAction(a *argAction, t argTheme, width, indent, helpPosition int) string {
	helpWidth := max(width-helpPosition, 11)
	actionWidth := helpPosition - indent - 2
	header := a.invocation(t)
	plain := decolor(header)
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	indentFirst := 0
	switch {
	case a.help == "":
		b.WriteString(pad + header + "\n")
	case runeLen(plain) <= actionWidth:
		b.WriteString(pad + header + strings.Repeat(" ", actionWidth-runeLen(plain)) + "  ")
	default:
		b.WriteString(pad + header + "\n")
		indentFirst = helpPosition
	}
	if strings.TrimSpace(a.help) != "" {
		lines := pyWrap(strings.Join(strings.Fields(a.help), " "), helpWidth)
		for i, l := range lines {
			if i == 0 {
				b.WriteString(strings.Repeat(" ", indentFirst) + l + "\n")
			} else {
				b.WriteString(strings.Repeat(" ", helpPosition) + l + "\n")
			}
		}
	} else if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
