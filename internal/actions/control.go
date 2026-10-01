package actions

import (
	"context"
	"errors"
	"fmt"

	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
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
		// debug var= evaluates the NAME as an expression against host
		// vars, an undefined value rendered in place and warned about.
		val, markers, err := actx.Vars.At(actx.ArgPos["var"]).EvalExprReplacing(varName)
		if err == nil && actx.Warn != nil {
			for _, w := range template.MarkerWarnings(markers) {
				actx.Warn(warningBlock(w.Msg, w.Pos))
			}
		}
		if err != nil {
			var re *template.RecursionError
			if errors.As(err, &re) {
				// A recursive value cannot be finalized.
				inner := "Error while resolving `var` expression: " + re.Error()
				res := agentproto.Fail("Task failed: %s", inner)
				res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: inner}
				if p, has := actx.ArgPos["var"]; has {
					res.ErrorChain.InnerFile, res.ErrorChain.InnerLine, res.ErrorChain.InnerCol = p.File, p.Line, p.Col
				}
				return res
			} else if cause, ok := template.Cause(err); ok {
				inner := "Error while resolving `var` expression: " + cause
				res := agentproto.Fail("Task failed: %s", inner)
				res.Origin = "verbatim"
				res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: inner}
				if p, has := actx.ArgPos["var"]; has {
					res.ErrorChain.InnerFile, res.ErrorChain.InnerLine, res.ErrorChain.InnerCol = p.File, p.Line, p.Col
				}
				return res
			} else {
				return agentproto.Fail("debug var=%s: %v", varName, err)
			}
		}
		return &agentproto.Result{VerboseAlways: true, Extra: map[string]any{varName: jsonSafe(val)}}
	}
	msg := "Hello world!"
	if m, ok := args["msg"]; ok {
		if m == nil {
			return &agentproto.Result{VerboseAlways: true, Extra: map[string]any{"msg": nil}}
		}
		if s, isStr := m.(string); isStr && s == "" {
			return &agentproto.Result{VerboseAlways: true, Extra: map[string]any{"msg": ""}}
		} else if isStr {
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
	// that: _check_type_list_strict (a non-list becomes a one-item list);
	// each entry is a conditional: a bool is taken as is, a string is
	// evaluated, and anything else is a broken conditional.
	entries, isList := that.([]any)
	if !isList {
		entries = []any{that}
	}
	for _, e := range entries {
		var ok bool
		switch t := e.(type) {
		case bool:
			ok = t
		case string:
			var err error
			var pos template.Position
			if !isList {
				pos = actx.ArgPos["that"]
			} else if file, line, col, ok := yaml.Origin(t); ok {
				// Each listed conditional reports from its own entry.
				pos = template.Position{File: file, Line: line, Col: col}
			}
			vctx := actx.Vars
			if pos.File != "" {
				vctx = vctx.At(pos)
			}
			ok, err = vctx.EvalWhen([]string{t})
			if err != nil {
				cause := template.ConditionalCause(err)
				res := agentproto.Fail("Task failed: %s", cause)
				res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: cause,
					InnerFile: pos.File, InnerLine: pos.Line, InnerCol: pos.Col}
				if be, broken := template.IsBrokenConditional(err); broken {
					res.ErrorChain.Help = be.Help
				}
				return res
			}
		default:
			res := agentproto.Fail("Task failed: Conditional expressions must be strings.")
			res.ErrorChain = &agentproto.ErrorChain{
				Outer: "Task failed.",
				Inner: "Conditional expressions must be strings.",
				Help:  "Broken conditionals can be temporarily allowed with the `ALLOW_BROKEN_CONDITIONALS` configuration option.",
			}
			if p, has := actx.ArgPos["that"]; has && !isList {
				res.ErrorChain.InnerFile, res.ErrorChain.InnerLine, res.ErrorChain.InnerCol = p.File, p.Line, p.Col
			}
			return res
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
				Extra:  map[string]any{"assertion": e, "evaluated_to": false},

				VerboseAlways: !isTruthy(args["quiet"]),
			}
		}
	}
	msg := "All assertions passed"
	if m, has := args["success_msg"].(string); has && m != "" {
		msg = m
	}
	return &agentproto.Result{VerboseAlways: !isTruthy(args["quiet"]), Msg: msg}
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

// warningBlock is Display's warning with its origin and source excerpt.
func warningBlock(msg string, pos template.Position) string {
	if pos.File == "" {
		return "[WARNING]: " + msg + "\n"
	}
	return fmt.Sprintf("[WARNING]: %s\nOrigin: %s:%d:%d\n\n%s\n", msg, pos.File, pos.Line, pos.Col,
		template.SourceExcerpt(pos.File, pos.Line, pos.Col))
}
