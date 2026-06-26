package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// OpenOptions configures Open. The zero value uses the default committed-ref
// topology and attaches no blob fetcher.
type OpenOptions struct {
	// BlobFetcher is the CLI-level on-demand blob fetcher. The checkpoint
	// package cannot resolve it itself, so the CLI layer injects it here and
	// Open attaches it to the constructed store(s). nil leaves on-demand
	// fetching off.
	BlobFetcher BlobFetchFunc

	// Refs overrides the default committed-ref topology. A non-nil value wins,
	// e.g. attach pins reads to Primary via PrimaryAsRead().
	Refs *PersistentRefs
}

// Stores is the facade returned by Open: the persistent store plus the git-only
// ephemeral (shadow-branch) capability and resolved committed-ref topology.
type Stores struct {
	// Persistent is the committed store that serves permanent reads and writes.
	Persistent PersistentStore

	ephemeral EphemeralStore
	refs      PersistentRefs
}

// Open resolves the checkpoint storage topology and constructs the backing
// store(s). It keeps ref resolution, backend selection, and blob-fetcher wiring
// in one place. The primary is built through the backend registry; with no
// checkpoints config it resolves to the git backend with no mirrors, so default
// behavior is unchanged. When mirrors are configured, the persistent store is a
// fan-out wrapper (reads from primary, best-effort writes to each mirror).
//
// Backend selection is read via settings.LoadCheckpointsConfig, which resolves
// like settings.Load: from the context's worktree root if set, else relative to
// the current working directory — not from repo. Callers opening a repository
// that is not the cwd should wrap ctx with that worktree root (as dispatch does).
// Resolution is fail-soft: a missing or unreadable settings file yields the
// default git backend with no mirrors, preserving default behavior.
func Open(ctx context.Context, repo *git.Repository, opts OpenOptions) (*Stores, error) {
	refs := resolveOpenRefs(ctx, opts)
	env := OpenEnv{Repo: repo, BlobFetcher: opts.BlobFetcher, Refs: refs}

	cfg, err := settings.LoadCheckpointsConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoints config: %w", err)
	}

	primary, err := buildPrimary(ctx, env, cfg)
	if err != nil {
		return nil, err
	}
	mirrors, err := buildMirrors(ctx, env, cfg)
	if err != nil {
		return nil, err
	}

	return &Stores{
		Persistent: newFanoutStore(primary, mirrors),
		ephemeral:  newEphemeralStore(repo, refs),
		refs:       refs,
	}, nil
}

// buildPrimary constructs the primary persistent store. The primary must be the
// git backend: attach, resume, push, doctor, cleanup, and OPF all assume a git
// refs.Primary, so a non-git primary is rejected rather than silently
// half-supported.
func buildPrimary(ctx context.Context, env OpenEnv, cfg *settings.CheckpointsConfig) (PersistentStore, error) {
	typ, raw := BackendTypeGit, json.RawMessage(nil)
	if cfg != nil && cfg.Primary.Type != "" {
		typ, raw = cfg.Primary.Type, cfg.Primary.Config
	}
	if typ != BackendTypeGit {
		return nil, fmt.Errorf("checkpoints.primary.type %q is not supported: only %q may be the primary backend", typ, BackendTypeGit)
	}
	return build(ctx, env, typ, raw)
}

// buildMirrors constructs the mirror writers. A git-typed mirror is rejected: it
// would share the primary ref topology and double-write the same ref, so it is
// never a meaningful independent mirror.
func buildMirrors(ctx context.Context, env OpenEnv, cfg *settings.CheckpointsConfig) ([]Writer, error) {
	if cfg == nil || len(cfg.Mirrors) == 0 {
		return nil, nil
	}
	mirrors := make([]Writer, 0, len(cfg.Mirrors))
	for i, m := range cfg.Mirrors {
		if m.Type == BackendTypeGit {
			return nil, fmt.Errorf("checkpoints.mirrors[%d]: a %q mirror would duplicate the primary ref and is not supported", i, BackendTypeGit)
		}
		store, err := build(ctx, env, m.Type, m.Config)
		if err != nil {
			return nil, fmt.Errorf("checkpoints.mirrors[%d]: %w", i, err)
		}
		mirrors = append(mirrors, store)
	}
	return mirrors, nil
}

func resolveOpenRefs(ctx context.Context, opts OpenOptions) PersistentRefs {
	if opts.Refs != nil {
		return *opts.Refs
	}
	return ResolveRefs(ctx)
}

// Ephemeral returns the git-backed shadow-branch (temporary) store.
func (s *Stores) Ephemeral() EphemeralStore { return s.ephemeral }

// Refs returns the resolved committed-ref topology.
func (s *Stores) Refs() PersistentRefs { return s.refs }
