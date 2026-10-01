// Package vars implements Ansible's layered variable precedence with lazy,
// use-time template resolution. This is the M3 subset — inventory layers,
// magic vars, and hostvars land with the full store in M4/M5; the Layer
// ordering already reserves their slots.
package vars

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/giraffesyo/understudy/internal/template"
	"github.com/giraffesyo/understudy/internal/yaml"
)

// Layer identifies one precedence level; higher wins.
type Layer int

const (
	LRoleDefaults Layer = iota
	LInventoryGroupVars
	LGroupVarsAll
	LGroupVars
	LInventoryHostVars
	LHostVars
	LPlayVars
	LPlayVarsFiles
	LRoleVars // role vars/main.yml: outrank play vars (Ansible precedence)
	LTaskVars
	LFacts
	LIncludeVars // include_vars: raw (templated on use); persists across plays
	LHostFacts   // set_fact + register; persists across plays
	LExtraVars   // -e always wins
	layerCount
)

// Store holds raw (untemplated) variables per layer. Host-scoped layers key
// by hostname; global layers use the "" key.
type Store struct {
	mu     sync.RWMutex
	layers [layerCount]map[string]map[string]any // layer -> scope key -> vars
	// origins is where each layer's values came from, where known (a
	// broken conditional names its result's origin).
	origins [layerCount]map[string]map[string]valueOrigin
	// order is each layer's variable names in the order they were
	// first set.
	order  [layerCount]map[string][]string
	engine *template.Engine

	// VaultDecrypt decrypts a !vault-tagged value at use time. Nil means no
	// vault password is configured; encountering an encrypted value errors.
	VaultDecrypt func(yaml.VaultedString) (string, error)

	// FactLoader, when set, installs a host's cached facts (SetFacts) the
	// first time its variables are read, as get_vars reads the fact
	// cache.
	FactLoader  func(host string)
	factsMu     sync.Mutex
	factsLoaded map[string]bool
}

// EnsureFacts installs host's cached facts now, if they are not yet.
func (s *Store) EnsureFacts(host string) { s.loadFacts(host) }

// loadFacts runs FactLoader for host once.
func (s *Store) loadFacts(host string) {
	if s.FactLoader == nil || host == "" {
		return
	}
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	if s.factsLoaded[host] {
		return
	}
	if s.factsLoaded == nil {
		s.factsLoaded = map[string]bool{}
	}
	s.factsLoaded[host] = true
	s.FactLoader(host)
}

func NewStore(engine *template.Engine) *Store {
	return &Store{engine: engine}
}

func (s *Store) set(layer Layer, scope string, vars map[string]any) {
	s.setOrdered(layer, scope, vars, nil)
}

// setOrdered is set with the order of vars' keys (those order leaves
// out follow, by where they were written, then by name): a variable keeps
// the place it was first set at, as a Python dict's key does.
func (s *Store) setOrdered(layer Layer, scope string, vars map[string]any, order []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layers[layer] == nil {
		s.layers[layer] = map[string]map[string]any{}
	}
	dst := s.layers[layer][scope]
	if dst == nil {
		dst = map[string]any{}
		s.layers[layer][scope] = dst
	}
	if s.origins[layer] == nil {
		s.origins[layer] = map[string]map[string]valueOrigin{}
	}
	orig := s.origins[layer][scope]
	if orig == nil {
		orig = map[string]valueOrigin{}
		s.origins[layer][scope] = orig
	}
	if s.order[layer] == nil {
		s.order[layer] = map[string][]string{}
	}
	var added []string
	for k, v := range vars {
		if _, had := dst[k]; !had {
			added = append(added, k)
		}
		dst[k] = v
		if file, line, col, ok := yaml.ChildOrigin(vars, k); ok {
			orig[k] = valueOrigin{pos: template.Position{File: file, Line: line, Col: col}}
		} else {
			delete(orig, k)
		}
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		if _, dup := rank[k]; !dup {
			rank[k] = i
		}
	}
	sort.Slice(added, func(i, j int) bool {
		a, b := added[i], added[j]
		ra, aRanked := rank[a]
		rb, bRanked := rank[b]
		if aRanked != bRanked {
			return aRanked
		}
		if aRanked {
			return ra < rb
		}
		oa, aKnown := orig[a]
		ob, bKnown := orig[b]
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown && oa.pos != ob.pos {
			if oa.pos.File != ob.pos.File {
				return oa.pos.File < ob.pos.File
			}
			if oa.pos.Line != ob.pos.Line {
				return oa.pos.Line < ob.pos.Line
			}
			return oa.pos.Col < ob.pos.Col
		}
		return a < b
	})
	s.order[layer][scope] = append(s.order[layer][scope], added...)
}

