package checkpoint

import (
	"context"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// OpenOptions configures Open. The zero value resolves the committed-ref
// topology from on-disk settings and attaches no blob fetcher.
type OpenOptions struct {
	// BlobFetcher is the CLI-level on-demand blob fetcher. The checkpoint
	// package cannot resolve it itself, so the CLI layer injects it here and
	// Open attaches it to the constructed store(s). nil leaves on-demand
	// fetching off.
	BlobFetcher BlobFetchFunc

	// Settings overrides on-disk settings when resolving the committed-ref
	// topology. nil resolves from disk via ResolveCommittedRefs. Ignored when
	// Refs is non-nil.
	Settings *settings.EntireSettings

	// Refs overrides the resolved committed-ref topology outright. nil resolves
	// from Settings (or disk). A non-nil value wins — e.g. attach pins reads to
	// Primary via PrimaryAsRead().
	Refs *CommittedRefs
}

// Stores is the facade returned by Open: the committed store plus the git-only
// temporary capability and the resolved topology accessors callers need during
// the transition to pluggable backends.
//
// Phase 0 (centralized construction) holds the concrete *GitStore in Primary,
// and the same instance backs Temporary(). Later phases narrow Primary to a
// pluggable committed-store interface and add independent-backend mirrors
// without changing this call-site-facing API.
type Stores struct {
	// Primary is the committed store — the source of truth that serves all
	// committed reads and writes.
	Primary *GitStore

	temporary *GitStore
	refs      CommittedRefs
}

// Open resolves the checkpoint storage topology and constructs the backing
// store(s). It is the single construction seam that replaces scattered
// NewGitStore(repo, ResolveCommittedRefs(ctx)) calls, so ref resolution and
// blob-fetcher wiring live in one place.
//
//nolint:unparam // The error result is part of the forward-looking facade contract: pluggable backends (Phase 2+) can fail to open. The git backend never returns one today.
func Open(ctx context.Context, repo *git.Repository, opts OpenOptions) (*Stores, error) {
	refs := resolveOpenRefs(ctx, opts)
	store := NewGitStore(repo, refs)
	if opts.BlobFetcher != nil {
		store.SetBlobFetcher(opts.BlobFetcher)
	}
	return &Stores{
		Primary:   store,
		temporary: store,
		refs:      refs,
	}, nil
}

func resolveOpenRefs(ctx context.Context, opts OpenOptions) CommittedRefs {
	switch {
	case opts.Refs != nil:
		return *opts.Refs
	case opts.Settings != nil:
		return ResolveCommittedRefsFromSettings(opts.Settings)
	default:
		return ResolveCommittedRefs(ctx)
	}
}

// Temporary returns the git-backed temporary (shadow-branch) store. Temporary
// capture is inherently git-only; a future non-git Primary would leave this
// pointing at a dedicated git store.
func (s *Stores) Temporary() *GitStore { return s.temporary }

// Refs returns the resolved committed-ref topology. Transition accessor: this
// ref logic moves behind sync/admin capabilities in a later phase.
func (s *Stores) Refs() CommittedRefs { return s.refs }

// Repository returns the underlying git repository. Transition accessor for
// git-topology operations (e.g. mirror repair) that have not yet moved behind a
// capability.
func (s *Stores) Repository() *git.Repository { return s.Primary.Repository() }
