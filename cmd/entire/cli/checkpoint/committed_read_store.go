package checkpoint

import (
	"context"
	"errors"
	"log/slog"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// NewCommittedReadStore returns a GitStore bound to the topology's read ref.
func NewCommittedReadStore(ctx context.Context, repo *git.Repository) *GitStore {
	return NewGitStoreWithRef(repo, ResolveCommittedRefs(ctx).Read)
}

// SyncCommittedReadRef advances the mirror ref to the primary tip before a read
// so a git pull (which updates the primary but not the local-only mirror) is
// reflected. No-op without a mirror. Best-effort.
func SyncCommittedReadRef(ctx context.Context, repo *git.Repository) {
	refs := ResolveCommittedRefs(ctx)
	if !refs.HasMirror() {
		return
	}
	syncMirrorForRead(ctx, repo, refs)
}

// syncMirrorForRead advances the mirror ref to the primary tip (local primary
// ref, or origin's on a fresh clone): seed when missing, advance when an
// ancestor, no-op when equal, leave a diverged ref as-is. Failures are logged.
func syncMirrorForRead(ctx context.Context, repo *git.Repository, refs CommittedRefs) {
	primaryHash, ok := resolvePrimaryTip(repo, refs.Primary)
	if !ok {
		logging.Debug(ctx, "mirror read sync skipped: no primary tip available")
		return
	}

	mirrorRefName := refs.Mirror
	mirrorRef, err := repo.Reference(mirrorRefName, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		setMirrorRef(ctx, repo, mirrorRefName, primaryHash) // missing — seed at primary tip
		return
	}
	if err != nil {
		// Unexpected read error — don't overwrite the ref; read it as-is.
		logging.Warn(ctx, "mirror read sync skipped: mirror ref unreadable",
			slog.String("ref", mirrorRefName.String()),
			slog.String("error", err.Error()))
		return
	}

	if mirrorRef.Hash() == primaryHash {
		return // already current
	}

	mirrorCommit, err := repo.CommitObject(mirrorRef.Hash())
	if err != nil {
		logging.Warn(ctx, "mirror read sync skipped: mirror ref commit unreadable",
			slog.String("ref", mirrorRefName.String()),
			slog.String("error", err.Error()))
		return
	}
	primaryCommit, err := repo.CommitObject(primaryHash)
	if err != nil {
		logging.Warn(ctx, "mirror read sync skipped: primary commit unreadable",
			slog.String("error", err.Error()))
		return
	}

	isAncestor, err := mirrorCommit.IsAncestor(primaryCommit)
	if err != nil {
		logging.Warn(ctx, "mirror read sync skipped: ancestry check failed",
			slog.String("error", err.Error()))
		return
	}
	if !isAncestor {
		// Diverged from the primary: leave the ref untouched and read it as-is.
		logging.Warn(ctx, "mirror ref diverged from primary; reading mirror ref as-is",
			slog.String("ref", mirrorRefName.String()),
			slog.String("mirror_hash", mirrorRef.Hash().String()),
			slog.String("primary_hash", primaryHash.String()))
		return
	}

	setMirrorRef(ctx, repo, mirrorRefName, primaryHash)
}

// resolvePrimaryTip returns the primary metadata tip, preferring the local
// primary ref and falling back to origin's remote-tracking branch (so the
// mirror can seed on a fresh clone).
func resolvePrimaryTip(repo *git.Repository, primary plumbing.ReferenceName) (plumbing.Hash, bool) {
	if ref, err := repo.Reference(primary, true); err == nil {
		return ref.Hash(), true
	}
	if primary.IsBranch() {
		tracking := plumbing.NewRemoteReferenceName("origin", primary.Short())
		if ref, err := repo.Reference(tracking, true); err == nil {
			return ref.Hash(), true
		}
	}
	return plumbing.ZeroHash, false
}

// setMirrorRef points refName at hash; failures are logged and swallowed so the
// read can proceed against the ref as-is.
func setMirrorRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, hash plumbing.Hash) {
	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		logging.Warn(ctx, "mirror read sync failed to advance mirror ref",
			slog.String("ref", refName.String()),
			slog.String("error", err.Error()))
		return
	}
	logging.Debug(ctx, "mirror ref synced for read",
		slog.String("ref", refName.String()),
		slog.String("hash", hash.String()))
}
