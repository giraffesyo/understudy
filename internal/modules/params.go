package modules

import (
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/modules/args"
)

// parseModuleArgs validates raw against spec as AnsibleModule does,
// failing unknown parameters with its message for the module as invoked
// (the executor names a directly invoked module as the task wrote it).
// Hidden arguments (a leading underscore) are not checked.
func parseModuleArgs(spec args.Spec, raw map[string]any, module string) (*args.Parsed, *agentproto.Result) {
	legal := map[string]bool{}
	var names, aliases []string
	for n, def := range spec {
		legal[n] = true
		names = append(names, n)
		for _, a := range def.Aliases {
			legal[a] = true
			aliases = append(aliases, a)
		}
	}
	var unknown []string
	clean := make(map[string]any, len(raw))
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue
		}
		clean[k] = v
		if !legal[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		sort.Strings(names)
		sort.Strings(aliases)
		supported := strings.Join(names, ", ")
		if len(aliases) > 0 {
			supported += " (" + strings.Join(aliases, ", ") + ")"
		}
		return nil, agentproto.Fail("Unsupported parameters for (%s) module: %s. Supported parameters include: %s.",
			module, strings.Join(unknown, ", "), supported)
	}
	p, err := spec.Parse(clean)
	if err != nil {
		return nil, agentproto.Fail("%v", err)
	}
	return p, nil
}

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
