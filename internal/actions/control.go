package actions

import (
	"context"
	"fmt"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/template"
)

func init() {
	Register("debug", actionFunc(runDebug))
	Register("set_fact", actionFunc(runSetFact))
	Register("fail", actionFunc(runFail))
	Register("assert", actionFunc(runAssert))
}

type actionFunc func(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result

func (f actionFunc) Run(ctx context.Context, actx *Context, args map[string]any, freeForm string) *agentproto.Result {
	return f(ctx, actx, args, freeForm)
}

// runDebug implements debug: msg=/var= with verbosity gating.
func runDebug(_ context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	if v, ok := args["verbosity"]; ok {
		if lvl, isNum := toInt(v); isNum && actx.Verbosity < int(lvl) {
			return &agentproto.Result{
				Skipped: true,
				Msg:     "Verbosity threshold not met.",
			}
		}
	}
	if varName, ok := args["var"].(string); ok {
		// debug var= evaluates the NAME as an expression against host vars.
		val, err := actx.Vars.EvalExpr(varName)
		if err != nil {
			if ue, isUndef := err.(*template.UndefinedError); isUndef {
				// ansible-core 2.19+ renders the template error in place.
				val = fmt.Sprintf("<< error 1 - '%s' is undefined >>", ue.Name)
			} else {
				return agentproto.Fail("debug var=%s: %v", varName, err)
			}
		}
		return &agentproto.Result{VerboseAlways: true, Extra: map[string]any{varName: jsonSafe(val)}}
	}
	msg := "Hello world!"
	if m, ok := args["msg"]; ok {
		if m == nil {
			return &agentproto.Result{VerboseAlways: true}
		}
		if s, isStr := m.(string); isStr {
			msg = s
		} else {
			return &agentproto.Result{VerboseAlways: true, Extra: map[string]any{"msg": jsonSafe(m)}}
		}
	}
	return &agentproto.Result{VerboseAlways: true, Msg: msg}
}

func runSetFact(_ context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	if len(args) == 0 {
		return agentproto.Fail("set_fact requires at least one key=value pair")
	}
	facts := make(map[string]any, len(args))
	for k, v := range args {
		if k == "cacheable" {
			continue
		}
		actx.SetFact(k, v)
		facts[k] = jsonSafe(v)
	}
	return &agentproto.Result{AnsibleFacts: facts}
}

func runFail(_ context.Context, _ *Context, args map[string]any, _ string) *agentproto.Result {
	msg := "Failed as requested from task"
	if m, ok := args["msg"].(string); ok && m != "" {
		msg = m
	}
	return &agentproto.Result{Failed: true, Msg: msg}
}

func runAssert(_ context.Context, actx *Context, args map[string]any, _ string) *agentproto.Result {
	that, ok := args["that"]
	if !ok {
		return agentproto.Fail("assert requires the 'that' argument")
	}
	var exprs []string
	switch t := that.(type) {
	case string:
		exprs = []string{t}
	case []any:
		for _, e := range t {
			s, isStr := e.(string)
			if !isStr {
				return agentproto.Fail("assert 'that' entries must be strings")
			}
			exprs = append(exprs, s)
		}
	default:
		return agentproto.Fail("assert 'that' must be a string or list of strings")
	}
	for _, expr := range exprs {
		ok, err := actx.Vars.EvalWhen([]string{expr})
		if err != nil {
			return agentproto.Fail("assert: error evaluating %q: %v", expr, err)
		}
		if !ok {
			msg := "Assertion failed"
			if m, has := args["fail_msg"].(string); has && m != "" {
				msg = m
			} else if m, has := args["msg"].(string); has && m != "" {
				msg = m
			}
			return &agentproto.Result{
				Failed: true,
				Msg:    msg,
				Extra:  map[string]any{"assertion": expr, "evaluated_to": false},

				VerboseAlways: true,
			}
		}
	}
	msg := "All assertions passed"
	if m, has := args["success_msg"].(string); has && m != "" {
		msg = m
	}
	return &agentproto.Result{VerboseAlways: true, Msg: msg}
}

func toInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	}
	return 0, false
}

// jsonSafe converts engine values (Omit, Undefined, UnsafeString) into
// plain JSON-encodable values for result payloads.
func jsonSafe(v any) any {
	// Engine values (ordered maps, unsafe strings, lazy ranges) become plain
	// JSON-shaped values; ints stay ints and floats stay floats.
	return template.Plain(v)
}
