// Package vars implements Ansible's layered variable precedence with lazy,
// use-time template resolution. This is the M3 subset — inventory layers,
// magic vars, and hostvars land with the full store in M4/M5; the Layer
// ordering already reserves their slots.
package vars

import (
	"fmt"
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

// SetPlayVars replaces the play-vars layer (called at play start).
func (s *Store) SetPlayVars(vars map[string]any) {
	s.mu.Lock()
	s.layers[LPlayVars] = nil
	s.mu.Unlock()
	if vars != nil {
		s.set(LPlayVars, "", vars)
	}
}

// AddVarsFile merges one vars_files result.
func (s *Store) AddVarsFile(vars map[string]any) { s.set(LPlayVarsFiles, "", vars) }

// SetExtraVars installs -e vars (highest precedence).
func (s *Store) SetExtraVars(vars map[string]any) { s.set(LExtraVars, "", vars) }

// SetHostFact records set_fact/register results for one host.
func (s *Store) SetHostFact(host, name string, value any) {
	s.set(LHostFacts, host, map[string]any{name: value})
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
	return v, ok
}

// SetFacts records gathered facts for one host.
func (s *Store) SetFacts(host string, facts map[string]any) {
	s.set(LFacts, host, facts)
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

// Get implements template.VarGetter with lazy recursive resolution.
func (c *Context) Get(name string) (any, bool) {
	if v, ok := c.magic[name]; ok {
		return v, true
	}
	if v, ok := c.cache[name]; ok {
		return v, true
	}
	raw, ok := c.overlay[name]
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
