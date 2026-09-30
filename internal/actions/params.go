package actions

import (
	"sort"

	"github.com/giraffesyo/understudy/internal/modules"
)

// actionParams are the parameters the control-side actions read, for
// tools/paramaudit. An action forwarding to a same-named module (copy,
// template, unarchive, script) also accepts what that module accepts.
var actionParams = map[string][]string{
	"debug":                  {"msg", "var", "verbosity"},
	"fail":                   {"msg"},
	"assert":                 {"that", "fail_msg", "msg", "success_msg", "quiet"},
	"raw":                    {"_raw_params"},
	"script":                 {"_raw_params", "cmd", "creates", "removes", "chdir", "executable"},
	"validate_argument_spec": {"argument_spec", "provided_arguments"},
}

// Names lists the control-side action names.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AcceptedParams lists the parameter names an action or module accepts;
// ok is false when nothing is declared (free-form actions like set_fact).
func AcceptedParams(name string) ([]string, bool) {
	var out []string
	declared := false
	if p, ok := actionParams[name]; ok {
		out, declared = append(out, p...), true
	}
	switch name {
	case "copy", "template":
		for k := range forwardedFileArgs {
			out = append(out, k)
		}
		for k := range fileActionArgs[name] {
			out = append(out, k)
		}
		declared = true
	}
	if registry[name] == nil || name == "unarchive" {
		if p, ok := modules.AcceptedParams(name); ok {
			out, declared = append(out, p...), true
		}
	}
	sort.Strings(out)
	return out, declared
}
