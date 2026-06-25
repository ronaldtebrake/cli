package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6"
)

// BackendTypeGit is the built-in git checkpoint backend, registered in
// production and used as the default primary when no backend is configured.
const BackendTypeGit = "git"

// OpenEnv carries the construction context a backend factory may need. The git
// backend requires Repo and uses Refs/BlobFetcher; non-git backends ignore the
// git-shaped fields and read their own configuration from the cfg block.
type OpenEnv struct {
	Repo        *git.Repository
	BlobFetcher BlobFetchFunc
	Refs        PersistentRefs
}

// Factory constructs a persistent store for one backend type. cfg is the
// backend-specific JSON "config" block from settings (nil when absent).
type Factory func(ctx context.Context, env OpenEnv, cfg json.RawMessage) (PersistentStore, error)

var (
	registryMu sync.RWMutex
	// registry maps backend type to factory. The git backend is built in;
	// test-only backends add themselves through their RegisterForTesting helpers
	// so a production binary can never select them.
	registry = map[string]Factory{BackendTypeGit: gitBackendFactory}
)

// Register adds a backend factory under typ. It panics on a duplicate type to
// surface wiring mistakes at init time. Production code registers only the git
// backend; test-only backends register through their own RegisterForTesting
// helpers so they can never be selected by a production binary.
func Register(typ string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[typ]; exists {
		panic(fmt.Sprintf("checkpoint: backend type %q already registered", typ))
	}
	registry[typ] = f
}

// build constructs the store for the named backend type, returning a clear
// error (naming the registered types) when the type is unknown.
func build(ctx context.Context, env OpenEnv, typ string, cfg json.RawMessage) (PersistentStore, error) {
	registryMu.RLock()
	f, ok := registry[typ]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown checkpoint backend type %q (registered: %s)", typ, registeredTypes())
	}
	store, err := f(ctx, env, cfg)
	if err != nil {
		return nil, fmt.Errorf("construct %q checkpoint backend: %w", typ, err)
	}
	return store, nil
}

func registeredTypes() string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	types := make([]string, 0, len(registry))
	for t := range registry {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// gitBackendFactory builds the git-backed persistent store from the open
// environment. It ignores cfg: git topology comes from env.Refs, not settings.
func gitBackendFactory(_ context.Context, env OpenEnv, _ json.RawMessage) (PersistentStore, error) {
	if env.Repo == nil {
		return nil, errors.New("git checkpoint backend requires a repository")
	}
	store := NewGitStore(env.Repo, env.Refs)
	if env.BlobFetcher != nil {
		store.SetBlobFetcher(env.BlobFetcher)
	}
	return store, nil
}
