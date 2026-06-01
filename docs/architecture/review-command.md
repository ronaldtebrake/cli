# `entire review` Command

`entire review` runs a named review profile. A profile defines one canonical task (for example `general`, `security`, or `accessibility`), a set of worker agents that all run that task, and an optional master agent that critically adjudicates worker reports into one final report. Worker review sessions are immutable facts attached to checkpoints; the master report is stored locally in the review manifest for findings/fix workflows.

## Command Surface

```
entire review                          # Run the default review profile
entire review security                 # Run a named profile
entire review --profile accessibility  # Same, flag form
entire review --edit --profile general # Configure a profile locally
entire review --agent <name>           # Run one worker from the selected profile
entire review --prompt "focus on auth" # Add one-off instructions
entire review attach <session-id>      # Tag an existing agent session as a review (post-hoc)
entire review attach --force           # Skip confirmation
entire review attach --agent <name>    # Agent that created the session
entire review attach --skills <s,...>  # Declare which skills were run
```

When no profiles are configured, `entire review` creates an opinionated clone-local `general` profile from installed launchable agents and runs immediately — no first-run skills picker. The defaults are intentionally simple: Claude/Codex use `/review`, Gemini uses the profile task directly, and Claude is preferred as master when available.

When two or more launchable agents are configured in the selected profile and `--agent` is not set, `entire review` fans out to all configured workers. There is no per-run multi-picker: the profile is the fan-out contract. Profiles with multiple workers must set `master`; the master runs after workers finish and produces the canonical final report.

## Settings Schema

Review profiles are configured in clone-local preferences (or settings) under `review_profiles`:

```json
{
  "review_default_profile": "general",
  "review_profiles": {
    "general": {
      "task": "Review this change for correctness, regressions, tests, and maintainability.",
      "agents": {
        "claude-code": {"skills": ["/review"]},
        "codex": {"skills": ["/review"]}
      },
      "master": "claude-code"
    },
    "security": {
      "task": "Review this change for auth, injection, secrets, and privilege-boundary bugs.",
      "agents": {
        "claude-code": {"skills": ["/security-review"]},
        "codex": {"skills": ["/review"], "prompt": "Focus on security."}
      },
      "master": "claude-code"
    }
  }
}
```

The profile-level `task` is the shared work item. Per-agent `skills` and `prompt` adapt that task to agent-specific mechanics. Settings fields: `EntireSettings.ReviewProfiles` and `EntireSettings.ReviewDefaultProfile` in `cmd/entire/cli/settings/settings.go`. The old top-level `review` map is no longer used by `entire review`.

## How It Works (env-var handshake)

1. `entire review` selects a profile (positional/`--profile` → `review_default_profile` → `general` → only configured profile). If no profiles exist, it writes an opinionated clone-local default profile from installed launchable agents and continues. It then composes worker prompts via `review.ComposeReviewPrompt` and computes scope (mainline base ref via `review.ComputeScopeStats`, overridable with `--base`).
2. **For launchable agents** (claude-code, codex, gemini-cli): the spawned agent process is given env vars `ENTIRE_REVIEW_{SESSION,AGENT,SKILLS,PROMPT,STARTING_SHA}` that the agent's `UserPromptSubmit` lifecycle hook reads to tag the session as `Kind = "agent_review"` with the configured skills/prompt. Each spawned process has its own env, so multiple worktrees and multi-agent runs are correct by construction (no shared marker file, no race).
3. **For non-launchable agents** (cursor, opencode, factoryai-droid): `RunMarkerFallback` writes a `PendingReviewMarker` file and prints guidance — the user opens the agent themselves and runs the skills. Single shared file (`review/marker_fallback.go`); adding new non-launchable agents is a registry entry, not a new file.
4. Worker agents run the selected profile's task; each session ends naturally.
5. In multi-worker profiles, the configured master agent receives all worker reports and produces one critical final report. The master prompt asks it to reject unsupported claims, resolve contradictions, merge duplicates, and prioritize evidence-backed findings.
6. On the next `git commit`, the PostCommit hook condenses worker review sessions into the checkpoint on `entire/checkpoints/v1`, with `Kind`, `ReviewSkills`, and `ReviewPrompt` recorded in `CommittedMetadata`.
7. The `CheckpointSummary` sets `HasReview = true` for O(1) lookup. `HasReview` is an umbrella "any review happened" flag — future review kinds (e.g. manual review) should also set it.
8. `entire status` and the re-run guard read `HasReview` from the checkpoint metadata (no commit history walking).

