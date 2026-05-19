package claudecode

import (
	"context"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
)

// claudeCodeSpawner produces argv: claude -p <prompt>; no stdin.
type claudeCodeSpawner struct{}

// NewSpawner returns a Spawner for claude-code's non-interactive review/investigate mode.
func NewSpawner() spawn.Spawner { return claudeCodeSpawner{} }

func (claudeCodeSpawner) Name() string { return string(agent.AgentNameClaudeCode) }

func (claudeCodeSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	// --permission-mode bypassPermissions auto-accepts every tool call.
	// `-p` (print) mode has no UI to answer permission prompts, so the
	// default mode silently denies anything that isn't pre-approved —
	// not just Write/Edit but also Bash (the previous `acceptEdits`
	// fix only unblocked file writes; `entire investigate` agents still
	// had their `git`, `grep`, `ls` invocations denied and gave up
	// without writing pending_turn to state.json). The prompt sent by
	// the CLI instructs the agent not to run destructive commands, so
	// we rely on prompt-level discipline rather than tool-level prompts.
	// Review doesn't issue any tool calls that would otherwise be
	// blocked, so the flag is a no-op for that path.
	cmd := exec.CommandContext(ctx, "claude", "-p", "--permission-mode", "bypassPermissions", prompt)
	cmd.Env = env
	return cmd
}