// valueOrigin is where a variable's value came from; inherit: its items
// too (a JSON file's values, a CLI option's).
type valueOrigin struct {
	pos     template.Position
	inherit bool
}

// SetValueOrigins records where the values of a layer's variables came
// from, for sources other than YAML (an INI inventory line, -e): pos
// with Line 0 names a file or a description ("<CLI option '-e'>"), the
// origin of their items too.
func (s *Store) SetValueOrigins(layer Layer, scope string, origins map[string]template.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.origins[layer] == nil {
		s.origins[layer] = map[string]map[string]valueOrigin{}
	}
	if s.origins[layer][scope] == nil {
		s.origins[layer][scope] = map[string]valueOrigin{}
	}
	for k, p := range origins {
		s.origins[layer][scope][k] = valueOrigin{pos: p, inherit: p.Line == 0}
	}
}

// SetPlayVars replaces all play-scoped layers (called at play start).
func (s *Store) SetPlayVars(vars map[string]any) {
	s.mu.Lock()
	s.layers[LPlayVars] = nil
	s.layers[LPlayVarsFiles] = nil
	s.layers[LRoleDefaults] = nil
	s.layers[LRoleVars] = nil
	for _, l := range []Layer{LPlayVars, LPlayVarsFiles, LRoleDefaults, LRoleVars} {
		s.order[l] = nil
	}
	s.origins[LPlayVars] = nil
	s.origins[LPlayVarsFiles] = nil
	s.origins[LRoleDefaults] = nil
	s.origins[LRoleVars] = nil
	s.mu.Unlock()
	if vars != nil {
		s.set(LPlayVars, "", vars)
	}
}

// AddRoleDefaults merges one role's defaults (lowest precedence).
func (s *Store) AddRoleDefaults(vars map[string]any) { s.set(LRoleDefaults, "", vars) }

// AddRoleVars merges one role's vars (outrank play vars).
func (s *Store) AddRoleVars(vars map[string]any) { s.set(LRoleVars, "", vars) }

// AddVarsFile merges one vars_files result.
func (s *Store) AddVarsFile(vars map[string]any) { s.set(LPlayVarsFiles, "", vars) }

// SetExtraVars installs -e vars (highest precedence).
func (s *Store) SetExtraVars(vars map[string]any) { s.set(LExtraVars, "", vars) }

// Final marks a value that is already the product of templating (a
// registered result, a set_fact value, a gathered fact, a loop item):
// reading it never templates it again, as in Ansible, where such values
// are unsafe/final — command output containing "{{" stays literal.
type Final struct {
	V any
	// Origin is where a set_fact value came from (nil: unknown, as a
	// registered result's is).
	Origin *template.OriginRef
}

// SetHostFact records set_fact/register results for one host.
func (s *Store) SetHostFact(host, name string, value any) {
	s.set(LHostFacts, host, map[string]any{name: Final{V: value}})
}

// SetHostFactOrigin is SetHostFact for a value with a known origin.
func (s *Store) SetHostFactOrigin(host, name string, value any, origin template.OriginRef) {
	s.set(LHostFacts, host, map[string]any{name: Final{V: value, Origin: &origin}})
}

// SetIncludeVars records include_vars results for one host. Unlike
// set_fact values they stay raw: templates inside are resolved on use.
func (s *Store) SetIncludeVars(host string, vars map[string]any) {
	s.set(LIncludeVars, host, vars)
}

// SetHostVarRaw registers a value that is templated on use (a registered
// include_vars result, whose data Ansible keeps trusted).
func (s *Store) SetHostVarRaw(host, name string, value any) {
	s.set(LHostFacts, host, map[string]any{name: value})
}

