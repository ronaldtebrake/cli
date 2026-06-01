package checkpoint

import (
	"context"
	"errors"
	"log/slog"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// NewCommittedReadStore returns a GitStore for reading committed checkpoints:
// the local-only v1.1 custom ref when checkpoints_version 1.1 is enabled (no v1
// fallback), else the v1 branch. Pure — callers run SyncCommittedReadRef first
// to bring the v1.1 ref up to date.
func NewCommittedReadStore(ctx context.Context, repo *git.Repository) *GitStore {
	if !settings.MirrorsToV1CustomRef(ctx) {
		return NewGitStore(repo)
	}
	return NewGitStoreWithRef(repo, plumbing.ReferenceName(paths.MetadataRefName))
}

// SyncCommittedReadRef advances the v1.1 custom ref to the v1 tip before a read,
// a no-op unless checkpoints_version 1.1 is enabled. The ref is local-only, so a
// git pull updates v1 but not v1.1; this keeps it current. Best-effort.
func SyncCommittedReadRef(ctx context.Context, repo *git.Repository) {
	if !settings.MirrorsToV1CustomRef(ctx) {
		return
	}
	syncV1CustomRefForRead(ctx, repo)
}

// syncV1CustomRefForRead advances the v1.1 custom ref to the v1 tip (local v1
// branch, or origin's on a fresh clone): seed when missing, advance when an
// ancestor, no-op when equal, leave a diverged ref as-is. Failures are logged.
func syncV1CustomRefForRead(ctx context.Context, repo *git.Repository) {
	v1Hash, ok := resolveV1Tip(repo)
	if !ok {
		logging.Debug(ctx, "v1.1 read sync skipped: no v1 tip available")
		return
	}

	customRefName := plumbing.ReferenceName(paths.MetadataRefName)
	customRef, err := repo.Reference(customRefName, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		setCustomRef(ctx, repo, customRefName, v1Hash) // missing — seed at v1 tip
		return
	}
	if err != nil {
		// Unexpected read error — don't overwrite the ref; read it as-is.
		logging.Warn(ctx, "v1.1 read sync skipped: custom ref unreadable",
			slog.String("ref", paths.MetadataRefName),
			slog.String("error", err.Error()))
		return
	}

	if customRef.Hash() == v1Hash {
		return // already current
	}

	customCommit, err := repo.CommitObject(customRef.Hash())
	if err != nil {
		logging.Warn(ctx, "v1.1 read sync skipped: custom ref commit unreadable",
			slog.String("ref", paths.MetadataRefName),
			slog.String("error", err.Error()))
		return
	}
	v1Commit, err := repo.CommitObject(v1Hash)
	if err != nil {
		logging.Warn(ctx, "v1.1 read sync skipped: v1 commit unreadable",
			slog.String("error", err.Error()))
		return
	}

	isAncestor, err := customCommit.IsAncestor(v1Commit)
	if err != nil {
		logging.Warn(ctx, "v1.1 read sync skipped: ancestry check failed",
			slog.String("error", err.Error()))
		return
	}
	if !isAncestor {
		// Diverged from v1: leave the ref untouched and read it as-is.
		logging.Warn(ctx, "v1.1 custom ref diverged from v1; reading custom ref as-is",
			slog.String("ref", paths.MetadataRefName),
			slog.String("custom_hash", customRef.Hash().String()),
			slog.String("v1_hash", v1Hash.String()))
		return
	}

	setCustomRef(ctx, repo, customRefName, v1Hash)
}

// resolveV1Tip returns the v1 metadata tip, preferring the local v1 branch and
// falling back to origin's remote-tracking branch (so v1.1 can seed on a fresh
// clone).
func resolveV1Tip(repo *git.Repository) (plumbing.Hash, bool) {
	if ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true); err == nil {
		return ref.Hash(), true
	}
	if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), true); err == nil {
		return ref.Hash(), true
	}
	return plumbing.ZeroHash, false
}

// setCustomRef points refName at hash; failures are logged and swallowed so the
// read can proceed against the ref as-is.
func setCustomRef(ctx context.Context, repo *git.Repository, refName plumbing.ReferenceName, hash plumbing.Hash) {
	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		logging.Warn(ctx, "v1.1 read sync failed to advance custom ref",
			slog.String("ref", refName.String()),
			slog.String("error", err.Error()))
		return
	}
	logging.Debug(ctx, "v1.1 custom ref synced for read",
		slog.String("ref", refName.String()),
		slog.String("hash", hash.String()))
}
