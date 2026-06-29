package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/go-git/go-git/v6"
)

func readLocalCheckpointPolicy(ctx context.Context, repo *git.Repository) (checkpointpolicy.Policy, bool) {
	state, err := checkpointpolicy.ReadLocal(ctx, repo)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy read failed; allowing checkpoint write",
			slog.String("error", err.Error()),
		)
		return checkpointpolicy.Policy{}, false
	}
	return state.Policy, true
}

func syncCheckpointPolicyForPrePush(ctx context.Context, ps pushSettings) {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to open repository; allowing checkpoint push",
			slog.String("error", err.Error()),
		)
		return
	}
	defer repo.Close()

	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to resolve worktree root; allowing checkpoint push",
			slog.String("error", err.Error()),
		)
		return
	}
	target := checkpointpolicy.Target{Remote: ps.pushTarget(), Dir: dir}
	state, err := checkpointpolicy.Sync(ctx, repo, target)
	if err != nil {
		warnOrLogCheckpointPolicySyncFailure(ctx, err)
		localState, readErr := checkpointpolicy.ReadLocal(ctx, repo)
		if readErr == nil {
			warnIfCheckpointPolicyNeedsUpgrade(ctx, localState.Policy)
		}
		return
	}
	if state.Source == checkpointpolicy.SourceLocalDiverged {
		warnOrLogCheckpointPolicyDiverged(ctx, state)
		return
	}
	warnIfCheckpointPolicyNeedsUpgrade(ctx, state.Policy)
}

func warnOrLogCheckpointPolicySyncFailure(ctx context.Context, err error) {
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(stderrWriter, "[entire] Could not refresh checkpoint policy: %v\n", err)
		return
	}
	logging.Warn(ctx, "checkpoint policy sync failed",
		slog.String("error", err.Error()),
	)
}

func warnOrLogCheckpointPolicyDiverged(ctx context.Context, state checkpointpolicy.State) {
	if interactive.CanPromptInteractively() {
		fmt.Fprintf(
			stderrWriter,
			"[entire] Could not reconcile checkpoint policy: local checkpoint policy %s diverges from remote %s\n",
			state.Hash,
			state.RemoteHash,
		)
		return
	}
	logging.Warn(ctx, "checkpoint policy diverged; allowing checkpoint push",
		slog.String("local_hash", state.Hash.String()),
		slog.String("remote_hash", state.RemoteHash.String()),
	)
}

func warnIfCheckpointPolicyNeedsUpgrade(ctx context.Context, policy checkpointpolicy.Policy) {
	if checkpointpolicy.CanSatisfyPolicy(policy) {
		return
	}
	warnOrLogCheckpointPolicyUpgrade(ctx, policy, checkpointpolicy.CheckpointVersion(policy))
}

func warnOrLogCheckpointPolicyUpgrade(ctx context.Context, policy checkpointpolicy.Policy, version string) {
	warning := checkpointpolicy.UnsupportedPolicyMessage(policy, versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version))
	if interactive.CanPromptInteractively() {
		fmt.Fprint(stderrWriter, warning)
		return
	}
	logging.Warn(ctx, "checkpoint policy requires newer CLI; using checkpoint version",
		slog.String("checkpoint_version", policy.CheckpointVersion),
		slog.String("checkpoint_min_version", policy.CheckpointMinVersion),
		slog.String("using_checkpoint_version", version),
	)
}
