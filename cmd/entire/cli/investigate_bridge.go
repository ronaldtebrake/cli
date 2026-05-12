package cli

// investigate_bridge.go wires cli-package implementations into the
// investigate subpackage's NewCommand Deps struct. Functions that need
// agent registry access or checkpoint summaries live here to avoid the
// import cycle:
//
//	investigate → checkpoint → ... → investigate
//	investigate → claudecode/codex/geminicli → investigate
//
// The bridge mirrors review_bridge.go so the two experimental commands
// share a single wiring pattern.

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agentlaunch"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
)

// buildInvestigateDeps builds the investigate.Deps used by
// investigate.NewCommand. attachCmd is the cobra subcommand for
// `entire investigate attach`; pass nil in tests that don't need it.
//
// PriorEntireContextFn is left nil — a future task can wire `entire
// search` lookup so investigate seeds inherit prior context. LoopRun is
// also nil so production uses investigate.RunInvestigateLoop directly.
func buildInvestigateDeps(attachCmd *cobra.Command) investigate.Deps {
	return investigate.Deps{
		GetAgentsWithHooksInstalled: GetAgentsWithHooksInstalled,
		NewSilentError: func(err error) error {
			return NewSilentError(err)
		},
		SpawnerFor:                   launchableSpawnerFor,
		LaunchFix:                    agentlaunch.LaunchFixAgent,
		PriorEntireContextFn:         nil,
		AttachCmd:                    attachCmd,
		HeadHasInvestigateCheckpoint: headHasInvestigateCheckpoint,
	}
}

// launchableSpawnerFor returns the Spawner for known launchable agents,
// or nil for non-launchable agents (cursor, opencode, factoryai-droid,
// copilot-cli, vogon). Lives here for the same reason
// launchableReviewerFor does — to avoid the investigate subpackage
// importing the per-agent packages, which would create an import cycle.
func launchableSpawnerFor(agentName string) spawn.Spawner {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewSpawner()
	case string(agent.AgentNameCodex):
		return codex.NewSpawner()
	case string(agent.AgentNameGemini):
		return geminicli.NewSpawner()
	default:
		return nil
	}
}

// newInvestigateAttachCmd builds the `entire investigate attach
// <session-id>` cobra subcommand wired to AttachSession in the cli
// package. Mirrors newReviewAttachCmd.
func newInvestigateAttachCmd() *cobra.Command {
	return investigate.NewAttachCommand(investigate.AttachDeps{
		Attach: func(ctx context.Context, sessionID string, runID string, round, turn int, topic, prompt string) error {
			return AttachSession(ctx, AttachOptions{
				Kind:              AttachKindInvestigate,
				SessionID:         sessionID,
				InvestigateRunID:  runID,
				InvestigateRound:  round,
				InvestigateTurn:   turn,
				InvestigateTopic:  topic,
				InvestigatePrompt: prompt,
			})
		},
		NewSilentError: func(err error) error { return NewSilentError(err) },
	})
}
