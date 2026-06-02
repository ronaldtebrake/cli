package strategy

import (
	"context"
	"log/slog"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// mirrorMetadataToV1CustomRef advances the topology's mirror to the primary's
// commit when a mirror is configured. Call after a successful primary write; a
// mirror failure must not affect that write, so problems are logged, not returned.
func mirrorMetadataToV1CustomRef(ctx context.Context, repo *git.Repository) {
	refs := checkpoint.ResolveCommittedRefs(ctx)
	if !refs.HasMirror() {
		return
	}

	primaryRef, err := repo.Reference(refs.Primary, true)
	if err != nil {
		// No primary metadata ref yet — nothing to mirror. Expected on first use.
		logging.Debug(ctx, "committed-ref mirror skipped: primary metadata ref unavailable",
			slog.String("error", err.Error()))
		return
	}

	if err := SafelyAdvanceLocalRef(ctx, repo, refs.Mirror, primaryRef.Hash()); err != nil {
		logging.Warn(ctx, "committed-ref mirror failed",
			slog.String("ref", refs.Mirror.String()),
			slog.String("error", err.Error()))
		return
	}

	logging.Debug(ctx, "committed-ref mirror updated",
		slog.String("ref", refs.Mirror.String()),
		slog.String("hash", primaryRef.Hash().String()))
}
