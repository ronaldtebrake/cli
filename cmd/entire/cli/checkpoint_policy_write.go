package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

var errUnsupportedCheckpointPolicy = errors.New("checkpoint policy cannot be satisfied by this Entire CLI")

func checkpointVersionForNewCheckpoint(ctx context.Context, repo *git.Repository) (string, error) {
	policy := localCheckpointPolicyForNewCheckpoint(ctx, repo)
	if !checkpointpolicy.CanSatisfyPolicy(policy) {
		return "", unsupportedCheckpointPolicyError(policy)
	}
	return checkpointpolicy.CheckpointVersion(policy), nil
}

func ensureCheckpointPolicyAllowsCheckpointData(ctx context.Context, repo *git.Repository) error {
	policy := localCheckpointPolicyForNewCheckpoint(ctx, repo)
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return nil
	}
	return unsupportedCheckpointPolicyError(policy)
}

func localCheckpointPolicyForNewCheckpoint(ctx context.Context, repo *git.Repository) checkpointpolicy.Policy {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy read failed; using default checkpoint policy",
			slog.String("error", err.Error()),
		)
		return checkpointpolicy.DefaultPolicy()
	}
	return state.Policy
}

func unsupportedCheckpointPolicyError(policy checkpointpolicy.Policy) error {
	message := strings.TrimSpace(checkpointpolicy.UnsupportedPolicyMessage(
		policy,
		versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version),
	))
	return fmt.Errorf("%w:\n%s", errUnsupportedCheckpointPolicy, message)
}