## Checkpoint Metadata

Review metadata is stored at two levels on `entire/checkpoints/v1`:

- **`CommittedMetadata` (per-session)**: `kind: "agent_review"`, `review_skills: ["/skill1", "/skill2"]`, `review_prompt: "..."`
- **`CheckpointSummary` (per-checkpoint)**: `has_review: true` (umbrella; set when any session in the checkpoint has a review-kind `Kind`)

## Architecture

- **`AgentReviewer` interface** (`cmd/entire/cli/review/types/reviewer.go`): per-agent contract with `Name() string` and `Start(ctx, RunConfig) (Process, error)`. Each launchable agent implements this in its own package.
- **`ReviewerTemplate`** (`cmd/entire/cli/review/types/template.go`): shared scaffolding (Spawn → pipe stdout → run parser → forward events → close). Each agent supplies only its `BuildCmd` (argv/env) and `Parser` (stdout-to-Event stream).
- **`Sink` interface**: consumers of the event stream. Production sinks: `DumpSink` (post-run per-agent narrative), `TUISink` (Bubble Tea live dashboard with Ctrl+O drill-in), `SynthesisSink` (profile-master final report / legacy prompted synthesis). Sinks are composed by `composeMultiAgentSinks` based on TTY detection.
- **`Run(ctx, reviewer, cfg, sinks)`** (`cmd/entire/cli/review/run.go`): single-agent orchestrator. Forwards events to all sinks via `AgentEvent`, calls `RunFinished` once at end with a populated `RunSummary`. Sink dispatch is serialized; sinks need not internally synchronize.
- **`RunMulti(ctx, reviewers, cfg, sinks)`** (`cmd/entire/cli/review/run_multi.go`): N-agent orchestrator. Each agent runs concurrently in its own goroutine; events fan into a single dispatch loop so the serial-dispatch contract is preserved. Per-agent skills/prompts are injected via `perAgentConfiguredReviewer` adapter (each reviewer sees its own `RunConfig` despite the shared API surface).
- **Env-var contract** (`cmd/entire/cli/review/env.go`): single source of truth for `ENTIRE_REVIEW_*` constants used by spawn-side and lifecycle adoption.
- **Scope detection** (`cmd/entire/cli/review/scope.go`): `detectScopeBaseRef` returns the first existing ref from the fallback chain `origin/HEAD → origin/main → origin/master → main → master`. Overridable per-invocation via `--base <ref>` (validated through go-git's `ResolveRevision`). Banner output: "Reviewing feat/X vs main: 3 commits, 7 files changed, 2 uncommitted".

## Multi-Agent UI

When `RunMulti` is dispatched in a TTY, the sink slice is `[TUISink, DumpSink, SynthesisSink]` for profiles with a master:

- **`TUISink` / `reviewTUIModel`** (`cmd/entire/cli/review/tui_sink.go`, `tui_model.go`, `tui_detail.go`): live dashboard with one row per agent (name, status, tokens, last assistant preview, duration). `Ctrl+O` enters drill-in mode on the alt screen showing the full event buffer for the selected agent; `Esc` returns to the dashboard. `Ctrl+C` cancels the run via the shared `CancelFunc`. The model uses `tea.WithoutSignalHandler` so the cobra root retains SIGINT routing. After all agents finish, the user dismisses with any key — `RunFinished` blocks on dismissal so `DumpSink` renders below the TUI rather than overlapping it.
- **`SynthesisSink`** (`cmd/entire/cli/review/synthesis_sink.go`): in profile-native mode, runs automatically after the dump, composes an adjudication prompt covering all worker narratives + per-run user prompt + profile task, calls the profile master agent, and prints the final report. Skipped when the run was cancelled or fewer than 2 workers produced usable output. Provider failures degrade gracefully ("final report unavailable: <err>") so the user can still commit. The old prompted y/N mode remains available for tests/legacy callers but `entire review` uses auto mode.
- **Sink composition** (`composeMultiAgentSinks` in `cmd/entire/cli/review/cmd.go`): pure helper taking explicit `isTTY`/`canPrompt` so tests don't depend on real TTY detection. `findTUISink` picks the TUI out of the slice for `Start`/`Wait` lifecycle hooks.

## Skill Discovery (Claude Code)

`DiscoverReviewSkills` (`cmd/entire/cli/agent/claudecode/discovery.go`) walks three roots: plugin cache (`~/.claude/plugins/cache/<market>/<plugin>/<version>/{skills,commands,agents}`), user skills (`~/.claude/skills`), user commands/agents (`~/.claude/commands`, `~/.claude/agents`).

For the plugin cache, `pickLatestVersion` picks ONE version directory per plugin: highest valid semver wins; if no entries parse as semver, the lexicographic max is picked (handles the `unknown` sentinel some plugins ship). Without this, multiple installed versions of a plugin produced duplicate skill entries in the picker and prompt.

## Anti-Features (do NOT recreate)

The redesign eliminated several constructs from the prior implementation. None should be reintroduced without explicit design:

- `PendingReviewMarker` for launchable agents (env-var handshake makes it unnecessary)
- `WorktreePath` field + worktree-scoping logic (env per process eliminates the multi-tenant problem)
- `AgentEntries` map on the marker (each agent has its own env)
- Marker overwrite tripwire / refuse-attach guard (the bug classes they defended against don't exist)
- `--track-only` flag (intentionally removed by #1009)
- `--postreview` / `--finalize` / empty review commits / `/entire-review:finish` skill installer
- `Launcher` + `HeadlessLauncher` as separate interfaces (single `AgentReviewer`)
- Codex chrome-line filtering or any agent-specific stdout post-processing in shared multi-agent code (per-agent parsers own their format; shared code only sees `Event` variants)
- `sync.Once`-guarded onCancel + parallel `signal.Notify` goroutine (single cancel from start)

## Key Files

- `cmd/entire/cli/review/cmd.go` — `NewCommand()`, `runReview` dispatch fork, `composeMultiAgentSinks`
- `cmd/entire/cli/review/picker.go` / `profile.go` — profile config picker, first-run setup, profile resolution/default tasks
- `cmd/entire/cli/review/attach.go` + `cli/review_helpers.go:newReviewAttachCmd` — `entire review attach` subcommand
- `cmd/entire/cli/review/marker_fallback.go` — non-launchable agent flow (single shared file)
- `cmd/entire/cli/review/prompt.go` / `scope.go` / `run.go` / `dump.go` / `run_multi.go` — core machinery (single-agent + N-agent fan-in)
- `cmd/entire/cli/review/tui_sink.go` / `tui_model.go` / `tui_detail.go` — Bubble Tea TUI sink
- `cmd/entire/cli/review/synthesis_sink.go` / `synthesis_prompt.go` — opt-in cross-agent verdict
- `cmd/entire/cli/review/types/{reviewer,sink,template}.go` — interface contracts (CU2 + CU4 + CU5b)
- `cmd/entire/cli/review/env.go` — `ENTIRE_REVIEW_*` constants + `EncodeSkills`/`DecodeSkills` + `AppendReviewEnv`
- `cmd/entire/cli/agent/{claudecode,codex,geminicli}/reviewer.go` — per-agent `AgentReviewer` implementations (claude-code, codex, gemini-cli)
- `cmd/entire/cli/agent/claudecode/discovery.go` — skill discovery + `pickLatestVersion` plugin-cache dedupe
- `cmd/entire/cli/lifecycle.go` — `adoptReviewEnv` reads `ENTIRE_REVIEW_*` from process env; replaces marker-file adoption
- `cmd/entire/cli/review_bridge.go` / `review_helpers.go` — bridge code in `cli` package for cycle-bound functions (`headHasReviewCheckpoint`, `launchableReviewerFor`, `newReviewAttachCmd`)
- `cmd/entire/cli/checkpoint/checkpoint.go` — `Kind`, `ReviewSkills`, `ReviewPrompt` on `CommittedMetadata`; `HasReview` on `CheckpointSummary`
- `cmd/entire/cli/settings/settings.go` — `EntireSettings.Review` field
