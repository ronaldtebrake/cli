//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestV2DualWrite_Disabled verifies that when checkpoints_v2 is NOT enabled,
// no v2 refs are created.
func TestV2DualWrite_Disabled(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-disabled")

	// Initialize WITHOUT checkpoints_v2
	env.InitEntire()

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add helper")
	require.NoError(t, err)

	env.WriteFile("helper.go", "package main\n\nfunc Helper() {}")
	session.CreateTranscript(
		"Add helper",
		[]FileChange{{Path: "helper.go", Content: "package main\n\nfunc Helper() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Add helper", "helper.go")

	// v1 should exist
	assert.True(t, env.BranchExists(paths.MetadataBranchName),
		"v1 metadata branch should exist")

	// v2 refs should NOT exist
	assert.False(t, env.RefExists(paths.V2MainRefName),
		"v2 /main ref should NOT exist when v2 is disabled")
	assert.False(t, env.RefExists(paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist when v2 is disabled")
}

// TestCheckpointsVersion2Disallowed_WritesV1 verifies that setting
// strategy_options.checkpoints_version: 2 is now ignored — v1 metadata is
// written and v2 refs are not created by the setting alone.
func TestCheckpointsVersion2Disallowed_WritesV1(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/checkpoints-v2-disallowed")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_version": 2,
	})

	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add greeting function"))

	env.WriteFile("greet.go", "package main\n\nfunc Greet() string { return \"hello\" }")
	session.CreateTranscript(
		"Add greeting function",
		[]FileChange{{Path: "greet.go", Content: "package main\n\nfunc Greet() string { return \"hello\" }"}},
	)
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	env.GitCommitWithShadowHooks("Add greeting function", "greet.go")

	cpIDStr := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpIDStr, "checkpoint ID should be in commit trailer")

	cpID, err := id.NewCheckpointID(cpIDStr)
	require.NoError(t, err)
	cpPath := cpID.Path()

	_, found := env.ReadFileFromBranch(paths.MetadataBranchName, cpPath+"/"+paths.MetadataFileName)
	assert.True(t, found,
		"v1 committed checkpoint metadata should be written even with checkpoints_version: 2 (the setting is disallowed)")

	assert.False(t, env.RefExists(paths.V2MainRefName),
		"v2 /main ref should NOT exist solely because checkpoints_version: 2 is configured")
	assert.False(t, env.RefExists(paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist solely because checkpoints_version: 2 is configured")
}

// TestCheckpointsVersion2Disallowed_HookDrivenCommitOnMain verifies the same
// hook-driven prompt -> stop -> commit flow used by the attach E2E
// precondition setup. checkpoints_version: 2 is now disallowed, so the normal
// git hook path writes v1 metadata even when the setting is configured.
func TestCheckpointsVersion2Disallowed_HookDrivenCommitOnMain(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_version": 2,
	})

	env.GitAdd(".entire/settings.json")
	env.GitCommit("Enable entire")

	session := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add existing checkpoint doc"))

	env.WriteFile("docs/existing.md", "# Existing\n\nA short paragraph about existing checkpoints.\n")
	session.CreateTranscript(
		"Add existing checkpoint doc",
		[]FileChange{{Path: "docs/existing.md", Content: "# Existing\n\nA short paragraph about existing checkpoints.\n"}},
	)
	require.NoError(t, env.SimulateStop(session.ID, session.TranscriptPath))

	env.GitCommitWithShadowHooks("Add existing checkpoint doc", "docs/existing.md")

	cpIDStr := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpIDStr, "checkpoint ID should be in commit trailer")

	cpID, err := id.NewCheckpointID(cpIDStr)
	require.NoError(t, err)
	cpPath := cpID.Path()

	v1Summary, found := env.ReadFileFromBranch(paths.MetadataBranchName, cpPath+"/0/"+paths.MetadataFileName)
	require.True(t, found, "v1 metadata branch should be written even with checkpoints_version: 2 (the setting is disallowed)")
	assert.Contains(t, v1Summary, cpIDStr)

	assert.False(t, env.RefExists(paths.V2MainRefName),
		"v2 /main ref should NOT exist solely because checkpoints_version: 2 is configured")
	assert.False(t, env.RefExists(paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist solely because checkpoints_version: 2 is configured")
}
