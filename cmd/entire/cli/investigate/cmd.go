// Package investigate — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire
// investigate`. The command bootstraps shared findings/timeline docs and
// drives a round-robin multi-agent investigation loop via
// RunInvestigateLoop. The cobra wiring mirrors `entire review` (cmd.go in
// the review package): a Deps struct collects runtime-injectable hooks so
// tests can stub spawners, picker forms, and the loop runner without
// needing real TTY or agent binaries.
package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Deps collects the runtime-injectable hooks NewCommand needs from the
// parent cli package. Tests stub fields to drive branches that would
// otherwise require a real TTY or enabled repo.
type Deps struct {
	// GetAgentsWithHooksInstalled returns the registry names of all agents
	// whose lifecycle hooks are installed in the current repo.
	GetAgentsWithHooksInstalled func(ctx context.Context) []types.AgentName

	// NewSilentError wraps an error so the cobra root does not double-print
	// it.
	NewSilentError func(err error) error

	// SpawnerFor maps an agent name → Spawner (claude-code, codex,
	// gemini-cli). Returns nil for non-launchable agents. Wired by
	// investigate_bridge.go in a later task.
	SpawnerFor func(agentName string) spawn.Spawner

	// LaunchFix delegates to agentlaunch.LaunchFixAgent in production. Tests
	// inject a stub.
	LaunchFix func(ctx context.Context, agentName string, prompt string) error

	// LoopRun, when non-nil, replaces RunInvestigateLoop. Tests inject a
	// stub to capture LoopInput and return a canned LoopResult.
	LoopRun func(ctx context.Context, in LoopInput, ldeps LoopDeps) (LoopResult, error)

	// PromptYN is the interactive y/N prompt used by the settings migration
	// and the HEAD-soft-warn. Nil means "use the real huh-backed prompt".
	PromptYN func(ctx context.Context, question string, def bool) (bool, error)

	// HeadHasInvestigateCheckpoint returns (true, info) when the
	// checkpoint at HEAD already has HasInvestigation set. Used to
	// soft-warn against running a redundant investigation. Nil means
	// "skip the check entirely".
	HeadHasInvestigateCheckpoint func(ctx context.Context) (bool, string)

	// InvestigateMultipicker overrides the spawn-time agent picker. Nil
	// means "use the real PickInvestigateAgents form". Test-injectable so
	// tests can drive the picker without a TTY.
	InvestigateMultipicker func(ctx context.Context, choices []AgentChoice, askPrompt bool) (PickedInvestigate, error)
}

// runFlags collects the flag values the run path inspects. Captured into a
// struct so helpers don't need a giant signature.
type runFlags struct {
	issueLink string
	agentsCSV string
	maxTurns  int
	quorum    int
	cont      string
	edit      bool
	findings  bool
}

// NewCommand returns the `entire investigate` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	flags := runFlags{}

	cmd := &cobra.Command{
		Use:   "investigate [seed-doc]",
		Short: "Run a multi-agent investigation against the current branch",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run it normally.
		Hidden: true,
		Long: `Run a multi-agent investigation. Agents take turns appending findings,
evidence, and analysis to a shared findings document until quorum is reached.

Labs entry: investigate is experimental. We are actively refining it based on
user feedback.

Inputs (mutually exclusive):
  [seed-doc]              positional path to a starting findings file
  --issue-link <url>      GitHub issue or PR URL (resolved via gh)

When neither input is supplied and the spawn-time multi-agent picker fires,
the picker collects an "Investigation prompt" that becomes the topic for the
run.

Flags:
  --agents <csv>          override configured agents (comma-separated)
  --max-turns N           per-agent turn budget (default 2)
  --quorum N              approvals needed to terminate (0 = all agents)
  --continue <run-id>     resume an existing run
  --edit                  re-open the investigate config picker
  --findings              browse local investigation manifests

Subcommands:
  fix [run-id]            launch a coding agent with the run's findings as
                          grounded context
  show [run-id]           print a saved investigation's summary + findings
  clean [run-id|--all]    delete saved investigation artifacts`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one seed-doc path, received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := validateFlags(args, flags); err != nil {
				return err
			}
			return runInvestigate(ctx, cmd, args, flags, deps)
		},
	}

	cmd.Flags().StringVar(&flags.issueLink, "issue-link", "", "GitHub issue or PR URL")
	cmd.Flags().StringVar(&flags.agentsCSV, "agents", "", "override configured agents (comma-separated)")
	cmd.Flags().IntVar(&flags.maxTurns, "max-turns", 0, "per-agent turn budget (default 2)")
	cmd.Flags().IntVar(&flags.quorum, "quorum", 0, "approvals needed to terminate (0 = all agents)")
	cmd.Flags().StringVar(&flags.cont, "continue", "", "resume an existing run by id")
	cmd.Flags().BoolVar(&flags.edit, "edit", false, "re-open the investigate config picker")
	cmd.Flags().BoolVar(&flags.findings, "findings", false, "browse local investigation manifests")

	cmd.AddCommand(newFixSubcommand(deps))
	cmd.AddCommand(newShowSubcommand(deps))
	cmd.AddCommand(newCleanSubcommand(deps))
	return cmd
}

