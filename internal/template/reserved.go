package template

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// reservedNames is ansible-core's _RESERVED_NAMES (vars/reserved.py): the
// template globals and every Play, Role, Block and Task keyword, less
// gather_subset; 'vars' is never warned about.
var reservedNames = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`action always any_errors_fatal args async async_val become
		become_exe become_flags become_method become_user block changed_when check_mode collections
		connection cycler debugger delay delegate_facts delegate_to dict diff environment fact_path
		failed_when force_handlers gather_facts gather_timeout handlers hosts ignore_errors
		ignore_unreachable joiner lipsum local_action lookup loop loop_control loop_with
		max_fail_percentage module_defaults name namespace no_log notify now omit order poll port
		post_tasks pre_tasks q query range register remote_user rescue retries roles run_once serial
		strategy tags tasks throttle timeout undef until validate_argspec vars_files vars_prompt when
		with_`) {
		reservedNames[n] = true
	}
}

// IsReservedName reports whether a variable name collides with a name
// ansible-core reserves (warn_if_reserved).
func IsReservedName(name string) bool { return reservedNames[name] }

// KeyOrigin is where a variable's name was written, for a warning about
// the variable.
type KeyOrigin struct {
	Name string
	// File and Line locate it; Col 0 is an INI inventory line, shown
	// underlined whole.
	File      string
	Line, Col int
	// Label stands in for a source location ("<CLI option '-e'>"); with
	// neither, the origin is unknown.
	Label string
}

// ReservedWarning is warn_if_reserved's warning for a variable named at o,
// as Display.warning formats it.
func ReservedWarning(o KeyOrigin) string {
	head := fmt.Sprintf("[WARNING]: Found variable using reserved name %s.\n", PyRepr(o.Name))
	switch {
	case o.File != "" && o.Line > 0 && o.Col > 0:
		return head + fmt.Sprintf("Origin: %s:%d:%d\n\n%s\n", o.File, o.Line, o.Col, SourceExcerpt(o.File, o.Line, o.Col))
	case o.File != "" && o.Line > 0:
		return head + fmt.Sprintf("Origin: %s:%d\n\n%s\n", o.File, o.Line, lineExcerpt(o.File, o.Line))
	case o.Label != "":
		return head + fmt.Sprintf("Origin: %s\n\n%s\n\n", o.Label, o.Name)
	}
	return head + fmt.Sprintf("Origin: <unknown>\n\n%s\n\n", o.Name)
}

// lineExcerpt is the source context of an origin without a column: the
// lines up to line, with that line underlined.
func lineExcerpt(file string, line int) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	lines := SourceLines(string(data))
	if line > len(lines) {
		return "(source not shown: file truncated)\n"
	}
	width := len(strconv.Itoa(line))
	var b strings.Builder
	for n := max(1, line-2); n <= line; n++ {
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("%*d %s", width, n, lines[n-1]), " \t\r"))
	}
	src := strings.TrimRight(lines[line-1], " \t\r")
	fmt.Fprintf(&b, "%s%s\n", strings.Repeat(" ", width+1), strings.Repeat("^", len([]rune(src))))
	return b.String()
}
