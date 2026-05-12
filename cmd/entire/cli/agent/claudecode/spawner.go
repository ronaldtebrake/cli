package claudecode

import (
	"context"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
)

// claudeCodeSpawner produces argv: claude -p <prompt>; no stdin.
type claudeCodeSpawner struct{}

// NewSpawner returns a Spawner for claude-code's non-interactive review/investigate mode.
//

func NewSpawner() spawn.Spawner { return claudeCodeSpawner{} }

func (claudeCodeSpawner) Name() string { return "claude-code" }

func (claudeCodeSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	// --permission-mode acceptEdits enables file writes in non-interactive
	// mode. Without it, Write/Edit tool calls in `-p` (print) mode are
	// silently denied because there's no UI to answer the permission
	// prompt — which breaks `entire investigate` (the agent can't write
	// the findings/timeline doc) and would break any future shared use
	// that needs writes. Review doesn't write files in practice, so the
	// flag is a no-op for that path.
	cmd := exec.CommandContext(ctx, "claude", "-p", "--permission-mode", "acceptEdits", prompt)
	cmd.Env = env
	return cmd
}