// validateFlags enforces the mutual-exclusion rules described in the long
// help text. Run before any I/O so usage errors are visible without
// touching disk.
func validateFlags(args []string, f runFlags) error {
	seedSet := len(args) == 1
	issueSet := strings.TrimSpace(f.issueLink) != ""
	contSet := strings.TrimSpace(f.cont) != ""

	inputCount := 0
	for _, set := range []bool{seedSet, issueSet} {
		if set {
			inputCount++
		}
	}
	if inputCount > 1 {
		return errors.New("at most one of [seed-doc], --issue-link may be set")
	}

	if contSet && inputCount > 0 {
		return errors.New("--continue is mutually exclusive with [seed-doc]/--issue-link")
	}

	modes := 0
	for _, m := range []bool{f.edit, f.findings} {
		if m {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("--edit and --findings are mutually exclusive")
	}
	if (f.edit || f.findings) && (inputCount > 0 || contSet) {
		return errors.New("--edit and --findings cannot be combined with a run input")
	}

	return nil
}

// newFixSubcommand wires `entire investigate fix [run-id]` to RunFix.
func newFixSubcommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "fix [run-id]",
		Short: "Launch a coding agent with a saved investigation as grounded context",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one run id, received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
				return wrapSilent(deps.NewSilentError, errors.New("not a git repository"))
			}
			store, err := NewLocalManifestStore(ctx)
			if err != nil {
				return fmt.Errorf("open manifest store: %w", err)
			}
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			}
			launch := deps.LaunchFix
			if launch == nil {
				return errors.New("fix: launch function not wired")
			}
			return RunFix(ctx, FixInput{
				RunID:  runID,
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
			}, FixDeps{
				ManifestStore: store,
				Launch:        launch,
			})
		},
	}
}

// newShowSubcommand wires `entire investigate show [run-id]` to RunShow.
func newShowSubcommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show [run-id]",
		Short: "Print a saved investigation's summary and findings",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one run id, received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
				return wrapSilent(deps.NewSilentError, errors.New("not a git repository"))
			}
			store, err := NewLocalManifestStore(ctx)
			if err != nil {
				return fmt.Errorf("open manifest store: %w", err)
			}
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			}
			return RunShow(ctx, ShowInput{
				RunID:  runID,
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
			}, ShowDeps{ManifestStore: store})
		},
	}
}

// newCleanSubcommand wires `entire investigate clean [run-id]` to RunClean.
func newCleanSubcommand(deps Deps) *cobra.Command {
	var (
		all   bool
		force bool
	)
	cmd := &cobra.Command{
		Use:   "clean [run-id]",
		Short: "Delete a saved investigation (or all)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one run id, received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
				return wrapSilent(deps.NewSilentError, errors.New("not a git repository"))
			}
			store, err := NewLocalManifestStore(ctx)
			if err != nil {
				return fmt.Errorf("open manifest store: %w", err)
			}
			stateStore, err := NewStateStore(ctx)
			if err != nil {
				return fmt.Errorf("open state store: %w", err)
			}
			runID := ""
			if len(args) == 1 {
				runID = args[0]
			}
			return RunClean(ctx, CleanInput{
				RunID:  runID,
				All:    all,
				Force:  force,
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
			}, CleanDeps{
				ManifestStore: store,
				RunDir:        stateStore.RunDir,
				ManifestPath:  store.PathFor,
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "delete every investigation")
	cmd.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	return cmd
}

// runInvestigate is the main run path. It pre-flights the repo, dispatches
// to --edit/--findings/--continue branches, then invokes the loop.
func runInvestigate(ctx context.Context, cmd *cobra.Command, args []string, f runFlags, deps Deps) error {
	silentErr := deps.NewSilentError

	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapSilent(silentErr, errors.New("not a git repository"))
	}

	// Initialize the file-backed logger so per-turn info/warn lines land in
	// .entire/logs/entire.log instead of stderr — which, during a TUI run,
	// would otherwise be interleaved with the dashboard frame and corrupt
	// the display. Failure is non-fatal; the fallback inside logging.log
	// will use slog.Default() and the user sees no worse than before.
	if err := logging.Init(ctx, ""); err == nil {
		defer logging.Close()
	}

	// Soft warn: HEAD already has an investigation. Skip for sub-modes
	// (edit / findings) and for non-interactive runs.
	if !f.edit && !f.findings && deps.HeadHasInvestigateCheckpoint != nil {
		has, info := deps.HeadHasInvestigateCheckpoint(ctx)
		if has {
			prompt := deps.PromptYN
			canPrompt := prompt != nil
			if prompt == nil {
				prompt = realPromptYN
				canPrompt = interactive.CanPromptInteractively()
			}
			if canPrompt {
				msg := fmt.Sprintf("HEAD already has an investigation (%s). Run another?", info)
				ok, promptErr := prompt(ctx, msg, true)
				if promptErr != nil {
					cmd.SilenceUsage = true
					fmt.Fprintln(cmd.ErrOrStderr(), "prompt cancelled")
					return wrapSilent(silentErr, promptErr)
				}
				if !ok {
					return nil
				}
			} else {
				logging.Info(ctx, "HEAD already has a recorded investigation; running anyway (non-interactive)",
					slog.String("info", info))
			}
		}
	}

	if f.edit {
		return runEdit(ctx, cmd, deps)
	}
	if f.findings {
		return runInvestigateFindings(ctx, cmd, silentErr)
	}
	if strings.TrimSpace(f.cont) != "" {
		return runContinue(ctx, cmd, f, deps)
	}
	return runFresh(ctx, cmd, args, f, deps)
}

