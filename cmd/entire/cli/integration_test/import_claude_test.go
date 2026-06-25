//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/importclaude"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

func TestImportClaudeCode_EndToEnd(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Write a two-turn Claude transcript into the (overridden) Claude project dir.
	sessionID := "sess1"
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl"), []byte(content), 0o644))

	importsRef := plumbing.NewBranchReferenceName(paths.ImportsBranchName)

	// 1. Dry-run reports counts and writes nothing.
	out := env.RunCLI("import", "claude-code", "--dry-run")
	require.Contains(t, out, "Would import 2", "dry-run should count 2 turns; got: %s", out)
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	if _, err := repo.Reference(importsRef, false); err == nil {
		t.Fatal("dry-run must not create the imports ref")
	}

	// 2. Real import creates the imports ref.
	out = env.RunCLI("import", "claude-code")
	require.Contains(t, out, "Imported 2", "got: %s", out)
	ref, err := repo.Reference(importsRef, false)
	require.NoError(t, err, "imports ref should exist after import")
	require.False(t, ref.Hash().IsZero())

	// 3. checkpoint list surfaces imported entries labeled [imported].
	listOut := env.RunCLI("checkpoint", "list")
	require.Contains(t, listOut, "[imported]", "checkpoint list should label imported checkpoints; got: %s", listOut)

	// 4. Re-running import is idempotent.
	out = env.RunCLI("import", "claude-code")
	require.Contains(t, out, "(2 already imported)", "re-run should skip already-imported turns; got: %s", out)

	// 5. Rewinding to an imported checkpoint is refused with a clear message.
	importedID := importclaude.DeriveCheckpointID(sessionID, "u1").String()
	rewindOut, rewindErr := env.RunCLIWithError("checkpoint", "rewind", "--to", importedID)
	require.Error(t, rewindErr, "rewind to imported checkpoint should fail")
	require.Contains(t, rewindOut, "read-only and not rewindable", "got: %s", rewindOut)
}
