// Package vars implements Ansible's layered variable precedence with lazy,
// use-time template resolution. This is the M3 subset — inventory layers,
// magic vars, and hostvars land with the full store in M4/M5; the Layer
// ordering already reserves their slots.
package vars

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"

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
	LHostFacts // set_fact + register; persists across plays
	LExtraVars // -e always wins
	layerCount
)

// Store holds raw (untemplated) variables per layer. Host-scoped layers key
// by hostname; global layers use the "" key.
type Store struct {
	mu     sync.RWMutex
	layers [layerCount]map[string]map[string]any // layer -> scope key -> vars
	engine *template.Engine

	// VaultDecrypt decrypts a !vault-tagged value at use time. Nil means no
	// vault password is configured; encountering an encrypted value errors.
	VaultDecrypt func(yaml.VaultedString) (string, error)
}

func NewStore(engine *template.Engine) *Store {
	return &Store{engine: engine}
}

func (s *Store) set(layer Layer, scope string, vars map[string]any) {
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
	for k, v := range vars {
		dst[k] = v
	}
}

// SetPlayVars replaces all play-scoped layers (called at play start).
func (s *Store) SetPlayVars(vars map[string]any) {
	s.mu.Lock()
	s.layers[LPlayVars] = nil
	s.layers[LPlayVarsFiles] = nil
	s.layers[LRoleDefaults] = nil
	s.layers[LRoleVars] = nil
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
type Final struct{ V any }

// SetHostFact records set_fact/register results for one host.
func (s *Store) SetHostFact(host, name string, value any) {
	s.set(LHostFacts, host, map[string]any{name: Final{value}})
}

// SetInventoryVars installs a host's merged inventory vars (group vars in
// depth order + host vars — the inventory package pre-merges them).
func (s *Store) SetInventoryVars(host string, vars map[string]any) {
	s.set(LHostVars, host, vars)
}

// RawHostVar fetches an untemplated var for one host (behavioral
// connection vars like ansible_connection/ansible_host).
func (s *Store) RawHostVar(host, name string) (any, bool) {
	flat := s.flatten(host)
	v, ok := flat[name]
	if f, isFinal := v.(Final); isFinal {
		v = f.V
	}
	return v, ok
}

// SetFacts records gathered facts for one host.
func (s *Store) SetFacts(host string, facts map[string]any) {
	final := make(map[string]any, len(facts))
	for k, v := range facts {
		final[k] = Final{v}
	}
	s.set(LFacts, host, final)
}

// ClearFacts drops a host's gathered facts (meta: clear_facts); set_fact
// values are not cached facts and survive, as in Ansible.
func (s *Store) ClearFacts(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layers[LFacts] != nil {
		delete(s.layers[LFacts], host)
	}
}

// flatten merges all layers for one host in precedence order.
func (s *Store) flatten(host string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{}
	for layer := Layer(0); layer < layerCount; layer++ {
		scopes := s.layers[layer]
		if scopes == nil {
			continue
		}
		for k, v := range scopes[""] {
			out[k] = v
		}
		if host != "" {
			for k, v := range scopes[host] {
				out[k] = v
			}
		}
	}
	return out
}

// Context is the per-(host, task) variable view handed to the template
// engine. Raw values are templated lazily at Get time with cycle detection
// and memoization; task vars and the loop variable overlay the flattened
// store.
type Context struct {
	host      string
	store     *Store
	overlay   map[string]any // task vars, loop var — highest below magic
	magic     map[string]any
	flat      map[string]any
	resolving map[string]bool
	cache     map[string]any
	pos       template.Position
}

// NewContext builds a variable context for one host and task.
func (s *Store) NewContext(host string, pos template.Position) *Context {
	c := &Context{
		host:      host,
		store:     s,
		overlay:   map[string]any{},
		magic:     map[string]any{},
		flat:      s.flatten(host),
		resolving: map[string]bool{},
		cache:     map[string]any{},
		pos:       pos,
	}
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
	for k, v := range c.overlay {
		child.overlay[k] = v
	}
	for k, v := range vars {
		child.overlay[k] = v
	}
	child.resolving = map[string]bool{}
	child.cache = map[string]any{}
	return &child
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
	return out
}

// AsMapping exposes a context as a template.Mapping (GetItem/Keys/Len), so
// one host's resolved variables can be read from another host — the basis
// of the hostvars magic variable.
func (c *Context) AsMapping() template.Mapping { return &contextMapping{ctx: c} }

type contextMapping struct{ ctx *Context }

func (m *contextMapping) GetItem(key string) (any, bool) { return m.ctx.getSafe(key) }
func (m *contextMapping) Keys() []string                 { return m.ctx.Names() }
func (m *contextMapping) Len() int                       { return len(m.ctx.Names()) }

// getSafe is Get with resolution panics converted to a not-found so a bad
// var on another host doesn't abort the referencing host.
func (c *Context) getSafe(name string) (v any, ok bool) {
	defer func() {
		if recover() != nil {
			v, ok = nil, false
		}
	}()
	return c.Get(name)
}

// Get implements template.VarGetter with lazy recursive resolution.
func (c *Context) Get(name string) (any, bool) {
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
		return nil, false
	}
	if c.resolving[name] {
		// Matching Ansible's error text for recursive templates.
		panic(&CycleError{Name: name})
	}
	c.resolving[name] = true
	defer delete(c.resolving, name)
	v, err := c.deepTemplate(raw)
	if err != nil {
		panic(err) // recovered by Context.Template*/executor boundary
	}
	c.cache[name] = v
	return v, true
}

// CycleError reports a self-referencing variable.
type CycleError struct{ Name string }

func (e *CycleError) Error() string {
	return fmt.Sprintf("recursive loop detected in template: variable %q references itself", e.Name)
}

// deepTemplate walks a raw value, rendering every string through the engine.
func (c *Context) deepTemplate(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return c.store.engine.RenderTemplate(t, c, c.pos)
	case yaml.UnsafeString:
		return t, nil // never re-templated
	case Final:
		return t.V, nil
	case yaml.VaultedString:
		if c.store.VaultDecrypt == nil {
			return nil, fmt.Errorf("an encrypted value was found but no vault password was provided (use --vault-password-file or --ask-vault-pass)")
		}
		plain, err := c.store.VaultDecrypt(t)
		if err != nil {
			return nil, err
		}
		// The decrypted value may itself contain templates.
		return c.store.engine.RenderTemplate(plain, c, c.pos)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			r, err := c.deepTemplate(item)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			r, err := c.deepTemplate(val)
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
		for _, k := range t.Keys() {
			r, err := c.deepTemplate(t.Get(k))
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

// EvalWhen evaluates a when: clause list (implicit AND).
func (c *Context) EvalWhen(exprs []string) (ok bool, err error) {
	defer capturePanic(&err)
	for _, e := range exprs {
		if e == "" {
			continue
		}
		b, err := c.store.engine.EvalBool(e, c, c.pos)
		if err != nil {
			return false, err
		}
		if !b {
			return false, nil
		}
	}
	return true, nil
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