// confirmUntrustedIssueSeed warns the operator that an --issue-link run
// feeds external (potentially attacker-controlled) GitHub content into
// agents that spawn with permission/sandbox bypass, and waits for an
// affirmative answer before continuing.
//
// Interactive: prompts y/N (default N). Returns (false, nil) on decline so
// the caller exits cleanly. Returns the prompt error wrapped on transport
// failure (Ctrl+C is treated as decline by uiform.PromptYN).
//
// Non-interactive: logs the warning to stderr and proceeds. CI / scripted
// callers passed --issue-link deliberately and need a way through; a hard
// block would break automation. The risk surfaces in operator-facing
// telemetry instead of being silently bypassed.
func confirmUntrustedIssueSeed(ctx context.Context, cmd *cobra.Command, deps Deps, issueLink string) (bool, error) {
	const warning = "Warning: --issue-link seeds the investigation with content fetched from " +
		"GitHub (issue body + comments). Agents in this run spawn with " +
		"permission/sandbox bypass and will read that content. A malicious " +
		"issue or comment can influence agent behaviour."
	prompt := deps.PromptYN
	canPrompt := prompt != nil
	if prompt == nil {
		prompt = realPromptYN
		canPrompt = interactive.IsTerminalWriter(cmd.OutOrStdout()) && interactive.CanPromptInteractively()
	}
	if !canPrompt {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"%s\nProceeding non-interactively (no TTY to prompt). Source: %s\n",
			warning, issueLink)
		return true, nil
	}
	fmt.Fprintln(cmd.ErrOrStderr(), warning)
	fmt.Fprintf(cmd.ErrOrStderr(), "Source: %s\n", issueLink)
	ok, err := prompt(ctx, "Continue with externally seeded investigation?", false)
	if err != nil {
		return false, fmt.Errorf("issue-link confirmation prompt: %w", err)
	}
	return ok, nil
}

// runEdit re-opens the config picker and persists the result.
func runEdit(ctx context.Context, cmd *cobra.Command, deps Deps) error {
	out := cmd.OutOrStdout()
	cfg, err := RunInvestigateConfigPicker(ctx, out, deps.SpawnerFor, deps.GetAgentsWithHooksInstalled)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(deps.NewSilentError, err)
	}
	if cfg == nil {
		return nil
	}
	return saveInvestigateConfig(ctx, cfg)
}

// saveInvestigateConfig persists cfg into .entire/settings.local.json
// (worktree-local, not committed). Other settings fields are preserved by
// reading the local file first, mutating, and writing it back. The
// committed .entire/settings.json is never touched.
func saveInvestigateConfig(ctx context.Context, cfg *settings.InvestigateConfig) error {
	localPath, err := paths.AbsPath(ctx, settings.EntireSettingsLocalFile)
	if err != nil {
		localPath = settings.EntireSettingsLocalFile
	}

	local := &settings.EntireSettings{}
	data, readErr := os.ReadFile(localPath) //nolint:gosec // path is from AbsPath
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read local settings: %w", readErr)
	}
	if len(data) > 0 {
		local, err = settings.LoadFromBytes(data)
		if err != nil {
			return fmt.Errorf("parse local settings: %w", err)
		}
	}

	local.Investigate = cfg
	if err := settings.SaveLocal(ctx, local); err != nil {
		return fmt.Errorf("save local settings: %w", err)
	}
	return nil
}

