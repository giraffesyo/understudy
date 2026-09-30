package modules

import "sort"

// adhocParams are the parameters of modules that read their arguments
// without an args.Spec (free-form parsing), for tools/paramaudit.
var adhocParams = map[string][]string{
	"command":      {"_raw_params", "_uses_shell", "argv", "chdir", "cmd", "creates", "executable", "expand_argument_vars", "removes", "stdin", "stdin_add_newline", "strip_empty_ends"},
	"shell":        {"_raw_params", "_uses_shell", "argv", "chdir", "cmd", "creates", "executable", "expand_argument_vars", "removes", "stdin", "stdin_add_newline", "strip_empty_ends"},
	"ping":         {"data"},
	"async_status": {"jid", "mode", "_async_dir"},
}

// Names lists every registered module name, short names and FQCNs.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AcceptedParams lists the parameter names (aliases included) a module
// accepts; ok is false when the module declares none.
func AcceptedParams(name string) (params []string, ok bool) {
	if spec, found := specs[name]; found {
		for n, def := range spec {
			params = append(params, n)
			params = append(params, def.Aliases...)
		}
		sort.Strings(params)
		return params, true
	}
	short := name
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			short = name[i+1:]
			break
		}
	}
	if p, found := adhocParams[short]; found {
		return append([]string{}, p...), true
	}
	return nil, false
}
