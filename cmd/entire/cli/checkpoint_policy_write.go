package cli

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6"
)

func committedCheckpointVersion(ctx context.Context, repo *git.Repository) string {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		version := checkpointpolicy.DefaultCheckpointVersion()
		logging.Warn(ctx, "checkpoint policy read failed; using default checkpoint version",
			slog.String("error", err.Error()),
			slog.String("using_checkpoint_version", version),
		)
		return version
	}
	return checkpointpolicy.CheckpointVersion(state.Policy)
}