// runContinue resumes an existing run from persisted RunState.
func runContinue(ctx context.Context, cmd *cobra.Command, f runFlags, deps Deps) error {
	silentErr := deps.NewSilentError

	store, err := NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("open run state store: %w", err)
	}
	state, err := store.Load(ctx, f.cont)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}
	if state == nil {
		err := fmt.Errorf("no run state found for run id %q", f.cont)
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}

	agents := state.Agents
	if csv := strings.TrimSpace(f.agentsCSV); csv != "" {
		agents = parseAgentsCSV(csv)
	}
	if err := verifyAgentsLaunchable(ctx, agents, deps); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}

	// Resume reuses the agents the user originally selected — we do NOT
	// open the multipicker on --continue. The persisted state already
	// captures their intent (and re-prompting would force them to re-pick
	// every time they resume a paused run). Pass --agents to narrow on
	// resume; the multipicker is for fresh runs only.

	// state.NextAgentIdx is the index into agents the next turn will use.
	// If --agents shrinks the list (or the persisted state is otherwise
	// inconsistent), the loop would index out of range on the first turn.
	// Refuse rather than crash: the user gets an actionable error and the
	// state file is left intact for them to either fix the override or
	// `entire investigate --findings` and start fresh.
	if state.NextAgentIdx >= len(agents) {
		err := fmt.Errorf(
			"cannot resume: persisted next agent index %d exceeds available agents (%d). "+
				"This usually means --agents was used with a shorter list than the original run. "+
				"Either re-run with the original agents (or a superset), or remove the run state at "+
				".git/entire-investigations/%s/state.json and start a fresh investigation",
			state.NextAgentIdx, len(agents), state.RunID)
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}

	maxTurns := state.MaxTurns
	if f.maxTurns > 0 {
		maxTurns = f.maxTurns
	}
	quorum := state.Quorum
	if f.quorum > 0 {
		quorum = f.quorum
	}

	// AlwaysPrompt is not persisted in RunState — it's a settings-level
	// customization that the user controls outside the run. Load it fresh
	// on resume so a configured "be skeptical" preamble survives Ctrl+C.
	// If settings.Load fails (e.g. the file was hand-edited and is now
	// malformed), surface a warning so the user notices their preamble has
	// silently disappeared instead of letting the agent's behaviour change
	// mid-investigation with no explanation.
	alwaysPrompt := ""
	if s, sErr := settings.Load(ctx); sErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Warning: could not reload settings on --continue (%v). The configured "+
				"investigate.always_prompt is not being applied to this resumed run.\n", sErr)
	} else if s != nil && s.Investigate != nil {
		alwaysPrompt = s.Investigate.AlwaysPrompt
	}

	in := LoopInput{
		RunID:        state.RunID,
		Topic:        state.Topic,
		Agents:       agents,
		MaxTurns:     maxTurns,
		Quorum:       quorum,
		AlwaysPrompt: alwaysPrompt,
		FindingsDoc:  state.FindingsDoc,
		StartingSHA:  state.StartingSHA,
		Resume:       state,
	}
	if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
		fmt.Fprintf(cmd.OutOrStdout(), "Resuming investigation: %q (run %s)\n", state.Topic, state.RunID)
	}

	result, err := executeLoopAndCapture(ctx, cmd, in, deps)
	if err != nil {
		return err
	}

	// Rewrite the manifest with the new terminal outcome. Reusing
	// state.StartedAt keeps the filename stable (manifests are keyed
	// <stamp>-<runID>.json) so this overwrites the paused/cancelled
	// record in place rather than creating a duplicate. WorktreePath
	// isn't on RunState, so re-resolve — if it fails the manifest is
	// still written, just without the path.
	worktreeRoot, wtErr := paths.WorktreeRoot(ctx)
	if wtErr != nil {
		worktreeRoot = ""
	}
	writeRunManifest(ctx, cmd.OutOrStdout(), state.RunID, state.Topic, agents,
		state.StartingSHA, worktreeRoot, state.FindingsDoc,
		state.StartedAt, time.Now().UTC(), result)
	return nil
}

