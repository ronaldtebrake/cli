//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeImportFixture is a two-turn Claude transcript used to verify enable-time import.
const claudeImportFixture = `{"type":"user","uuid":"u1","timestamp":"2026-06-20T00:00:00Z","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":5}}}
{"type":"user","uuid":"u2","timestamp":"2026-06-20T00:01:00Z","message":{"role":"user","content":"second"}}
`

// freshRepoEnv builds a repo with an initial commit but WITHOUT Entire enabled,
// so `entire enable` runs its real first-time flow.
func freshRepoEnv(t *testing.T) *TestEnv {
	t.Helper()
	env := NewTestEnv(t)
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	return env
}

func TestEnableOffersImport_FirstRunAutoImports(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)

	// Pre-existing Claude history for this repo.
	require.NoError(t, os.WriteFile(
		filepath.Join(env.ClaudeProjectDir, "sess1.jsonl"),
		[]byte(claudeImportFixture), 0o644))

	// First-time enable in a non-interactive (no-TTY) subprocess auto-imports the
	// selected agent's discoverable history.
	out := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.Contains(t, out, "Imported 2 turn(s)", "first-time enable should import discovered history; got: %s", out)

	// The imported turns are real checkpoints on the v1 metadata branch.
	require.Contains(t, env.RunCLI("checkpoint", "list"), "[imported]",
		"imported checkpoints should be listed")
}

func TestEnableOffersImport_NoHistoryIsSilent(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	// No transcripts written: nothing discoverable.

	out := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	require.Contains(t, out, "Ready.", "enable should complete; got: %s", out)
	require.NotContains(t, out, "Imported", "no history => import offer must be a silent no-op; got: %s", out)
}

func TestEnableOffersImport_NotOfferedOnReEnable(t *testing.T) {
	t.Parallel()
	env := freshRepoEnv(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.ClaudeProjectDir, "sess1.jsonl"),
		[]byte(claudeImportFixture), 0o644))

	// First enable imports.
	first := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	require.Contains(t, first, "Imported 2 turn(s)", "first enable should import; got: %s", first)

	// Re-enable must not re-offer or re-import, even though history is still present.
	second := env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	require.NotContains(t, second, "Imported", "re-enable must not offer import again; got: %s", second)
	require.False(t, strings.Contains(second, "already imported"),
		"re-enable must not run import at all; got: %s", second)
}