// SetInventoryVars installs a host's merged inventory vars (group vars in
// depth order + host vars — the inventory package pre-merges them), in
// order (as ansible-core's get_vars combines them).
func (s *Store) SetInventoryVars(host string, vars map[string]any, order []string) {
	s.mu.Lock()
	if s.layers[LHostVars] != nil {
		delete(s.layers[LHostVars], host)
		delete(s.order[LHostVars], host)
	}
	s.mu.Unlock()
	s.setOrdered(LHostVars, host, vars, order)
}

// RawHostVar fetches an untemplated var for one host (behavioral
// connection vars like ansible_connection/ansible_host).
func (s *Store) RawHostVar(host, name string) (any, bool) {
	flat := s.flatten(host)
	v, ok := flat[name]
	if f, isFinal := v.(Final); isFinal {
		v = f.V
	}
	return template.Undeprecate(v), ok
}

// SetFacts records gathered facts for one host.
func (s *Store) SetFacts(host string, facts map[string]any) {
	final := make(map[string]any, len(facts))
	for k, v := range facts {
		final[k] = Final{V: v}
	}
	s.set(LFacts, host, final)
}

// Fact is one of a host's gathered facts (as SetFacts recorded it).
func (s *Store) Fact(host, name string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.layers[LFacts][host][name]
	if f, isFinal := v.(Final); isFinal {
		return f.V, ok
	}
	return v, ok
}

// ClearFacts drops a host's gathered facts (meta: clear_facts); set_fact
// values are not cached facts and survive, as in Ansible.
func (s *Store) ClearFacts(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layers[LFacts] != nil {
		delete(s.layers[LFacts], host)
		delete(s.order[LFacts], host)
	}
}

// flatten merges all layers for one host in precedence order.
func (s *Store) flatten(host string) map[string]any {
	return s.flattenWith(host, nil, nil)
}

// flattenWith is flatten with a private role's defaults and vars at
// their layers' precedence (after the play-wide ones).
func (s *Store) flattenWith(host string, roleDefaults, roleVars []map[string]any) map[string]any {
	out, _ := s.flattenOrigins(host, roleDefaults, roleVars)
	return out
}

// flattenOrigins is flattenWith with where each value came from.
func (s *Store) flattenOrigins(host string, roleDefaults, roleVars []map[string]any) (map[string]any, map[string]valueOrigin) {
	return s.flattenLayers(host, roleDefaults, roleVars, nil)
}

