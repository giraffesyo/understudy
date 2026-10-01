// Package factcache implements ansible-core's fact cache: the builtin
// cache plugins (memory, jsonfile) behind the cache loader's interposer,
// which stores each host under a schema-qualified key ("s1_<host>") as a
// JSON payload preserving the values' tags. jsonfile files are those
// ansible-core writes, byte for byte, so both can share a cache
// directory.
package factcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/giraffesyo/understudy/internal/omap"
	"github.com/giraffesyo/understudy/internal/template"
)

// Settings are the cache configuration: CACHE_PLUGIN and the plugin's
// _uri (already resolved; "" when not set), _prefix and _timeout.
type Settings struct {
	Plugin  string
	URI     string
	Prefix  string
	Timeout int
}

// Facts is one host's cached facts, in insertion order (a Python dict):
// each key's plain value (what variables see) and its serialized form
// with the tags ansible-core keeps (what the payload holds).
type Facts struct {
	keys   []string
	plain  map[string]any
	tagged map[string]any
}

// NewFacts is an empty set of facts.
func NewFacts() *Facts {
	return &Facts{plain: map[string]any{}, tagged: map[string]any{}}
}

// Keys are the fact names in order.
func (f *Facts) Keys() []string { return f.keys }

// Get is a fact's plain value.
func (f *Facts) Get(k string) (any, bool) {
	v, ok := f.plain[k]
	return v, ok
}

// Map is the facts as a plain map.
func (f *Facts) Map() map[string]any {
	out := make(map[string]any, len(f.keys))
	for _, k := range f.keys {
		out[k] = f.plain[k]
	}
	return out
}

// Len is the number of facts.
func (f *Facts) Len() int { return len(f.keys) }

// Set sets one fact (host_cache |= facts: a key already there keeps its
// place): its plain value and its tagged form (see Tag).
func (f *Facts) Set(k string, plain, tagged any) {
	if _, had := f.plain[k]; !had {
		f.keys = append(f.keys, k)
	}
	f.plain[k] = plain
	f.tagged[k] = tagged
}

// Update merges other into f (dict |=).
func (f *Facts) Update(other *Facts) {
	for _, k := range other.keys {
		f.Set(k, other.plain[k], other.tagged[k])
	}
}

// Clone is a shallow copy.
func (f *Facts) Clone() *Facts {
	c := NewFacts()
	c.Update(f)
	return c
}

// payload is the interposer's JSON payload of the facts.
func (f *Facts) payload() string {
	m := omap.NewOMap()
	for _, k := range f.keys {
		m.Set(k, f.tagged[k])
	}
	return template.PyJSON(m, 0, false, true)
}

// Cache is a cache plugin as the cache loader returns it.
type Cache interface {
	// Get is a host's facts; nil (and no error) when there are none.
	Get(host string) (*Facts, error)
	Set(host string, f *Facts) error
	Contains(host string) bool
	Delete(host string)
}

// Error is an AnsibleError a cache plugin raises.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Warn receives the warnings the plugins show (without "[WARNING]: ").
type Warn func(msg string)

// Open is cache_loader.get(CACHE_PLUGIN): a new plugin instance, failing
// as ansible-core's loader and the plugin's constructor do.
func Open(s Settings, warn Warn) (Cache, error) {
	switch s.Plugin {
	case "memory", "ansible.builtin.memory", "ansible.legacy.memory":
		return &memory{m: map[string]*Facts{}}, nil
	case "jsonfile", "ansible.builtin.jsonfile", "ansible.legacy.jsonfile":
		return openJSONFile(s, warn)
	}
	return nil, &Error{fmt.Sprintf("Unable to load the cache plugin %s.", pyQuote(s.Plugin))}
}

// Memory is the builtin memory plugin (the fallback of a plugin that
// cannot load).
func Memory() Cache { return &memory{m: map[string]*Facts{}} }

type memory struct {
	mu sync.Mutex
	m  map[string]*Facts
}

func (c *memory) Get(host string) (*Facts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[host], nil
}

func (c *memory) Set(host string, f *Facts) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[host] = f
	return nil
}

func (c *memory) Contains(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[host]
	return ok
}

func (c *memory) Delete(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, host)
}

// schemaPrefix is the interposer's key prefix for the cache persistence
// profile's schema.
const schemaPrefix = "s1_"

// jsonFile is the jsonfile plugin (BaseFileCacheModule) behind the
// interposer.
type jsonFile struct {
	mu      sync.Mutex
	dir     string
	prefix  string
	timeout float64
	cache   map[string]*Facts // the plugin's in-memory _cache
	warn    Warn
}