// runFresh handles the full first-run path: bootstrap docs, build initial
// state, dispatch to the loop, persist a manifest.
func runFresh(ctx context.Context, cmd *cobra.Command, args []string, f runFlags, deps Deps) error {
	silentErr := deps.NewSilentError

	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(), "Fix `.entire/settings.json` and re-run `entire investigate`.")
		return wrapSilent(silentErr, err)
	}
	if s == nil || s.Investigate.IsZero() {
		if !ConfirmFirstRunSetup(ctx, cmd.OutOrStdout()) {
			return nil
		}
		cfg, pickErr := RunInvestigateConfigPicker(ctx, cmd.OutOrStdout(), deps.SpawnerFor, deps.GetAgentsWithHooksInstalled)
		if pickErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
			return wrapSilent(silentErr, pickErr)
		}
		if cfg == nil {
			return nil
		}
		if saveErr := saveInvestigateConfig(ctx, cfg); saveErr != nil {
			return saveErr
		}
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.Investigate = cfg
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "Setup complete — running investigation now.")
	}

	agents, maxTurns, quorum, err := resolveRunConfig(s.Investigate, f)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}
	if err := verifyAgentsLaunchable(ctx, agents, deps); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}

	// hasSeedOrIssue is true when the user supplied a seed-doc or
	// --issue-link, in which case the picker (if it fires) skips the
	// "Investigation prompt" field — the topic comes from the seed/issue
	// directly.
	hasSeedOrIssue := len(args) == 1 || strings.TrimSpace(f.issueLink) != ""

	// Spawn-time multipicker: when 2+ agents configured AND --agents not
	// set, narrow the agent list and (when no seed/issue was supplied)
	// collect the investigation prompt that becomes the topic.
	pickerPrompt := ""
	if len(agents) >= 2 && strings.TrimSpace(f.agentsCSV) == "" {
		picker := deps.InvestigateMultipicker
		canRun := picker != nil
		if picker == nil {
			picker = PickInvestigateAgents
			canRun = interactive.CanPromptInteractively()
		}
		if canRun {
			choices := make([]AgentChoice, 0, len(agents))
			for _, name := range agents {
				choices = append(choices, AgentChoice{Name: name, Label: name})
			}
			picked, pickErr := picker(ctx, choices, !hasSeedOrIssue)
			if pickErr != nil {
				if errors.Is(pickErr, ErrInvestigatePickerCancelled) {
					return nil
				}
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
				return wrapSilent(silentErr, pickErr)
			}
			agents = picked.Names
			pickerPrompt = picked.Prompt
		}
	}

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	headSHA, err := currentHeadSHA(ctx, worktreeRoot)
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}

	runID, err := newRunID()
	if err != nil {
		return fmt.Errorf("generate run id: %w", err)
	}

	topic, seedDoc, issueSeed, issueTopic, err := resolveTopicAndSeed(ctx, args, f, pickerPrompt)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return wrapSilent(silentErr, err)
	}

	// Agents in this loop spawn with --permission-mode bypassPermissions
	// (claude-code) and --dangerously-bypass-approvals-and-sandbox (codex).
	// When the investigation is seeded from --issue-link, an attacker who
	// controls the linked GitHub issue body or comments can influence the
	// agent through content it reads. Make the operator confirm before
	// running with externally seeded input + unfettered agent permissions.
	if len(issueSeed) > 0 {
		ok, cErr := confirmUntrustedIssueSeed(ctx, cmd, deps, f.issueLink)
		if cErr != nil {
			return wrapSilent(silentErr, cErr)
		}
		if !ok {
			return nil
		}
	}

	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return fmt.Errorf("resolve git common dir: %w", err)
	}
	findingsDoc := resolveDocPaths(commonDir, runID)

	bres, err := Bootstrap(ctx, BootstrapInput{
		SeedDoc:        seedDoc,
		Topic:          topicForBootstrap(topic, seedDoc, issueSeed),
		IssueLinkSeed:  issueSeed,
		IssueLinkTopic: issueTopic,
		FindingsDoc:    findingsDoc,
	})
	if err != nil {
		return fmt.Errorf("bootstrap docs: %w", err)
	}
	if strings.TrimSpace(bres.Topic) != "" {
		topic = bres.Topic
	}

	// Skip the pre-TUI banner when the dashboard will render its own title
	// row — those two lines were echoing the TUI header and leaving stale
	// rows above the live dashboard. In non-TTY mode the text sink shows
	// nothing similar, so the banner remains useful there.
	if !interactive.IsTerminalWriter(cmd.OutOrStdout()) || !interactive.CanPromptInteractively() {
		fmt.Fprintf(cmd.OutOrStdout(), "Investigating: %q (run %s)\n", topic, runID)
		fmt.Fprintf(cmd.OutOrStdout(), "  Findings: %s\n", findingsDoc)
	}

	startedAt := time.Now().UTC()
	in := LoopInput{
		RunID:        runID,
		Topic:        topic,
		Agents:       agents,
		MaxTurns:     maxTurns,
		Quorum:       quorum,
		AlwaysPrompt: strings.TrimSpace(s.Investigate.AlwaysPrompt),
		FindingsDoc:  findingsDoc,
		StartingSHA:  headSHA,
	}
	result, err := executeLoopAndCapture(ctx, cmd, in, deps)
	if err != nil {
		return err
	}

	endedAt := time.Now().UTC()
	writeRunManifest(ctx, cmd.OutOrStdout(), runID, topic, agents, headSHA, worktreeRoot,
		findingsDoc, startedAt, endedAt, result)
	return nil
}

