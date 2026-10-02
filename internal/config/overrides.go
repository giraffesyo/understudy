package config

import (
	"os"
	"sync"
)

var (
	overridesMu sync.RWMutex
	overrides   map[string]string
)

// SetOverrides makes settings read as environment variables
// (ANSIBLE_GATHERING=smart, ...) over the process environment until the
// returned restore runs: the Go API's per-run settings.
func SetOverrides(env map[string]string) (restore func()) {
	overridesMu.Lock()
	prev := overrides
	overrides = env
	overridesMu.Unlock()
	return func() {
		overridesMu.Lock()
		overrides = prev
		overridesMu.Unlock()
	}
}

func lookupEnv(key string) (string, bool) {
	overridesMu.RLock()
	v, ok := overrides[key]
	overridesMu.RUnlock()
	if ok {
		return v, true
	}
	return os.LookupEnv(key)
}

func getenv(key string) string {
	v, _ := lookupEnv(key)
	return v
}
