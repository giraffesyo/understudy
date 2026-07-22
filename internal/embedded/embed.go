// Package embedded carries the cross-compiled agent binaries (gzipped)
// inside the control binary. The Makefile's `agents` target populates the
// agents/ directory before the control build.
package embedded

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sync"
)

//go:embed all:agents
var agentFS embed.FS

type AgentBinary struct {
	GzData []byte // gzipped agent executable
	Sha12  string // first 12 hex chars of the sha256 of the UNCOMPRESSED binary
}

var (
	mu    sync.Mutex
	cache = map[string]*AgentBinary{}
)

// Agent returns the embedded agent for a GOOS/GOARCH pair, or a helpful
// error when the two-stage build was skipped.
func Agent(goos, goarch string) (*AgentBinary, error) {
	key := goos + "-" + goarch
	mu.Lock()
	defer mu.Unlock()
	if a, ok := cache[key]; ok {
		return a, nil
	}
	data, err := agentFS.ReadFile("agents/agent-" + key + ".gz")
	if err != nil {
		return nil, fmt.Errorf(
			"no embedded agent for %s/%s (run `make agents` before building the control binary)", goos, goarch)
	}
	raw, err := gunzip(data)
	if err != nil {
		return nil, fmt.Errorf("embedded agent for %s is corrupt: %w", key, err)
	}
	sum := sha256.Sum256(raw)
	a := &AgentBinary{GzData: data, Sha12: hex.EncodeToString(sum[:])[:12]}
	cache[key] = a
	return a, nil
}