// resolveRunConfig derives the effective agents / max-turns / quorum from
// settings, with --agents / --max-turns / --quorum overrides taking
// precedence.
func resolveRunConfig(cfg *settings.InvestigateConfig, f runFlags) (agents []string, maxTurns int, quorum int, err error) {
	if cfg == nil {
		return nil, 0, 0, errors.New("no investigate config; run `entire investigate --edit` first")
	}
	agents = append([]string(nil), cfg.Agents...)
	if csv := strings.TrimSpace(f.agentsCSV); csv != "" {
		agents = parseAgentsCSV(csv)
	}
	if len(agents) == 0 {
		return nil, 0, 0, errors.New("no agents configured for investigate; run `entire investigate --edit`")
	}
	maxTurns = cfg.MaxTurns
	if f.maxTurns > 0 {
		maxTurns = f.maxTurns
	}
	quorum = cfg.Quorum
	if f.quorum > 0 {
		quorum = f.quorum
	}
	return agents, maxTurns, quorum, nil
}

// parseAgentsCSV splits a comma-separated agent list, trimming whitespace
// and dropping empty entries.
func parseAgentsCSV(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// verifyAgentsLaunchable confirms each agent has a non-nil Spawner AND has
// hooks installed in the current repo.
func verifyAgentsLaunchable(ctx context.Context, agents []string, deps Deps) error {
	if deps.SpawnerFor == nil {
		return errors.New("investigate: SpawnerFor not wired")
	}
	if deps.GetAgentsWithHooksInstalled == nil {
		return errors.New("investigate: GetAgentsWithHooksInstalled not wired")
	}
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	installedSet := make(map[string]struct{}, len(installed))
	for _, n := range installed {
		installedSet[string(n)] = struct{}{}
	}
	for _, name := range agents {
		if deps.SpawnerFor(name) == nil {
			return fmt.Errorf("agent %q is not launchable (spawner missing)", name)
		}
		if _, ok := installedSet[name]; !ok {
			return fmt.Errorf("agent %q is not launchable (run `entire configure --agent %s` first)", name, name)
		}
	}
	return nil
}

// resolveTopicAndSeed turns the user's input args into a topic + (seed
// doc path | issue link seed bytes + topic). pickerPrompt is the
// "Investigation prompt" collected from the spawn-time multipicker; it
// becomes the topic only when no seed-doc / --issue-link was supplied.
// Exactly one of seedDoc / issueSeed / topic-only is set on return.
func resolveTopicAndSeed(ctx context.Context, args []string, f runFlags, pickerPrompt string) (topic, seedDoc string, issueSeed []byte, issueTopic string, err error) {
	switch {
	case len(args) == 1:
		seedDoc = args[0]
		body, readErr := os.ReadFile(seedDoc) //nolint:gosec // path is user-supplied positional arg
		if readErr != nil {
			return "", "", nil, "", fmt.Errorf("read seed doc %s: %w", seedDoc, readErr)
		}
		topic = DeriveTopicFromSeed(body, seedDoc)
		return topic, seedDoc, nil, "", nil
	case strings.TrimSpace(f.issueLink) != "":
		res, resErr := ResolveIssueLink(ctx, f.issueLink)
		if resErr != nil {
			return "", "", nil, "", resErr
		}
		return res.Topic, "", res.SeedDoc, res.Topic, nil
	case strings.TrimSpace(pickerPrompt) != "":
		topic = strings.TrimSpace(pickerPrompt)
		return topic, "", nil, "", nil
	default:
		return "", "", nil, "", errors.New("missing investigation input: pass [seed-doc] or --issue-link, or enter an investigation prompt in the picker")
	}
}

// topicForBootstrap returns the topic value to embed in the bootstrap
// scaffold. The seed-doc path takes precedence (Bootstrap re-derives from
// the seed body), and the issue-link path uses IssueLinkTopic; only the
// topic-only path puts the resolved topic into BootstrapInput.Topic.
func topicForBootstrap(topic, seedDoc string, issueSeed []byte) string {
	if seedDoc != "" || len(issueSeed) > 0 {
		return ""
	}
	return topic
}

// resolveDocPaths returns the absolute findings path for a run. The
// findings doc lives alongside state.json in the per-run directory under
// the git common dir:
//
//	<commonDir>/entire-investigations/<run-id>/findings.md
//	<commonDir>/entire-investigations/<run-id>/state.json
//
// Putting the per-run artefacts under the git common dir (rather than the
// worktree's .entire/investigations/) keeps the worktree's working tree
// clean — investigation findings are session-scoped scratch space, not
// part of the user's source tree.
func resolveDocPaths(commonDir, runID string) string {
	return filepath.Join(commonDir, InvestigationsDirName, runID, "findings.md")
}

// executeLoopAndCapture runs the loop and returns the LoopResult so the
// caller can use it to compose a post-run manifest / footer.
func executeLoopAndCapture(ctx context.Context, cmd *cobra.Command, in LoopInput, deps Deps) (LoopResult, error) {
	stateStore, err := NewStateStore(ctx)
	if err != nil {
		return LoopResult{}, fmt.Errorf("open run state store: %w", err)
	}

	out := cmd.OutOrStdout()
	progress, tuiSink, runCtx, cancelTUI := buildProgressSink(ctx, in, out)
	if cancelTUI != nil {
		defer cancelTUI()
	}
	if tuiSink != nil {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	ldeps := LoopDeps{
		SpawnerFor: deps.SpawnerFor,
		States:     stateStore,
		Progress:   progress,
	}

	runner := deps.LoopRun
	if runner == nil {
		runner = RunInvestigateLoop
	}
	result, runErr := runner(runCtx, in, ldeps)
	if runErr != nil {
		return result, fmt.Errorf("investigate loop: %w", runErr)
	}
	return result, nil
}

// buildProgressSink chooses between the Bubble Tea TUI and the plain-text
// fallback based on terminal capability. In TTY mode we wrap ctx in a
// cancellable child so the in-TUI Ctrl+C handler can stop the run via the
// same cancel function the cobra root would use on SIGINT. In non-TTY mode
// the caller's ctx is returned unchanged and cancelTUI is nil.
//

func buildProgressSink(ctx context.Context, in LoopInput, out io.Writer) (ProgressSink, *tuiProgressSink, context.Context, context.CancelFunc) {
	if !interactive.IsTerminalWriter(out) || !interactive.CanPromptInteractively() {
		return newTextProgressSink(out), nil, ctx, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	maxTurns := in.MaxTurns
	if maxTurns == 0 {
		maxTurns = defaultMaxTurns
	}
	quorum := in.Quorum
	if quorum == 0 {
		quorum = len(in.Agents)
	}
	sink := newTUIProgressSink(in.Topic, in.RunID, in.Agents, maxTurns, quorum, cancel, out)
	return sink, sink, runCtx, cancel
}

// writeRunManifest builds a LocalManifest from the loop result and
// persists it. Failures are logged but do not error — the docs themselves
// are the deliverable.
//
// On terminal outcomes (Quorum/Stalled) the manifest captures the final
// findings.md content into FindingsContent and the per-run directory is
// removed — the manifest becomes the durable record of the run. On
// Paused/Cancelled the per-run directory is left in place so `--continue`
// can pick up where the run left off.
func writeRunManifest(
	ctx context.Context,
	out io.Writer,
	runID, topic string,
	agents []string,
	startingSHA, worktreePath, findingsDoc string,
	startedAt, endedAt time.Time,
	result LoopResult,
) {
	manifestStore, err := NewLocalManifestStore(ctx)
	if err != nil {
		logging.Debug(ctx, "investigate: open manifest store",
			slog.String("err", err.Error()), slog.String("run_id", runID))
		return
	}
	stancesByAgent := map[string]string{}
	if result.State != nil {
		for _, s := range result.State.Stances {
			stancesByAgent[s.Agent] = s.Stance
		}
	}
	if startedAt.IsZero() && result.State != nil {
		startedAt = result.State.StartedAt
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}

	// Capture findings into the manifest on terminal outcomes so the
	// content survives even after we delete the per-run dir. Failure to
	// read is logged but non-fatal — the manifest still records that
	// the run happened, just without the findings body. We intentionally
	// do NOT clean up the per-run dir if the read fails: leaving the
	// file behind gives the user a chance to recover it manually.
	terminal := result.Outcome == OutcomeQuorum || result.Outcome == OutcomeStalled
	findingsContent := ""
	captured := false
	if terminal && findingsDoc != "" {
		data, readErr := os.ReadFile(findingsDoc) //nolint:gosec // path computed from runID + git common dir
		if readErr != nil {
			logging.Debug(ctx, "investigate: read findings for manifest capture",
				slog.String("err", readErr.Error()), slog.String("run_id", runID))
		} else {
			findingsContent = string(data)
			captured = true
		}
	}

	m := LocalManifest{
		RunID:           runID,
		Topic:           topic,
		Slug:            SlugifyTopic(topic),
		StartingSHA:     startingSHA,
		WorktreePath:    worktreePath,
		FindingsDoc:     findingsDoc,
		FindingsContent: findingsContent,
		Agents:          append([]string(nil), agents...),
		Outcome:         string(result.Outcome),
		StancesByAgent:  stancesByAgent,
		StartedAt:       startedAt,
		EndedAt:         endedAt,
	}
	if writeErr := manifestStore.Write(ctx, m); writeErr != nil {
		logging.Debug(ctx, "investigate: manifest write failed",
			slog.String("err", writeErr.Error()), slog.String("run_id", runID))
		return
	}

	// Clean up the per-run dir only AFTER the manifest write succeeds
	// and only when we successfully captured the findings body. This
	// keeps the failure modes safe: a manifest write failure leaves the
	// per-run dir intact (for retry/inspection), and a read failure
	// leaves the file on disk so the user can recover it.
	if terminal && captured && findingsDoc != "" {
		runDir := filepath.Dir(findingsDoc)
		if rmErr := os.RemoveAll(runDir); rmErr != nil {
			logging.Debug(ctx, "investigate: cleanup per-run dir",
				slog.String("err", rmErr.Error()), slog.String("run_id", runID))
		}
	}

	writeInvestigateFooter(out, m)
}

// writeInvestigateFooter prints the post-run summary, the findings
// content, and how to run `entire investigate fix`. The findings
// content comes from the manifest's embedded FindingsContent on
// terminal outcomes (Quorum/Stalled — the per-run dir is gone); on
// paused/cancelled outcomes the per-run dir still has findings.md and
// we read it from there.
func writeInvestigateFooter(w io.Writer, m LocalManifest) {
	fmt.Fprintln(w)
	if m.Outcome != "" {
		fmt.Fprintf(w, "Outcome: %s\n", m.Outcome)
	}
	fmt.Fprintln(w, "Investigation complete.")
	fmt.Fprintln(w)

	body := findingsContentFor(m)
	if body != "" {
		rendered, renderErr := mdrender.RenderForWriter(w, body)
		if renderErr != nil {
			// Fall back to raw markdown so the user still sees the
			// content even when glamour fails (malformed style config,
			// unexpected runtime).
			rendered = body
		}
		fmt.Fprint(w, rendered)
		if !strings.HasSuffix(rendered, "\n") {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "To apply these findings:")
	fmt.Fprintf(w, "  entire investigate fix %s\n", m.RunID)
}

// findingsContentFor returns the findings body to render in the footer.
// Prefers the manifest's embedded content (set on terminal outcomes
// when the per-run dir has been cleaned); falls back to reading the
// on-disk findings.md for paused/cancelled outcomes. Errors and
// missing files both yield "" — the caller prints a shorter footer.
func findingsContentFor(m LocalManifest) string {
	if m.FindingsContent != "" {
		return m.FindingsContent
	}
	if m.FindingsDoc == "" {
		return ""
	}
	data, err := os.ReadFile(m.FindingsDoc)
	if err != nil {
		return ""
	}
	return string(data)
}

// newRunID returns a fresh 12-hex-char run identifier, sharing the
// checkpoint-id format used by the strategy package.
func newRunID() (string, error) {
	cid, err := id.Generate()
	if err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	return cid.String(), nil
}

// currentHeadSHA returns the current HEAD commit hash as a 40-char hex
// string. Thin wrapper around gitexec.HeadSHA preserved so existing call
// sites don't change.
func currentHeadSHA(ctx context.Context, repoRoot string) (string, error) {
	return gitexec.HeadSHA(ctx, repoRoot) //nolint:wrapcheck // gitexec already wraps
}

// wrapSilent applies the silent-error wrapper if it is non-nil. Mirrors
// review's pattern so tests can inject a passthrough.
func wrapSilent(fn func(error) error, err error) error {
	if fn == nil {
		return err
	}
	return fn(err)
}