// flattenLayers is flattenOrigins without the layers skip names.
func (s *Store) flattenLayers(host string, roleDefaults, roleVars []map[string]any, skip map[Layer]bool) (map[string]any, map[string]valueOrigin) {
	s.loadFacts(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{}
	origins := map[string]valueOrigin{}
	put := func(k string, v any, o valueOrigin, known bool) {
		out[k] = v
		if known {
			origins[k] = o
		} else {
			delete(origins, k)
		}
	}
	for layer := Layer(0); layer < layerCount; layer++ {
		if skip[layer] {
			continue
		}
		if scopes := s.layers[layer]; scopes != nil {
			scopeOrigins := s.origins[layer]
			for k, v := range scopes[""] {
				o, known := scopeOrigins[""][k]
				put(k, v, o, known)
			}
			if host != "" {
				for k, v := range scopes[host] {
					o, known := scopeOrigins[host][k]
					put(k, v, o, known)
				}
			}
		}
		var private []map[string]any
		switch layer {
		case LRoleDefaults:
			private = roleDefaults
		case LRoleVars:
			private = roleVars
		}
		for _, m := range private {
			for k, v := range m {
				file, line, col, known := yaml.ChildOrigin(m, k)
				put(k, v, valueOrigin{pos: template.Position{File: file, Line: line, Col: col}}, known)
			}
		}
	}
	return out, origins
}

// WithRoleScope is the context of a task of a role whose defaults and
// vars are private to it (include_role without public): they join the
// variables at their precedence.
func (c *Context) WithRoleScope(defaults, roleVars []map[string]any) *Context {
	if len(defaults) == 0 && len(roleVars) == 0 {
		return c
	}
	child := *c
	child.flat, child.flatOrigins = c.store.flattenOrigins(c.host, defaults, roleVars)
	child.resolving = map[string]bool{}
	child.cache = map[string]any{}
	return &child
}

// Context is the per-(host, task) variable view handed to the template
// engine. Raw values are templated lazily at Get time with cycle detection
// and memoization; task vars and the loop variable overlay the flattened
// store.
type Context struct {
	host    string
	store   *Store
	overlay map[string]any // task vars, loop var — highest below magic
	magic   map[string]any
	flat    map[string]any
	// flatOrigins and overlayOrigins are where flat's and overlay's
	// values came from, where known.
	flatOrigins    map[string]valueOrigin
	overlayOrigins map[string]valueOrigin
	resolving      map[string]bool
	cache          map[string]any
	pos            template.Position

	keepDeprecated bool
	// sourced: templates in the value being resolved report their own
	// origin (where the variable was defined) rather than pos.
	sourced bool
	// inContainer: the value being templated is an item of a container.
	inContainer bool
	// markers: a template in a variable's value that uses an undefined
	// value yields it (a marker) rather than failing (debug's var=).
	markers bool
	// conn are the connection variables a task execution adds under the
	// names the task's variables do not define (PlayContext.update_vars,
	// ConnectionBase.update_vars): visible by name, but not among the
	// variables "vars" and hostvars list.
	conn map[string]any
	// nameOrder is the order Names lists variables in (nil: by name).
	nameOrder []string
	noConn    bool
}

// NewContext builds a variable context for one host and task.
func (s *Store) NewContext(host string, pos template.Position) *Context {
	c := &Context{
		host:      host,
		store:     s,
		overlay:   map[string]any{},
		magic:     map[string]any{},
		resolving: map[string]bool{},
		cache:     map[string]any{},
		pos:       pos,
	}
	c.flat, c.flatOrigins = s.flattenOrigins(host, nil, nil)
	c.magic["inventory_hostname"] = host
	c.magic["inventory_hostname_short"] = shortHostname(host)
	c.magic["omit"] = template.Omit{}
	return c
}

// WithOverlay returns a child context adding vars (task vars, loop item).
// The resolution cache is not shared: an overlay can change what names mean.
func (c *Context) WithOverlay(vars map[string]any) *Context {
	child := *c
	child.overlay = make(map[string]any, len(c.overlay)+len(vars))
	child.overlayOrigins = make(map[string]valueOrigin, len(c.overlayOrigins))
	for k, v := range c.overlay {
		child.overlay[k] = v
	}
	for k, o := range c.overlayOrigins {
		child.overlayOrigins[k] = o
	}
	for k, v := range vars {
		child.overlay[k] = v
		if file, line, col, ok := yaml.ChildOrigin(vars, k); ok {
			child.overlayOrigins[k] = valueOrigin{pos: template.Position{File: file, Line: line, Col: col}}
		} else {
			delete(child.overlayOrigins, k)
		}
	}
	child.resolving = map[string]bool{}
	child.cache = map[string]any{}
	return &child
}

// WithConnectionVars returns a child context where the connection
// variables vars stand in for the names nothing else defines.
func (c *Context) WithConnectionVars(vars map[string]any) *Context {
	if len(vars) == 0 {
		return c
	}
	child := *c
	child.conn = vars
	return &child
}

// Has reports whether name is defined in the context, without templating
// its value (connection variables aside).
func (c *Context) Has(name string) bool {
	if name == "vars" {
		return true
	}
	if _, ok := c.magic[name]; ok {
		return true
	}
	if _, ok := c.store.extraVar(name); ok {
		return true
	}
	if _, ok := c.overlay[name]; ok {
		return true
	}
	_, ok := c.flat[name]
	return ok
}

// SetMagic installs a magic variable (groups, play_hosts, ansible_facts...).
func (c *Context) SetMagic(name string, v any) { c.magic[name] = v }

// Names returns the variable names visible in this context (flattened store
// vars, overlay, and magic vars) — used to expose a host's variable set
// through hostvars.
func (c *Context) Names() []string {
	seen := map[string]bool{}
	var out []string
	add := func(m map[string]any) {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	add(c.flat)
	add(c.overlay)
	add(c.magic)
	sort.Strings(out)
	if c.nameOrder == nil {
		return out
	}
	// The order the variables were combined in, the others after it.
	ordered := make([]string, 0, len(out))
	placed := map[string]bool{}
	for _, k := range c.nameOrder {
		if seen[k] && !placed[k] {
			placed[k] = true
			ordered = append(ordered, k)
		}
	}
	for _, k := range out {
		if !placed[k] {
			ordered = append(ordered, k)
		}
	}
	return ordered
}

// hostVarsSkip are the layers a host's variables through hostvars leave
// out: the play's (get_vars without a play or task).
var hostVarsSkip = map[Layer]bool{LPlayVars: true, LPlayVarsFiles: true, LRoleDefaults: true, LRoleVars: true, LTaskVars: true}

// NewHostVarsContext is a host's variables as hostvars shows them: no
// play's, nor the omit placeholder.
func (s *Store) NewHostVarsContext(host string, pos template.Position) *Context {
	c := s.NewContext(host, pos)
	c.flat, c.flatOrigins = s.flattenLayers(host, nil, nil, hostVarsSkip)
	delete(c.magic, "omit")
	return c
}

// HostLayerOrder is the order of a host's variable names as get_vars
// combines them without a play: its inventory variables, then (after
// the magic names a host carries, which the caller places) its facts,
// include_vars, set_fact and registered values, and the extra vars.
func (s *Store) HostLayerOrder(host string) (inventory, rest []string) {
	s.loadFacts(host)
	s.mu.RLock()
	defer s.mu.RUnlock()
	inventory = append(inventory, s.order[LHostVars][host]...)
	for _, l := range []Layer{LFacts, LIncludeVars, LHostFacts} {
		rest = append(rest, s.order[l][host]...)
	}
	rest = append(rest, s.order[LExtraVars][""]...)
	return inventory, rest
}

// SetNameOrder sets the order Names lists the variables in.
func (c *Context) SetNameOrder(order []string) { c.nameOrder = order }

// AsMapping exposes a context as a template.Mapping (GetItem/Keys/Len), so
// one host's resolved variables can be read from another host — the basis
// of the hostvars magic variable.
func (c *Context) AsMapping() template.Mapping { return &contextMapping{ctx: c} }

type contextMapping struct{ ctx *Context }

func (m *contextMapping) GetItem(key string) (any, bool) { return m.ctx.getSafe(key) }
func (m *contextMapping) Keys() []string                 { return m.ctx.Names() }
func (m *contextMapping) Len() int                       { return len(m.ctx.Names()) }

// VarOrigin is where the context's variable came from.
func (m *contextMapping) VarOrigin(name string) (template.OriginRef, bool) {
	return m.ctx.VarOrigin(name)
}

// varsMapping is the "vars" magic variable: the context's variables,
// without itself.
type varsMapping struct{ ctx *Context }

func (m *varsMapping) GetItem(key string) (any, bool) {
	if key == "vars" {
		return nil, false
	}
	c := *m.ctx
	c.noConn = true
	return c.getSafe(key)
}

func (m *varsMapping) Keys() []string {
	names := m.ctx.Names()
	out := names[:0:0]
	for _, n := range names {
		// get_vars has no omit placeholder, nor what the task executor
		// adds later (ansible_search_path) or understudy keeps inside.
		if n != "vars" && n != "omit" && n != "ansible_search_path" && !strings.HasPrefix(n, "__understudy") {
			out = append(out, n)
		}
	}
	return out
}

func (m *varsMapping) Len() int { return len(m.Keys()) }

// getSafe is GetTagged with resolution panics converted to a not-found so
// a bad var on another host doesn't abort the referencing host.
func (c *Context) getSafe(name string) (v any, ok bool) {
	defer func() {
		if recover() != nil {
			v, ok = nil, false
		}
	}()
	return c.GetTagged(name)
}

// At returns a view of the context that templates at pos (the origin a
// module argument's templates report).
func (c *Context) At(pos template.Position) *Context {
	if pos.File == "" {
		return c
	}
	child := *c
	child.pos = pos
	return &child
}

// KeepingDeprecated returns a view whose templating results keep their
// deprecated values tagged (set_fact copies stay deprecated).
func (c *Context) KeepingDeprecated() *Context {
	child := *c
	child.keepDeprecated = true
	return &child
}

// KeepDeprecated implements template.KeepsDeprecated.
func (c *Context) KeepDeprecated() bool { return c.keepDeprecated }

// Get implements template.VarGetter with lazy recursive resolution. A
// deprecated variable comes back plain (the engine reads GetTagged).
func (c *Context) Get(name string) (any, bool) {
	v, ok := c.GetTagged(name)
	return template.Undeprecate(v), ok
}

// GetTagged is Get keeping a deprecated variable's template.Deprecated
// wrapper, so templates that read it warn.
func (c *Context) GetTagged(name string) (any, bool) {
	if name == "vars" {
		// The deprecated "vars" magic variable: every variable in scope.
		return template.Deprecated{Value: &varsMapping{ctx: c}, Msg: `The internal "vars" dictionary is deprecated.`,
			Help: "Use the `vars` and `varnames` lookups instead.", Version: "2.24"}, true
	}
	if v, ok := c.magic[name]; ok {
		return v, true
	}
	if v, ok := c.cache[name]; ok {
		return v, true
	}
	// -e extra vars outrank everything, including task/block/loop vars
	// in the overlay (Ansible's precedence rule 22 of 22).
	raw, ok := c.store.extraVar(name)
	if !ok {
		raw, ok = c.overlay[name]
	}
	if !ok {
		raw, ok = c.flat[name]
	}
	if !ok {
		if v, isConn := c.conn[name]; isConn && !c.noConn {
			return v, true
		}
		return nil, false
	}
	if c.resolving[name] {
		// Matching Ansible's error text for recursive templates.
		panic(&CycleError{Name: name})
	}
	c.resolving[name] = true
	defer delete(c.resolving, name)
	// A variable's templates report where the variable was defined, as
	// ansible-core's origin-tagged values do (a deprecated value read,
	// an undefined variable).
	sourced := *c
	sourced.sourced = true
	v, err := sourced.deepTemplate(raw)
	if err != nil {
		if ve, ok := template.AsVaultError(err); ok && ve.Pos.File == "" {
			// An encrypted value fails where it was defined.
			if ref, ok := c.VarOrigin(name); ok {
				ve.Pos = ref.Pos
			}
		}
		panic(err) // recovered by Context.Template*/executor boundary
	}
	c.cache[name] = v
	return v, true
}

// RawVar implements template.RawVarGetter: a variable's value as
// defined, before templating (a !vault value still encrypted).
func (c *Context) RawVar(name string) (any, bool) {
	if v, ok := c.magic[name]; ok {
		return v, true
	}
	raw, ok := c.store.extraVar(name)
	if !ok {
		raw, ok = c.overlay[name]
	}
	if !ok {
		raw, ok = c.flat[name]
	}
	return raw, ok
}

// CycleError reports a self-referencing variable.
type CycleError struct{ Name string }

func (e *CycleError) Error() string {
	return fmt.Sprintf("recursive loop detected in template: variable %q references itself", e.Name)
}

// origin is where a template in the value being templated reports from:
// its own origin while resolving a variable, else the context's position.
func (c *Context) origin(s string) template.Position {
	pos := c.pos
	if c.sourced {
		if file, line, col, ok := yaml.Origin(s); ok {
			pos = template.Position{File: file, Line: line, Col: col}
		}
	}
	pos.InContainer = c.inContainer
	if c.inContainer && c.pos.File != "" {
		pos.ContainerFile, pos.ContainerLine, pos.ContainerCol = c.pos.File, c.pos.Line, c.pos.Col
	}
	return pos
}

// Sourced returns a view whose templates report their own origins (a
// container's items where each was written) where known, else its
// position.
func (c *Context) Sourced() *Context {
	child := *c
	child.sourced = true
	return &child
}

// items is the view a container's items are templated in.
func (c *Context) items() *Context {
	if c.inContainer {
		return c
	}
	child := *c
	child.inContainer = true
	return &child
}

// deepTemplate walks a raw value, rendering every string through the engine.
// A container met again inside itself (a recursive structure, as a YAML
// alias inside its own anchor builds) is the one being rendered, so the
// result recurses the same way; one met again elsewhere is rendered anew,
// as ansible-core's templating copies it.
func (c *Context) deepTemplate(v any) (any, error) {
	return c.deepTemplateIn(v, map[containerID]any{})
}

// containerID identifies a list (its backing array and length) or a map.
type containerID struct {
	ptr uintptr
	n   int
}

func idOf(v any) (containerID, bool) {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return containerID{}, false
		}
		return containerID{uintptr(unsafe.Pointer(&t[0])), len(t)}, true
	case map[string]any:
		return containerID{reflect.ValueOf(t).Pointer(), -1}, true
	case *yaml.OMap:
		return containerID{uintptr(unsafe.Pointer(t)), -2}, true
	}
	return containerID{}, false
}

