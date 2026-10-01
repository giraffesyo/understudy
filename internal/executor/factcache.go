package executor

import (
	"maps"
	"slices"
	"strings"

	"github.com/giraffesyo/understudy/internal/factcache"
	"github.com/giraffesyo/understudy/internal/playbook"
)

// The fact cache is VariableManager's: every host's facts (what modules
// return as ansible_facts, and set_fact's cacheable ones) live in the
// cache plugin, and a host's variables read them from there (namespaced
// under ansible_facts, and injected at top level). The runner's variable
// store holds the facts layer; the cache persists it (jsonfile).

// gatheredKey marks facts the gather_facts action collected, which the
// smart gathering policy looks for.
const gatheredKey = "_ansible_facts_gathered"

// openFactCache is the variable manager's cache_loader.get(CACHE_PLUGIN):
// a plugin that cannot load is a warning, and the memory plugin stands
// in.
func (r *Runner) openFactCache() {
	s := r.Opts.FactCache
	if s.Plugin == "" {
		s.Plugin = "memory"
	}
	c, err := factcache.Open(s, r.warn)
	if err != nil {
		r.warn(err.Error())
		c = factcache.Memory()
	}
	r.facts = c
	r.Store.FactLoader = r.loadCachedFacts
	if r.Opts.FlushCache {
		// CLI._flush_cache: localhost and every inventory host.
		c.Delete("localhost")
		for _, h := range r.Inv.HostNames() {
			c.Delete(h)
		}
	}
}

// loadCachedFacts installs a host's cached facts in the variable store
// the first time its variables are read. A cache file that cannot be
// read ends the run.
func (r *Runner) loadCachedFacts(host string) {
	f, err := r.facts.Get(host)
	if err != nil {
		r.fatal(err)
		return
	}
	if f != nil && f.Len() > 0 {
		r.installFacts(host, f.Map())
	}
}

// installFacts records facts in host's facts layer, both prefixed at top
// level (inject_facts_as_vars, internal keys cleaned off) and under the
// ansible_facts dict (namespace_facts), merged with the facts already
// there (host_cache |= facts).
func (r *Runner) installFacts(host string, facts map[string]any) {
	top := make(map[string]any, len(facts))
	stripped := make(map[string]any, len(facts))
	for k, v := range facts {
		if !strings.HasPrefix(k, "_ansible_") {
			top[k] = v
		}
		if k == "ansible_local" {
			stripped[k] = v // namespace_facts keeps ansible_local as-is
			continue
		}
		stripped[strings.TrimPrefix(k, "ansible_")] = v
	}
	r.Store.SetFacts(host, r.deprecatedFacts(top))
	merged := stripped
	if old, ok := r.Store.Fact(host, "ansible_facts"); ok {
		if m, ok := asStringMap(old); ok {
			merged = maps.Clone(m)
			maps.Copy(merged, stripped)
		}
	}
	r.Store.SetFacts(host, map[string]any{"ansible_facts": merged})
}

// setHostFacts is VariableManager.set_host_facts: facts update the
// host's cached ones (host_cache |= facts, saved back to the cache,
// which a persistent plugin writes out even when nothing changed) and
// its variables.
func (r *Runner) setHostFacts(host string, facts *factcache.Facts) {
	cur, err := r.facts.Get(host)
	if err != nil {
		r.fatal(err)
		return
	}
	if cur == nil {
		cur = facts
	} else {
		cur.Update(facts)
	}
	r.facts.Set(host, cur)
	if facts.Len() > 0 {
		r.installFacts(host, facts.Map())
	}
}

// moduleFacts are a result's facts as the cache stores them: the
// module's (whose order understudy does not keep: by name), then the
// interpreter discovery's and the gather_facts action's marker, which
// ansible-core adds after them.
func moduleFacts(facts map[string]any, gathered bool) *factcache.Facts {
	f := factcache.NewFacts()
	for _, k := range slices.Sorted(maps.Keys(facts)) {
		if k == discoveredKey || k == gatheredKey {
			continue
		}
		f.Set(k, facts[k], facts[k])
	}
	if v, ok := facts[discoveredKey]; ok {
		f.Set(discoveredKey, v, v)
	}
	if gathered {
		f.Set(gatheredKey, true, true)
	}
	return f
}

// factsGathered is _facts_gathered_for_host: the gather_facts action
// ran for host (as the cache remembers).
func (r *Runner) factsGathered(host string) bool {
	f, err := r.facts.Get(host)
	if err != nil || f == nil {
		return false
	}
	v, _ := f.Get(gatheredKey)
	return pyTruthy(v)
}

func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int64:
		return t != 0
	case float64:
		return t != 0
	}
	return true
}

// gathers reports whether the play gathers facts for host, by the
// DEFAULT_GATHERING policy: implicit unless gather_facts is false,
// explicit only when it is true, smart as implicit for a host whose
// facts were not gathered yet.
func (r *Runner) gathers(play *playbook.Play, host string) bool {
	implied := play.GatherFacts == nil || *play.GatherFacts
	switch r.Opts.Gathering {
	case "explicit":
		return play.GatherFacts != nil && *play.GatherFacts
	case "smart":
		return implied && !r.factsGathered(host)
	}
	return implied
}

// registersEmptyFacts lists the actions that register an empty
// cacheable facts layer (keeping their result's ansible_facts out of the
// cache): saving a host's facts unchanged.
func registersEmptyFacts(module string) bool {
	switch module {
	case "debug", "include_vars", "set_fact":
		return true
	}
	return false
}
