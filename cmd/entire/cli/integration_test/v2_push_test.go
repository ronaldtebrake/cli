//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bareRefExists checks if a ref exists in a bare repo by running git ls-remote.
func bareRefExists(t *testing.T, bareDir, refName string) bool {
	t.Helper()
	cmd := exec.Command("git", "ls-remote", bareDir, refName)
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) != ""
}

func TestV2Push_Disabled_NoV2Refs(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-push-disabled")

	// Enable checkpoints_v2 but NOT push_v2_refs
	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	bareDir := env.SetupBareRemote()

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add feature")
	require.NoError(t, err)

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}")
	session.CreateTranscript(
		"Add feature",
		[]FileChange{{Path: "feature.go", Content: "package main\n\nfunc Feature() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitAdd("feature.go")
	env.GitCommitWithShadowHooks("Add feature")

	env.RunPrePush("origin")

	// v2 refs should NOT be pushed
	assert.False(t, bareRefExists(t, bareDir, paths.V2MainRefName),
		"v2 /main ref should NOT exist on remote when push_v2_refs is disabled")
	assert.False(t, bareRefExists(t, bareDir, paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist on remote when push_v2_refs is disabled")

	// v1 should still be pushed
	assert.True(t, bareRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"v1 metadata branch should still exist on remote")
}

// TestPush_CheckpointsVersion2DisallowedFallsBackToV1 verifies that setting
// strategy_options.checkpoints_version: 2 is now ignored — the v1 metadata
// branch is pushed (as if checkpoints_version were unset), and no v2 refs are
// created by the setting alone.
func TestPush_CheckpointsVersion2DisallowedFallsBackToV1(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/checkpoints-v2-disallowed-push")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_version": 2,
	})

	bareDir := env.SetupBareRemote()

	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add feature"))

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}")
	session.CreateTranscript(
		"Add feature",
		[]FileChange{{Path: "feature.go", Content: "package main\n\nfunc Feature() {}"}},
	)
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	env.GitAdd("feature.go")
	env.GitCommitWithShadowHooks("Add feature")

	env.RunPrePush("origin")

	assert.True(t, bareRefExists(t, bareDir, "refs/heads/"+paths.MetadataBranchName),
		"v1 metadata branch should be pushed even with checkpoints_version: 2 (the setting is disallowed)")
	assert.False(t, bareRefExists(t, bareDir, paths.V2MainRefName),
		"v2 /main ref should NOT be pushed solely because checkpoints_version: 2 is configured")
}
