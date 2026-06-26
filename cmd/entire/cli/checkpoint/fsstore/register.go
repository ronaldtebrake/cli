package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
)

// Config is the fsstore backend's settings "config" block.
type Config struct {
	Path string `json:"path"`
}

var registerOnce sync.Once

// RegisterForTesting registers the fsstore backend under its type so tests can
// select it as a checkpoint mirror (or primary in fsstore's own tests). It is
// the only path that adds fsstore to the registry: production code never calls
// it, so a production binary cannot resolve the "fs" backend. Registration is
// process-wide and idempotent (checkpoint.Register panics on duplicates).
func RegisterForTesting() {
	registerOnce.Do(func() {
		checkpoint.Register(BackendType, factory)
	})
}

//nolint:ireturn // must return the contract interface to satisfy checkpoint.Factory
func factory(_ context.Context, _ checkpoint.OpenEnv, cfg json.RawMessage) (cp.PersistentStore, error) {
	var c Config
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, fmt.Errorf("fsstore: invalid config: %w", err)
		}
	}
	if c.Path == "" {
		return nil, errors.New("fsstore: config.path is required")
	}
	return New(c.Path), nil
}