func openJSONFile(s Settings, warn Warn) (Cache, error) {
	if s.URI == "" {
		return nil, &Error{"Required config '_uri' for 'jsonfile' cache plugin not provided."}
	}
	c := &jsonFile{dir: s.URI, prefix: s.Prefix, timeout: float64(s.Timeout), cache: map[string]*Facts{}, warn: warn}
	if _, err := os.Stat(c.dir); err != nil {
		if err := os.MkdirAll(c.dir, 0o777); err != nil {
			return nil, &Error{fmt.Sprintf("Error in 'jsonfile' cache plugin while trying to create cache dir %s.", pyQuote(c.dir))}
		}
	} else if !accessible(c.dir) {
		return nil, &Error{fmt.Sprintf("''jsonfile'' cache, configured path (%s) does not have necessary permissions (rwx), disabling plugin", c.dir)}
	}
	return c, nil
}

// file is _get_cache_file_name for a wrapped key: a key with path
// characters becomes the start of its SHA-256.
func (c *jsonFile) file(host string) string {
	key := schemaPrefix + host
	for _, bad := range []string{"/", "..", "<", ">", "|"} {
		if strings.Contains(key, bad) {
			sum := sha256.Sum256([]byte(key))
			h := hex.EncodeToString(sum[:])
			key = h[:min(len(h), max(len(key), 12))]
			break
		}
	}
	return filepath.Join(c.dir, c.prefix+key)
}

// expired is has_expired.
func (c *jsonFile) expired(host string) bool {
	if c.timeout == 0 {
		return false
	}
	st, err := os.Stat(c.file(host))
	if err != nil {
		return false
	}
	if float64(time.Since(st.ModTime()))/float64(time.Second) <= c.timeout {
		return false
	}
	delete(c.cache, host)
	return true
}

func (c *jsonFile) Get(host string) (*Facts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.cache[host]; ok {
		return f, nil
	}
	if c.expired(host) {
		return nil, nil
	}
	path := c.file(host)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Error{fmt.Sprintf("Error while accessing the cache file %s.", pyQuote(path))}
	}
	f, err := decodeFile(data)
	if err != nil {
		if c.warn != nil {
			c.warn(fmt.Sprintf("error in 'jsonfile' cache plugin while trying to read %s : b%s. Most likely a corrupt file, so erasing and failing.",
				path, pyQuote(err.Error())))
		}
		os.Remove(path)
		return nil, &Error{fmt.Sprintf("The cache file %s was corrupt, or did not otherwise contain valid data. It has been removed, so you can re-run your command now.", path)}
	}
	if f == nil {
		return nil, nil
	}
	c.cache[host] = f
	return f, nil
}

func (c *jsonFile) Set(host string, f *Facts) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[host] = f
	path := c.file(host)
	tmp, err := os.CreateTemp(c.dir, "tmp")
	if err != nil {
		return nil
	}
	defer os.Remove(tmp.Name())
	data := `{"__payload__": ` + template.PyJSON(f.payload(), 0, false, true) + `}`
	if _, err := tmp.WriteString(data); err != nil && c.warn != nil {
		c.warn(fmt.Sprintf("Error in 'jsonfile' cache plugin while trying to write to %s.", pyQuote(tmp.Name())))
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err == nil {
		_ = os.Chmod(path, 0o644)
	} else if c.warn != nil {
		c.warn(fmt.Sprintf("Error in 'jsonfile' cache plugin while trying to move %s to %s.", pyQuote(tmp.Name()), pyQuote(path)))
	}
	return nil
}

func (c *jsonFile) Contains(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cache[host]; ok {
		return true
	}
	if c.expired(host) {
		return false
	}
	_, err := os.Stat(c.file(host))
	return err == nil
}

func (c *jsonFile) Delete(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, host)
	os.Remove(c.file(host))
}

// decodeFile is the plugin's json.loads and the interposer's decoding
// of the payload. A file whose outer document is not JSON fails
// (ValueError); one without a payload holds no facts (KeyError).
func decodeFile(data []byte) (*Facts, error) {
	outer, err := omap.UnmarshalJSON(data)
	if err != nil {
		return nil, err
	}
	m, ok := outer.(*omap.OMap)
	if !ok {
		return nil, nil
	}
	payload, ok := m.Get("__payload__").(string)
	if !ok {
		return nil, nil
	}
	inner, err := omap.UnmarshalJSON([]byte(payload))
	if err != nil {
		return nil, err
	}
	im, ok := inner.(*omap.OMap)
	if !ok {
		return nil, fmt.Errorf("the payload is not a mapping")
	}
	f := NewFacts()
	for _, k := range im.Keys() {
		v := im.Get(k)
		plain, err := Untag(v)
		if err != nil {
			return nil, err
		}
		f.Set(k, plain, v)
	}
	return f, nil
}

// accessible is os.access(dir, R_OK), W_OK and X_OK.
func accessible(dir string) bool {
	return syscall.Access(dir, 0o7) == nil
}

// pyQuote is repr() of a str.
func pyQuote(s string) string { return template.PyRepr(s) }
