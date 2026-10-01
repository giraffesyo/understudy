package executor

import (
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"

	"github.com/giraffesyo/understudy/internal/actions"
	"github.com/giraffesyo/understudy/internal/agentproto"
	"github.com/giraffesyo/understudy/internal/playbook"
	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// setStatsValidArgs is set_stats' _VALID_ARGS.
var setStatsValidArgs = map[string]bool{"aggregate": true, "data": true, "per_host": true}

// runSetStats is the set_stats action: the stats it reports land in the
// run's custom stats when its result is recorded (statsOf).
func (r *Runner) runSetStats(task *playbook.Task, actx *actions.Context, args map[string]any) *agentproto.Result {
	var bad []string
	for k := range args {
		if !setStatsValidArgs[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		// ActionBase.validate_argument_spec's _VALID_ARGS check.
		sort.Strings(bad)
		res := agentproto.Fail("Invalid options for set_stats: %s", strings.Join(bad, ","))
		res.Origin = "raised"
		return res
	}
	data := yaml.NewOMap()
	perHost, aggregate := false, true
	if len(task.Args) > 0 {
		raw, has := args["data"]
		if !has {
			raw = map[string]any{}
		}
		m, ok := statsMapping(raw)
		if !ok {
			return &agentproto.Result{Failed: true, Msg: "The 'data' option needs to be a dictionary/hash", Origin: "action"}
		}
		for _, opt := range []string{"per_host", "aggregate"} {
			v, set := args[opt]
			if !set || v == nil {
				continue
			}
			b, _ := playbook.ParseBool(v) // boolean(strict=False): anything else is false
			if opt == "per_host" {
				perHost = b
			} else {
				aggregate = b
			}
		}
		// The names as written (and where), else the templated mapping's.
		written := task.Args["data"]
		_, literal := yaml.PlainMap(written)
		keys := task.ArgSubKeys["data"]
		if !literal || len(keys) != m.Len() {
			keys = nil
			for _, k := range m.Keys() {
				keys = append(keys, playbook.ArgKey{Name: k, Value: k})
			}
		}
		for _, k := range keys {
			name := k.Name
			if literal && strings.Contains(name, "{{") {
				if v, err := actx.Vars.At(k.Pos).TemplateString(name); err == nil {
					name = template.PyStr(v)
				}
			}
			if _, isStr := k.Value.(string); isStr || literal && name != k.Name {
				k.Value = name
			}
			if bad := invalidStatName(k, name); bad != nil {
				return bad
			}
			v, has := m.GetItem(name)
			if !has {
				v = m.Get(k.Name) // a name templated here
			}
			data.Set(name, v)
		}
	}
	return &agentproto.Result{Extra: map[string]any{"ansible_stats": map[string]any{
		"data": data, "per_host": perHost, "aggregate": aggregate,
	}}}
}

// statsMapping is a templated mapping as an ordered map.
func statsMapping(v any) (*yaml.OMap, bool) {
	if om, ok := v.(*yaml.OMap); ok {
		return om, true
	}
	m, ok := yaml.PlainMap(v)
	if !ok {
		return nil, false
	}
	out := yaml.NewOMap()
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		out.Set(k, m[k])
	}
	return out, true
}

// invalidStatName is validate_variable_name's failure for a set_stats
// data name, nil when the name is valid.
func invalidStatName(k playbook.ArgKey, name string) *agentproto.Result {
	desc := ""
	switch v := k.Value.(type) {
	case string:
		if playbook.ValidVariableName(name) {
			return nil
		}
		desc = "name " + template.PyRepr(name)
	case nil:
		desc = "name 'None' of type 'NoneType'"
	case bool:
		desc = fmt.Sprintf("name '%s' of type 'bool'", template.PyStr(v))
	case int64, int, *big.Int:
		desc = fmt.Sprintf("name '%s' of type 'int'", template.PyStr(v))
	case float64:
		desc = fmt.Sprintf("name '%s' of type 'float'", template.PyStr(v))
	default:
		desc = "name " + template.PyRepr(name)
	}
	msg := "Invalid variable " + desc + "."
	_, help := playbook.InvalidVariableName(name)
	res := agentproto.Fail("Task failed: %s", msg)
	res.Origin = "verbatim"
	res.ErrorChain = &agentproto.ErrorChain{Outer: "Task failed.", Inner: msg, Help: help,
		InnerFile: k.Pos.File, InnerLine: k.Pos.Line, InnerCol: k.Pos.Col}
	if k.Pos.Line == 0 {
		res.ErrorChain.InnerValue = name
	}
	return res
}

// statsOf lists the ansible_stats a recorded ok result carries: its
// own, or each loop item's.
func statsOf(res *agentproto.Result, loopItems []any) []map[string]any {
	var out []map[string]any
	if loopItems == nil {
		if st, ok := res.Extra["ansible_stats"].(map[string]any); ok {
			out = append(out, st)
		}
		return out
	}
	for _, it := range loopItems {
		if m, ok := asStringMap(it); ok {
			if st, ok := asStringMap(m["ansible_stats"]); ok {
				out = append(out, st)
			}
		}
	}
	return out
}

// applyStats is the strategy's custom stats handling for a set_stats
// result: each data name set (or, with aggregate, added) for the run or,
// with per_host, for each host the task ran for.
func (r *Runner) applyStats(host string, task *playbook.Task, stats map[string]any) {
	data, ok := statsMapping(stats["data"])
	if !ok {
		return
	}
	hosts := []string{"_run"}
	if b, _ := stats["per_host"].(bool); b {
		hosts = r.fanOut(host, task)
	}
	aggregate, _ := stats["aggregate"].(bool)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.custom == nil {
		r.custom = map[string]*yaml.OMap{}
	}
	for _, h := range hosts {
		cur := r.custom[h]
		if cur == nil {
			cur = yaml.NewOMap()
			r.custom[h] = cur
		}
		for _, k := range data.Keys() {
			v := data.Get(k)
			if prev, has := cur.GetItem(k); has && aggregate {
				if sum, ok := aggregateStat(prev, v); ok {
					cur.Set(k, sum)
				}
				continue
			}
			cur.Set(k, v)
		}
	}
}

// aggregateStat is AggregateStats.update_custom_stats for a name already
// set: values of a mismatching type are dropped (false), mappings merge
// and other values add as Python's + does.
func aggregateStat(prev, cur any) (any, bool) {
	if _, ok := yaml.PlainMap(prev); ok {
		if _, ok := yaml.PlainMap(cur); !ok {
			return nil, false
		}
		return mergeHashes(prev, cur), true
	}
	switch p := prev.(type) {
	case string:
		if c, ok := cur.(string); ok {
			return p + c, true
		}
	case []any:
		if c, ok := cur.([]any); ok {
			return append(slices.Clone(p), c...), true
		}
	case bool:
		// isinstance(x, bool) only holds for a bool; True + True is 2.
		if c, ok := cur.(bool); ok {
			return boolInt(p) + boolInt(c), true
		}
	case float64:
		if c, ok := cur.(float64); ok {
			return p + c, true
		}
	case int64:
		switch c := cur.(type) {
		case int64:
			return p + c, true
		case int:
			return p + int64(c), true
		case bool:
			return p + boolInt(c), true
		}
	case int:
		switch c := cur.(type) {
		case int64:
			return int64(p) + c, true
		case int:
			return int64(p + c), true
		case bool:
			return int64(p) + boolInt(c), true
		}
	}
	return nil, false
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// CustomStats are the run's set_stats results by host ("_run" for the
// run-wide ones), as ansible-core's AggregateStats.custom.
func (r *Runner) CustomStats() map[string]*yaml.OMap {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.custom
}