func (c *Context) deepTemplateIn(v any, seen map[containerID]any) (any, error) {
	id, isContainer := idOf(v)
	if isContainer {
		if out, ok := seen[id]; ok {
			return out, nil
		}
	}
	switch t := v.(type) {
	case string:
		render := c.store.engine.RenderTemplate
		if c.markers {
			// An undefined value in the text stays in place, as its
			// marker.
			render = c.store.engine.RenderTemplateMarking
		}
		out, err := render(t, c, c.origin(t))
		var ue *template.UndefinedError
		if err != nil && c.markers && errors.As(err, &ue) {
			return template.Undefined{Name: ue.Name, Err: ue}, nil
		}
		return out, err
	case yaml.UnsafeString:
		return t, nil // never re-templated
	case Final:
		return t.V, nil
	case yaml.VaultedString:
		if c.store.VaultDecrypt == nil {
			return nil, &template.VaultError{Reason: template.VaultNoSecrets}
		}
		plain, err := c.store.VaultDecrypt(t)
		if err != nil {
			return nil, &template.VaultError{Reason: template.VaultCannotOpen}
		}
		// The decrypted value may itself contain templates.
		return c.store.engine.RenderTemplate(plain, c, c.pos)
	case []any:
		// Its own backing array even when empty: the list has an identity
		// to_yaml aliases by, as PyYAML does by id().
		out := make([]any, len(t), max(len(t), 1))
		if isContainer {
			seen[id] = out
			defer delete(seen, id)
		}
		for i, item := range t {
			r, err := c.items().deepTemplateIn(item, seen)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		seen[id] = out
		defer delete(seen, id)
		for k, val := range t {
			r, err := c.items().deepTemplateIn(val, seen)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case *yaml.OMap:
		// Recurse into ordered maps (a dict-valued var/arg) so nested template
		// strings are rendered, and keep the key order the result should carry.
		out := yaml.NewOMap()
		seen[id] = out
		defer delete(seen, id)
		for _, k := range t.Keys() {
			r, err := c.items().deepTemplateIn(t.Get(k), seen)
			if err != nil {
				return nil, err
			}
			out.Set(k, r)
		}
		return out, nil
	default:
		return v, nil
	}
}

// TemplateValue templates any raw value (module args, loop lists) in this
// context. Var-resolution panics surface as errors here.
func (c *Context) TemplateValue(v any) (out any, err error) {
	defer capturePanic(&err)
	return c.deepTemplate(v)
}

// TemplateString renders one string value.
func (c *Context) TemplateString(s string) (out any, err error) {
	defer capturePanic(&err)
	return c.store.engine.RenderTemplate(s, c, c.pos)
}

// RenderFile renders template-file content: output is always text and the
// given position (the template file itself) is used for errors.
func (c *Context) RenderFile(src string, pos template.Position, searchPath ...string) (out string, err error) {
	defer capturePanic(&err)
	if len(searchPath) == 0 {
		searchPath = []string{filepath.Dir(pos.File)}
	}
	return c.store.engine.RenderFile(src, c, pos, searchPath)
}

// RenderFileWith is RenderFile with Jinja environment overrides (the
// template module's delimiters, trim_blocks, lstrip_blocks and
// newline_sequence).
func (c *Context) RenderFileWith(src string, pos template.Position, opts template.Options, searchPath ...string) (out string, err error) {
	defer capturePanic(&err)
	if len(searchPath) == 0 {
		searchPath = []string{filepath.Dir(pos.File)}
	}
	return c.store.engine.WithOptions(opts).RenderFile(src, c, pos, searchPath)
}

// EvalWhen evaluates a when: clause list (implicit AND): each must have
// a boolean result (see template.EvalConditional).
func (c *Context) EvalWhen(exprs []string) (ok bool, err error) {
	defer capturePanic(&err)
	for _, e := range exprs {
		b, err := c.store.engine.EvalConditional(e, c, c.pos)
		if err != nil {
			return false, err
		}
		if !b {
			return false, nil
		}
	}
	return true, nil
}

// VarOrigin implements template.OriginSource: the raw value of a
// variable, with where it came from.
func (c *Context) VarOrigin(name string) (template.OriginRef, bool) {
	if _, ok := c.magic[name]; ok {
		return template.OriginRef{}, false
	}
	var raw any
	var o valueOrigin
	var found, known bool
	if v, ok := c.store.extraVar(name); ok {
		raw, found = v, true
		c.store.mu.RLock()
		o, known = c.store.origins[LExtraVars][""][name]
		c.store.mu.RUnlock()
	} else if v, ok := c.overlay[name]; ok {
		raw, found = v, true
		o, known = c.overlayOrigins[name]
	} else if v, ok := c.flat[name]; ok {
		raw, found = v, true
		o, known = c.flatOrigins[name]
	}
	if !found {
		return template.OriginRef{}, false
	}
	raw = template.Undeprecate(raw)
	if f, isFinal := raw.(Final); isFinal {
		if f.Origin == nil {
			return template.OriginRef{}, false
		}
		return *f.Origin, true
	}
	ref := template.OriginRef{Raw: raw, HasRaw: true, Inherit: o.inherit}
	if file, line, col, ok := yaml.ValueOrigin(raw); ok {
		ref.Pos = template.Position{File: file, Line: line, Col: col}
	} else if known {
		ref.Pos = o.pos
	}
	return ref, true
}

// ValueOrigin is where the raw value raw, written at pos, comes from once
// templated here: what set_fact records for a fact.
func (c *Context) ValueOrigin(raw any, pos template.Position) template.OriginRef {
	ref := template.OriginRef{Raw: raw, HasRaw: true, Pos: pos}
	if file, line, col, ok := yaml.ValueOrigin(raw); ok {
		ref.Pos = template.Position{File: file, Line: line, Col: col}
	}
	return c.store.engine.ResolveOrigin(ref, c)
}

// EvalExprReplacing evaluates a bare expression as debug's var= does:
// undefined values become placeholders, reported as markers.
func (c *Context) EvalExprReplacing(expr string) (v any, markers []template.Marker, err error) {
	defer capturePanic(&err)
	// Variables resolve anew, an undefined item of one a marker in place.
	child := *c
	child.markers = true
	child.resolving = map[string]bool{}
	child.cache = map[string]any{}
	return c.store.engine.EvalExpressionReplacing(expr, &child, c.pos)
}

// EvalExpr evaluates a bare expression (until:, failed_when:).
func (c *Context) EvalExpr(expr string) (v any, err error) {
	defer capturePanic(&err)
	return c.store.engine.EvalExpression(expr, c, c.pos)
}

func capturePanic(err *error) {
	if r := recover(); r != nil {
		if e, ok := r.(error); ok {
			*err = e
			return
		}
		panic(r)
	}
}

func shortHostname(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			return host[:i]
		}
	}
	return host
}

// extraVar returns an -e extra var, if set.
func (s *Store) extraVar(name string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.layers[LExtraVars] == nil {
		return nil, false
	}
	v, ok := s.layers[LExtraVars][""][name]
	return v, ok
}

// isAllTemplate is is_possibly_all_template: the string starts and ends
// with Jinja delimiters.
func isAllTemplate(s string) bool {
	return strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") ||
		strings.HasPrefix(s, "{%") && strings.HasSuffix(s, "%}")
}

// ItemOrigin is where item i of the list raw (written at pos) resolves
// to came from: a loop's item, read through its loop variable.
func (c *Context) ItemOrigin(raw any, pos template.Position, i int) (template.OriginRef, bool) {
	return c.store.engine.ItemOrigin(c.ValueOrigin(raw, pos), i, c)
}
