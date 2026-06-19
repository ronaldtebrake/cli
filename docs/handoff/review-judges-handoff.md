# Handoff — `entire review` + Judge Panel

_Last updated: 2026-06-14 · branch `review-profiles` @ `ead8c12dc`_

## TL;DR

`entire review` is the evolved multi-agent code-review command. A profile runs
a crew of **reviewers** (parallel review
agents) and then a panel of **judges** that render the verdict; with ≥2 judges a
**chair** merges them. The command, profiles, guided setup, scripted config,
`--list`, and the judge panel are implemented, building, and green. The Pi-specific
work lives in a separate stacked PR (#1313) that now needs another rebase.

- Build: `go build ./...` ✓
- Tests: `go test ./...` → **72 ok, 0 fail** ✓
- Lint: `golangci-lint` → 0 issues on touched packages ✓
- `review-profiles` is pushed; local == `origin/review-profiles` == `ead8c12dc`.

## Terminology (final)

- **reviewers** — the worker agents that review the change in parallel
  (`agents` in settings).
- **judges** — the panel that evaluates reviewer reports (`judges` in settings).
- **chair** — the judge that merges a ≥2-judge panel into the final verdict
  (`chair` in settings; defaults to the first judge).
- Command is **`entire review`**; **`entire review`** is a kept alias (it shipped
  on `main`). **`entire scout`** was removed (never shipped).
- Internal identifiers, settings keys, and env vars are still `review_*` /
  `ENTIRE_REVIEW_*` — only user-facing surfaces use reviewer/judge/chair.

## Command surface

```
entire review                # interactive: profile chooser. non-interactive: list + error (never silent default)
entire review <profile>      # run a named profile
entire review --list         # list profiles (reviewers + judges, default marked)
entire review --configure    # interactive wizard; non-interactive discovery view
entire review --configure --profile P \
  --set-agents claude-code,codex \      # reviewers (simple)
  --set-slot claude-code=opus --set-slot codex \   # reviewer slots (dupes ok)
  --set-judge claude-code=opus --set-judge codex=gpt-5 \  # judges (repeatable; >1 = panel)
  --set-chair claude-code=opus \        # chair for a panel
  --set-model codex=gpt-5-codex --set-task "..."
entire review --edit         # advanced skill picker
entire review --agent N      # run one reviewer
entire review --agent N --model M
entire review --agents       # list reviewers (valid --agent values)
entire review --models [--agent N]
entire review --prompt "..." # one-off instructions
entire review --findings     # browse local findings
entire attach --review <id>   # post-hoc tag a session (the old `review attach` was removed)
```

## Settings schema (`review_profiles`)

```json
{
  "review_default_profile": "general",
  "review_profiles": {
    "general": {
      "task": "Review this change for correctness, regressions, tests, and maintainability.",
      "agents": { "claude-code": {"skills": ["/review"]}, "codex": {"skills": ["/review"]} },
      "judges": [{"agent": "claude-code", "model": "opus"}]
    },
    "security": {
      "task": "...",
      "agents": { "claude-sonnet": {"agent": "claude-code", "model": "sonnet"}, "codex": {"model": "gpt-5-codex"} },
      "judges": [{"agent": "claude-code", "model": "opus"}, {"agent": "codex", "model": "gpt-5"}],
      "chair": "claude-code:opus"
    }
  }
}
```

Back-compat: legacy `master` (an reviewer id) and `master_agent` / `master_model`
are still honored as a single judge when `judges` is empty. New configs write
`judges`/`chair`.

## How the judge panel works

- `profileJudges(profile)` resolves the panel `[]judgeSpec` + chair index:
  explicit `judges` → legacy `master_agent` → legacy worker `master`.
- `PanelSynthesisProvider` (`synthesis_panel.go`) implements `SynthesisProvider`,
  so `SynthesisSink` consumes it unchanged:
  - fans out to each judge in parallel over the same synthesis prompt,
  - one surviving verdict → passthrough,
  - ≥2 → chair merges via `composeChairPrompt`, individual verdicts appended as a
    `## Panel` section,
  - failed/empty judges dropped; all-fail surfaces "final report unavailable".
- **The chair runs twice by design**: once as a panel judge (its own independent
  verdict) and once to merge the panel. This is intentional and commented in the
  code.
- `runMultiAgentPath` builds an `AgentSynthesisProvider` for a single judge or a
  `PanelSynthesisProvider` for a panel. Validation requires only that ≥1 judge
  resolves; text-gen failures degrade gracefully at synthesis time.

## Done

- [x] Command is `review` (`inspect`/`scout` names removed).
- [x] Bare `review` requires explicit selection (interactive chooser / non-interactive error+list).
- [x] `--list` profiles (reviewers + judges, chair marked).
- [x] Custom focus/task option in guided setup; guided setup edits the existing profile.
- [x] Slot-based crew (`--set-slot`, duplicates allowed).
- [x] Judge panel: schema, `profileJudges`, `PanelSynthesisProvider`, chair merge, tests.
- [x] Scripted `--set-judge` / `--set-chair` (replaced `--set-master`).
- [x] Guided picker: `pickSlotList` for reviewers + judges, chair pick for panels.
- [x] Reviewer/judge/chair terminology across help, `--list`, catalog, errors.
- [x] Dropped legacy `[master]` marker in `--agents`; `judges=` in catalog.
- [x] Removed fabricated codex/gemini model lists (only claude-code advertises models).
- [x] Codex JSON error envelopes surfaced instead of bare `exit status 1`.
- [x] Pi-specific reviewer/model files kept out of `review-profiles` (live on #1313).
- [x] Merged latest `origin/main` (incl. attribution/trail work); clean.
- [x] Docs refreshed: `docs/architecture/review-command.md` + `CLAUDE.md` summary.
- [x] Build/tests/lint/gofmt all green; branch pushed.

## Pending / next steps

1. **Rebase PR #1313 (`review-pi-reviewer`) onto `origin/review-profiles` (`ead8c12dc`).**
   It is behind again after the judge-panel + merge + doc commits.
   - PR: https://github.com/entireio/cli/pull/1313 (base `review-profiles`, head `review-pi-reviewer`)
   - Contains: Pi review-runner adapter, Pi live model list (`pi --list-models`),
     Pi generate/text-gen. The Pi adapter is the obvious first **panel-capable
     text-gen judge** to validate the panel end-to-end with a real second judge.
2. Consider validating scripted `--set-judge` agents at config time (currently
   only validated at runtime, where failures are dropped). Intentional for now;
   revisit if users hit silent typos.
3. Optional: include the profile task / scope context in `composeChairPrompt`
   (today the chair reconciles verdicts only).
4. Optional: bound judge-panel concurrency if panels ever grow large (currently
   unbounded; fine for 2–3 judges).

## Key files

- `cmd/entire/cli/review/cmd.go` — `NewCommand`, dispatch, `--list`, catalog,
  `composeMultiAgentSinks`, judge-panel wiring (`runMultiAgentPath`).
- `cmd/entire/cli/review/picker.go` — guided setup, focus picker, `pickSlotList`
  (reviewers + judges), chair picker, profile chooser.
- `cmd/entire/cli/review/profile.go` — profile resolution, `profileJudges`,
  default tasks.
- `cmd/entire/cli/review/synthesis_panel.go` (+ `_test.go`) — `PanelSynthesisProvider`,
  `composeChairPrompt`.
- `cmd/entire/cli/review/synthesis_sink.go` / `synthesis_prompt.go` — verdict sink.
- `cmd/entire/cli/review/marker_fallback.go` — manual fallback for non-adapter agents.
- `cmd/entire/cli/review/env.go` — `ENTIRE_REVIEW_*` constants + skills codec.
- `cmd/entire/cli/agent/model_lister.go` — `ModelLister` capability.
- `cmd/entire/cli/agent/claudecode/models.go` — only real `ModelLister` on this branch.
- `cmd/entire/cli/settings/settings.go` — `ReviewProfileConfig` (`Agents`, `Judges`,
  `Chair`, legacy `Master`/`MasterAgent`/`MasterModel`).
- `cmd/entire/cli/attach.go` — `entire attach --review` (consumes pending marker).
- `docs/architecture/review-command.md` — full architecture reference (current).

## Verify

```
go build ./...
go test ./...                 # expect 72 ok, 0 fail
golangci-lint run ./cmd/entire/cli/review/... ./cmd/entire/cli/ ./cmd/entire/cli/settings/...
go run ./cmd/entire review --list
go run ./cmd/entire review --help
```

## Gotchas

- Entire's pre-push hook also pushes `entire/checkpoints/v1` to a **checkpoint
  remote**; if that errors (`signal: killed` / unreachable), it does **not** mean
  the code branch failed to push — verify with `git ls-remote origin review-profiles`.
- `[entire-dev] project isn't compiling; falling back to the entire binary on
  PATH` during git ops is expected noise from the dev hook, not an error.
