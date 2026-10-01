package executor

import (
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/vars"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// A registered result's values carry their origin and trust as
// ansible-core's tagged values do: an action that runs on the controller
// passes back its arguments' values (debug's msg, set_fact's facts), which
// keep where they were written and whether that source is trusted;
// everything else in a result (a module's output, the text an action
// plugin writes itself) has no origin and is not trusted, so a template
// or expression made of it fails the trust check.

// argOrigins are the origins of the values an action run on the
// controller passes back from its arguments, by result key (a nested map
// for the keys under a result key).
func argOrigins(task *playbook.Task, vctx *vars.Context, res *agentproto.Result) map[string]any {
	arg := func(name string) (template.OriginRef, bool) {
		raw, ok := task.Args[name]
		if !ok {
			return template.OriginRef{}, false
		}
		return vctx.ValueOrigin(raw, argPos(task, name)), true
	}
	out := map[string]any{}
	switch task.Module {
	case "debug":
		if ref, ok := arg("msg"); ok {
			out["msg"] = ref
		} else if v, ok := task.Args["var"].(string); ok && !template.HasTemplate(v) {
			// The value of the variable (or expression) named.
			out[v] = vctx.ValueOrigin("{{ "+v+" }}", argPos(task, "var"))
		}
	case "fail":
		if ref, ok := arg("msg"); ok {
			out["msg"] = ref
		}
	case "assert":
		key := "success_msg"
		if res.Failed {
			key = "fail_msg"
			if _, ok := task.Args[key]; !ok {
				key = "msg"
			}
		}
		if ref, ok := arg(key); ok {
			out["msg"] = ref
		}
	case "set_fact":
		facts := map[string]any{}
		for name := range task.Args {
			if name == "cacheable" {
				continue
			}
			ref, _ := arg(name)
			facts[name] = ref
		}
		out["ansible_facts"] = facts
	case "set_stats":
		if ref, ok := arg("data"); ok {
			out["ansible_stats"] = map[string]any{"data": ref}
		}
	}
	return out
}

// registeredOrigin is the origin a registered value carries: its values'
// own where origins gives them, else none and untrusted.
func registeredOrigin(v any, origins map[string]any) template.OriginRef {
	return template.OriginRef{Raw: originTree(v, origins), HasRaw: true}
}

// builtTree is an origin tree already built (a loop's item results').
type builtTree struct{ tree any }

// originTree mirrors v with each value's origin: override's (an origin,
// or a map of origins by key) where it has one, else an untrusted one.
func originTree(v any, override any) any {
	switch o := override.(type) {
	case template.OriginRef:
		return o
	case builtTree:
		return o.tree
	}
	keys, _ := override.(map[string]any)
	child := func(k string, item any) any {
		if o, ok := keys[k]; ok {
			return originTree(item, o)
		}
		return originTree(item, nil)
	}
	switch t := v.(type) {
	case *yaml.OMap:
		out := yaml.NewOMap()
		for _, k := range t.Keys() {
			out.Set(k, child(k, t.Get(k)))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = child(k, item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = originTree(item, nil)
		}
		return out
	case vars.Final:
		return originTree(t.V, override)
	}
	return template.OriginRef{Untrusted: true}
}
