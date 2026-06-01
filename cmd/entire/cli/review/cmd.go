// Package review — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire review`.
// It routes through the new AgentReviewer / Sink / Run architecture for
// launchable agents (claude-code, codex, gemini) and falls back to
// RunMarkerFallback for non-launchable agents (cursor, opencode,
// factoryai-droid, copilot-cli).
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Deps collects the runtime-injectable hooks NewCommand needs from the
// parent cli package. Tests stub fields to drive branches that would
// otherwise require a real TTY or enabled repo. Production wiring is
// provided by buildReviewDeps in cmd/entire/cli/review_bridge.go and
// passed to NewCommand from root.go.
type Deps struct {
	// GetAgentsWithHooksInstalled returns the registry names of all agents
	// whose lifecycle hooks are installed in the current repo.
	GetAgentsWithHooksInstalled func(ctx context.Context) []types.AgentName

	// NewSilentError wraps an error so the cobra root does not double-print it.
	NewSilentError func(err error) error

	// HeadHasReviewCheckpoint checks whether HEAD's checkpoint metadata
	// includes a review session. Returns (true, infoString) if HasReview is set.
	// Injected to avoid an import cycle: review → checkpoint → codex → review.
	HeadHasReviewCheckpoint func(ctx context.Context) (bool, string)

	// ReviewCheckpointContext returns best-effort checkpoint context for the
	// branch review scope. Injected from the cli package because checkpoint
	// readers cannot be imported here without cycling through agent reviewers.
	ReviewCheckpointContext func(ctx context.Context, worktreeRoot string, scopeBaseRef string) string

	// ReviewerFor maps an agent registry name to its AgentReviewer
	// implementation. Returns nil for non-launchable agents (cursor, opencode,
	// factoryai-droid, copilot-cli). Injected to break the import cycle:
	// per-agent reviewer packages import review (for ComposeReviewPrompt /
	// AppendReviewEnv), so review/cmd.go cannot import them back.
	ReviewerFor func(agentName string) reviewtypes.AgentReviewer

	// AttachCmd, when non-nil, is registered as the `review attach`
	// subcommand. Callers in the cli package pass newReviewAttachCmd() here;
	// tests pass nil to skip the subcommand.
	AttachCmd *cobra.Command
}

// NewCommand returns the `entire review` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	var edit bool
	var agentOverride string
	var baseOverride string
	var profileOverride string
	var perRunPrompt string
	var findings bool
	var fix bool
	var all bool

	cmd := &cobra.Command{
		Use: "review",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run `entire review` / `entire
		// review --help` and the command works normally.
		Hidden: true,
		Short:  "Run a review profile against the current branch",
		Long: `Run a named review profile against the current branch. Review
profiles are loaded from Entire settings and clone-local preferences. On
first run, an interactive picker writes clone-local preferences.

Labs entry: review is experimental. We are actively refining it based on user
feedback.

The review session is recorded as part of the next checkpoint, so the
review metadata is permanently attached to the commit it covers.

Flags:
  --edit         re-open the review profile config picker
  --findings     browse local review findings
  --fix          apply review findings in a normal agent session
  --all          with --fix, apply all sources/findings without selectors
  --agent NAME   run only one worker from the selected profile, or select the
                 fix agent with --fix
  --profile NAME select a review profile (also accepted as positional arg)
  --prompt TEXT  add one-off per-run instructions for this invocation
  --base REF     scope the review against REF instead of mainline. Useful
                 for stacked PRs where the review base is the parent feature
                 branch, not main. Default: first existing of origin/HEAD,
                 origin/main, origin/master, main, master.

Subcommands:
  attach <id>    tag an existing session as a review (equivalent to
                 'entire attach --review <id>')`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("accepts at most one argument, received %d", len(args))
			}
			if len(args) == 1 && !fix && profileOverride != "" {
				return errors.New("pass profile either positionally or with --profile, not both")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Discover external agents so review configs that target them
			// resolve correctly — without this, GetAgentsWithHooksInstalled
			// and agent.Get can't see them.
			external.DiscoverAndRegister(ctx)

			if all && !fix {
				return errors.New("--all requires --fix")
			}
			modes := 0
			for _, enabled := range []bool{edit, findings, fix} {
				if enabled {
					modes++
				}
			}
			if modes > 1 {
				return errors.New("--edit, --findings, and --fix are mutually exclusive")
			}
			profileName := profileOverride
			if len(args) == 1 && !fix {
				profileName = args[0]
			}
			if edit {
				_, err := RunReviewProfileConfigPicker(ctx, cmd.OutOrStdout(), deps.GetAgentsWithHooksInstalled, profileName)
				return err
			}
			if findings {
				return runReviewFindings(ctx, cmd, deps.NewSilentError)
			}
			if fix {
				target := ""
				if len(args) == 1 {
					target = args[0]
				}
				return runReviewFix(ctx, cmd, target, all, agentOverride, deps.NewSilentError)
			}
			return runReview(ctx, cmd, agentOverride, baseOverride, profileName, perRunPrompt, deps)
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "re-open the review profile config picker")
	cmd.Flags().BoolVar(&findings, "findings", false, "browse local review findings")
	cmd.Flags().BoolVar(&fix, "fix", false, "apply review findings in a normal agent session")
	cmd.Flags().BoolVar(&all, "all", false, "with --fix, apply all sources/findings without selectors")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "run one configured worker from the selected profile; with --fix, select the fix agent")
	cmd.Flags().StringVar(&profileOverride, "profile", "", "review profile to run (default: review_default_profile or general)")
	cmd.Flags().StringVar(&perRunPrompt, "prompt", "", "one-off instructions appended to this review run")
	cmd.Flags().StringVar(&baseOverride, "base", "", "git ref to scope the review against (default: origin/HEAD → origin/main → origin/master → main → master)")
	if deps.AttachCmd != nil {
		cmd.AddCommand(deps.AttachCmd)
	}
	return cmd
}

// runReview executes the main review flow.
func runReview(ctx context.Context, cmd *cobra.Command, agentOverride, baseOverride, profileOverride, perRunPrompt string, deps Deps) error {
	out := cmd.OutOrStdout()
	silentErr := deps.NewSilentError

	// 1. Pre-flight: must be in a git repo.
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}

	// 2. Load config. A load error means the settings file exists but is
	// malformed (Load returns a default-filled object when the file is
	// missing). Surface the error instead of silently opening the picker,
	// which would cause the config writer to write over the user's other
	// settings with an empty EntireSettings{}.
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(),
			"Fix your Entire settings or clone-local review preferences and re-run `entire review`.")
		return silentErr(err)
	}
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	if s == nil {
		s = &settings.EntireSettings{}
	}
	if len(s.ReviewProfiles) == 0 {
		profileForSetup := profileOverride
		if profileForSetup == "" {
			profileForSetup = DefaultProfileName
		}
		profile, defaultErr := defaultReviewProfileForInstalledAgents(ctx, profileForSetup, installed, deps.ReviewerFor)
		if defaultErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), defaultErr.Error())
			return silentErr(defaultErr)
		}
		if saveErr := saveDefaultReviewProfile(ctx, profileForSetup, profile); saveErr != nil {
			return saveErr
		}
		s.ReviewProfiles = map[string]settings.ReviewProfileConfig{profileForSetup: profile}
		s.ReviewDefaultProfile = profileForSetup
		fmt.Fprintf(out, "No review profiles found — using default %q profile with %s.\n", profileForSetup, strings.Join(sortedProfileAgentNames(profile), ", "))
		fmt.Fprintln(out, "Edit later with `entire review --edit`.")
		fmt.Fprintln(out)
	}

	profileName, profile, err := selectReviewProfile(s, profileOverride)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}
	profile.Task = profileTask(profileName, profile)
	profile.Agents = nonZeroAgentConfigs(profile.Agents)

	if agentOverride != "" {
		cfg, ok := profile.Agents[agentOverride]
		if !ok || cfg.IsZero() {
			cmd.SilenceUsage = true
			err := fmt.Errorf("agent %q is not configured in review profile %q", agentOverride, profileName)
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		return runSingleAgentPath(ctx, cmd, profileName, agentOverride, baseOverride, perRunPrompt, profile.Task, cfg, installed, deps, out)
	}

	if missing := missingInstalledProfileAgents(profile.Agents, installed); len(missing) > 0 {
		cmd.SilenceUsage = true
		err := fmt.Errorf("Hooks are not installed for review profile %q agent(s): %s. Run `entire configure --agent <name>` first, or edit the profile", profileName, strings.Join(missing, ", "))
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	eligible := ComputeEligibleConfiguredForProfile(profile, installed)
	switch len(eligible) {
	case 0:
		cmd.SilenceUsage = true
		err := fmt.Errorf("review profile %q has no eligible agents", profileName)
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	case 1:
		cfg := profile.Agents[eligible[0].Name]
		return runSingleAgentPath(ctx, cmd, profileName, eligible[0].Name, baseOverride, perRunPrompt, profile.Task, cfg, installed, deps, out)
	default:
		launchableEligible := computeLaunchableEligibleForProfile(profile, installed, deps.ReviewerFor)
		if len(launchableEligible) != len(eligible) {
			nonLaunchable := nonLaunchableEligibleNames(eligible, deps.ReviewerFor)
			cmd.SilenceUsage = true
			err := fmt.Errorf("review profile %q includes non-launchable agent(s) in a fan-out run: %s. Use --agent for a single manual fallback, or remove them from the profile", profileName, strings.Join(nonLaunchable, ", "))
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		if strings.TrimSpace(profile.Master) == "" {
			cmd.SilenceUsage = true
			err := fmt.Errorf("review profile %q has multiple workers but no master; set review_profiles.%s.master", profileName, profileName)
			fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
			return silentErr(err)
		}
		return runMultiAgentPath(ctx, cmd, profileName, profile, launchableEligible, baseOverride, perRunPrompt, deps, out)
	}
}

func missingInstalledProfileAgents(configured map[string]settings.ReviewConfig, installed []types.AgentName) []string {
	installedSet := make(map[string]struct{}, len(installed))
	for _, name := range installed {
		installedSet[string(name)] = struct{}{}
	}
	var missing []string
	for name, cfg := range configured {
		if cfg.IsZero() {
			continue
		}
		if _, ok := installedSet[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func nonLaunchableEligibleNames(eligible []AgentChoice, reviewerFor func(string) reviewtypes.AgentReviewer) []string {
	var out []string
	for _, c := range eligible {
		if reviewerFor(c.Name) == nil {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// runSingleAgentPath completes a single-agent review: verifies hooks + skills,
// guards against re-review, resolves scope, then dispatches via Run or
// RunMarkerFallback.
func runSingleAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	profileName, agentName, baseOverride, perRunPrompt, task string,
	cfg settings.ReviewConfig,
	installed []types.AgentName,
	deps Deps,
	out io.Writer,
) error {
	silentErr := deps.NewSilentError

	// 3.5. Verify hooks are installed for the selected agent.
	found := false
	for _, n := range installed {
		if string(n) == agentName {
			found = true
			break
		}
	}
	if !found {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Hooks are not installed for %q. Run `entire configure --agent %s` first, "+
				"or remove %q from review settings.\n",
			agentName, agentName, agentName)
		return silentErr(fmt.Errorf("hooks not installed for %s", agentName))
	}

	// 3.6. Verify configured skills are actually installed on disk.
	ag, agErr := agent.Get(types.AgentName(agentName))
	if agErr != nil {
		return fmt.Errorf("resolve agent %s: %w", agentName, agErr)
	}
	if err := VerifyConfiguredSkillsInstalled(ctx, ag, cfg); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	// 4. Re-run guard: check if HEAD's checkpoint already has a review.
	if reviewed, meta := deps.HeadHasReviewCheckpoint(ctx); reviewed {
		var proceed bool
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Already reviewed: %s. Proceed anyway?", meta)).
				Value(&proceed),
		))
		if err := form.RunWithContext(ctx); err != nil {
			fmt.Fprintln(out, "prompt cancelled")
			return err //nolint:wrapcheck // propagate huh cancellation
		}
		if !proceed {
			fmt.Fprintln(out, "Review cancelled.")
			return nil
		}
	}

	// 5. Resolve HEAD SHA and worktree root.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve worktree root: %w", err)
	}

	// 6. Resolve HEAD SHA and detect scope.
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}
	scopeBaseRef, scopeErr := detectScope(ctx, worktreeRoot, baseOverride, out)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	checkpointContext := ""
	if deps.ReviewCheckpointContext != nil {
		checkpointContext = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}

	runCfg := reviewtypes.RunConfig{
		ProfileName:       profileName,
		Task:              task,
		PerRunPrompt:      perRunPrompt,
		ScopeBaseRef:      scopeBaseRef,
		CheckpointContext: checkpointContext,
		StartingSHA:       headSHA,
	}
	applyReviewConfig(&runCfg, cfg)

	// 7. Branch on launchability.
	reviewer := deps.ReviewerFor(agentName)
	if reviewer == nil {
		// Non-launchable: write marker (with scope-aware prompt) and print guidance.
		return RunMarkerFallback(ctx, agentName, runCfg, worktreeRoot, out)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	runCfg.EnrichSummary = reviewSummaryTokenEnricher(worktreeRoot, headSHA)
	canPrompt := interactive.CanPromptInteractively()
	sinks := composeSingleAgentSinks(singleAgentSinkInputs{
		out:       out,
		isTTY:     interactive.IsTerminalWriter(out) && canPrompt,
		canPrompt: canPrompt,
		agentName: agentName,
		cancelRun: cancelRun,
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	summary, waitErr := Run(runCtx, reviewer, runCfg, sinks)
	writePostReviewManifest(ctx, out, worktreeRoot, headSHA, summary, "")
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		// Non-cancellation error: surface to caller.
		return fmt.Errorf("review run: %w", waitErr)
	}
	return nil
}

// detectScope computes the scope base ref for the current repo and prints
// a scope banner to out on success. baseOverride, when non-empty, comes from
// the `--base <ref>` flag and bypasses mainline auto-detection.
//
// Failure handling: when baseOverride is set and the ref is invalid,
// returns ("", err) so the caller can fail-loudly before spawning agents.
// Otherwise (auto-detection failed): returns "" and the caller proceeds in
// degraded mode without a scope banner.
func detectScope(ctx context.Context, worktreeRoot, baseOverride string, out io.Writer) (string, error) {
	repo, openErr := gitrepo.OpenPath(worktreeRoot)
	if openErr != nil {
		logging.Debug(ctx, "review repo open failed", slog.String("error", openErr.Error()))
		// Fail-loud when the user explicitly asked for a base. Without this
		// branch an explicit --base flag would be silently dropped on
		// PlainOpen failure, inconsistent with the ComputeScopeStats error
		// path below that aborts on bad overrides.
		if baseOverride != "" {
			return "", fmt.Errorf("--base %q given but cannot open repository at %q: %w", baseOverride, worktreeRoot, openErr)
		}
		return "", nil
	}
	defer repo.Close()
	stats, statsErr := ComputeScopeStats(ctx, repo, baseOverride)
	if statsErr != nil {
		// With an override, the user explicitly asked for a specific base.
		// A bad ref must abort before agents spawn so the user learns about
		// the typo immediately, not after a long review run.
		if baseOverride != "" {
			return "", statsErr
		}
		logging.Debug(ctx, "review scope detection failed", slog.String("error", statsErr.Error()))
		return "", nil
	}
	fmt.Fprintln(out, formatScopeBanner(stats))
	return stats.BaseRef, nil
}

// runMultiAgentPath handles the profile-native fan-out flow. Every configured
// worker in the selected profile runs concurrently against the same canonical
// task, then the profile's master agent produces the final report.
func runMultiAgentPath(
	ctx context.Context,
	cmd *cobra.Command,
	profileName string,
	profile settings.ReviewProfileConfig,
	launchableEligible []AgentChoice,
	baseOverride string,
	perRunPrompt string,
	deps Deps,
	out io.Writer,
) error {
	// Resolve worktree root and HEAD SHA for scope detection.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		cmd.SilenceUsage = true
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}

	if reviewed, meta := deps.HeadHasReviewCheckpoint(ctx); reviewed {
		var proceed bool
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Already reviewed: %s. Proceed anyway?", meta)).
				Value(&proceed),
		))
		if err := form.RunWithContext(ctx); err != nil {
			fmt.Fprintln(out, "prompt cancelled")
			return err //nolint:wrapcheck // propagate huh cancellation
		}
		if !proceed {
			fmt.Fprintln(out, "Review cancelled.")
			return nil
		}
	}

	scopeBaseRef, scopeErr := detectScope(ctx, worktreeRoot, baseOverride, out)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}
	checkpointContext := ""
	if deps.ReviewCheckpointContext != nil {
		checkpointContext = deps.ReviewCheckpointContext(ctx, worktreeRoot, scopeBaseRef)
	}
	reviewers := make([]reviewtypes.AgentReviewer, 0, len(launchableEligible))
	for _, choice := range launchableEligible {
		agentCfg := profile.Agents[choice.Name]
		if len(agentCfg.Skills) > 0 {
			ag, agErr := agent.Get(types.AgentName(choice.Name))
			if agErr != nil {
				return fmt.Errorf("resolve agent %s: %w", choice.Name, agErr)
			}
			if err := VerifyConfiguredSkillsInstalled(ctx, ag, agentCfg); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
				return deps.NewSilentError(err)
			}
		}
		reviewer := deps.ReviewerFor(choice.Name)
		if reviewer == nil {
			cmd.SilenceUsage = true
			return deps.NewSilentError(fmt.Errorf("agent %q is not launchable but appeared in eligible list", choice.Name))
		}
		reviewers = append(reviewers, &perAgentConfiguredReviewer{
			inner: reviewer,
			cfg: runConfigWithReviewConfig(reviewtypes.RunConfig{
				ProfileName:       profileName,
				Task:              profile.Task,
				PerRunPrompt:      perRunPrompt,
				ScopeBaseRef:      scopeBaseRef,
				CheckpointContext: checkpointContext,
				StartingSHA:       headSHA,
			}, agentCfg),
		})
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	agentNames := make([]string, len(reviewers))
	for i, r := range reviewers {
		agentNames[i] = r.Name()
	}
	aggregateOutput := ""

	masterProvider := AgentSynthesisProvider{AgentName: profile.Master, Model: profile.MasterModel}
	sinks := composeMultiAgentSinks(multiAgentSinkInputs{
		out:               out,
		isTTY:             interactive.IsTerminalWriter(out) && interactive.CanPromptInteractively(),
		canPrompt:         interactive.CanPromptInteractively(),
		agentNames:        agentNames,
		cancelRun:         cancelRun,
		runContext:        runCtx,
		synthesisProvider: masterProvider,
		perRunPrompt:      perRunPrompt,
		profileName:       profileName,
		task:              profile.Task,
		masterName:        profile.Master,
		autoSynthesis:     true,
		onSynthesisResult: func(result string) {
			aggregateOutput = result
		},
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	summary, waitErr := RunMulti(runCtx, reviewers, reviewtypes.RunConfig{
		EnrichAgentRun: reviewAgentRunTokenEnricher(worktreeRoot, headSHA),
	}, sinks)
	writePostReviewManifest(ctx, out, worktreeRoot, headSHA, summary, aggregateOutput)
	if waitErr != nil && runCtx.Err() == nil && ctx.Err() == nil {
		return fmt.Errorf("review run: %w", waitErr)
	}
	return nil
}

// handlePickerError maps multi-picker error sentinels to the appropriate
// command-layer response.
//   - ErrPickerCancelled → return nil (user cancelled; no error shown)
//   - ErrNoAgentsSelected → surface error to user
//   - other errors → surface to user
func handlePickerError(cmd *cobra.Command, silentErr func(error) error, pickErr error) error {
	if errors.Is(pickErr, ErrPickerCancelled) {
		return nil
	}
	cmd.SilenceUsage = true
	fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
	return silentErr(pickErr)
}

// multiAgentSinkInputs collects the parameters composeMultiAgentSinks needs.
// It exists so tests can drive the helper with explicit isTTY / canPrompt
// values instead of monkey-patching interactive helpers at run time.
//
// isTTY here means "the TUI sink is safe to compose" — production callers
// AND IsTerminalWriter(out) with CanPromptInteractively() before passing
// it in, since the TUI both writes ANSI to stdout AND reads keypresses
// from stdin. A terminal-stdout-but-non-interactive-stdin scenario (an
// agent host like Claude Code invoking `entire review`) must NOT use the
// TUI — its dismissal loop would block forever.
type multiAgentSinkInputs struct {
	out               io.Writer
	isTTY             bool
	canPrompt         bool
	agentNames        []string
	cancelRun         context.CancelFunc
	runContext        context.Context
	synthesisProvider SynthesisProvider
	promptYN          func(ctx context.Context, question string, def bool) (bool, error)
	perRunPrompt      string
	profileName       string
	task              string
	masterName        string
	autoSynthesis     bool
	onSynthesisResult func(result string)
}

type singleAgentSinkInputs struct {
	out       io.Writer
	isTTY     bool
	canPrompt bool
	agentName string
	cancelRun context.CancelFunc
}

// composeMultiAgentSinks builds the sink slice for a multi-agent run.
//
//   - Non-TTY: [DumpSink, SynthesisSink?] — narrative dump plus profile-native
//     final report when autoSynthesis is enabled.
//   - TTY: [TUISink, DumpSink, SynthesisSink?] — TUI owns the live dashboard;
//     DumpSink renders the post-run narrative; SynthesisSink renders the final
//     report after the TUI exits.
//
// Prompted legacy synthesis is still only appended when canPrompt is true.
// Profile-native auto synthesis does not need stdin, so it is available in
// redirected and CI output too.
func composeMultiAgentSinks(in multiAgentSinkInputs) []reviewtypes.Sink {
	sinks := []reviewtypes.Sink{}
	if in.isTTY {
		sinks = append(sinks, NewTUISink(in.agentNames, in.cancelRun, in.out, os.Stdin))
	}
	sinks = append(sinks, DumpSink{W: in.out})
	if in.synthesisProvider != nil && (in.autoSynthesis || in.canPrompt) {
		sinks = append(sinks, SynthesisSink{
			Provider:     in.synthesisProvider,
			Writer:       in.out,
			InputTTY:     in.canPrompt,
			PromptYN:     in.promptYN,
			PerRunPrompt: in.perRunPrompt,
			ProfileName:  in.profileName,
			Task:         in.task,
			MasterName:   in.masterName,
			Auto:         in.autoSynthesis,
			RunContext:   in.runContext,
			OnResult:     in.onSynthesisResult,
		})
	}
	return sinks
}

func writePostReviewManifest(
	ctx context.Context,
	out io.Writer,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	aggregateOutput string,
) {
	if summary.Cancelled || len(summary.AgentRuns) == 0 {
		return
	}
	manifest, states, err := localReviewManifestFromCurrentState(ctx, worktreeRoot, headSHA, summary, aggregateOutput)
	if err != nil {
		logging.Debug(ctx, "review manifest not written", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "could not load session state: "+err.Error())
		return
	}
	if len(manifest.Sources) == 0 {
		reason, sentinel := explainEmptyManifest(worktreeRoot, headSHA, summary, states)
		if sentinel {
			// Matcher and explainer have drifted — the matcher rejected
			// every tagged session for a reason none of the explainer's
			// filters cover. Surface at Warn so this gets noticed without
			// requiring debug logging.
			logging.Warn(ctx, "review manifest matcher/explainer drift detected",
				slog.String("reason", reason),
				slog.Int("tagged_state_count", len(states)),
				slog.Int("agent_run_count", len(summary.AgentRuns)))
		} else {
			logging.Debug(ctx, "review manifest not written: no matching review sessions",
				slog.String("reason", reason))
		}
		warnManifestNotWritten(out, reason)
		return
	}
	if err := writeLocalReviewManifest(ctx, manifest); err != nil {
		logging.Debug(ctx, "review manifest write failed", slog.String("error", err.Error()))
		warnManifestNotWritten(out, "write to disk failed: "+err.Error())
		return
	}
	writeReviewCompletionFooter(out, manifest)
}

// warnManifestNotWritten prints a user-visible note explaining that the
// review skills ran but findings were not persisted, so `entire review
// --findings` and `entire review --fix` will not see this run. The reason
// string is appended verbatim and should describe the underlying cause in
// terms the user can act on (or at least diagnose with debug logs).
func warnManifestNotWritten(out io.Writer, reason string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: review skills ran but findings were not persisted.")
	fmt.Fprintf(out, "  Reason: %s\n", reason)
	fmt.Fprintln(out, "  `entire review --findings` and `entire review --fix` will not see this run.")
	fmt.Fprintln(out, "  Re-run with `ENTIRE_LOG_LEVEL=debug` for diagnostic detail.")
}

func reviewSummaryTokenEnricher(worktreeRoot, headSHA string) func(context.Context, reviewtypes.RunSummary) reviewtypes.RunSummary {
	return func(ctx context.Context, summary reviewtypes.RunSummary) reviewtypes.RunSummary {
		enriched, err := hydrateReviewSummaryTokensFromCurrentState(ctx, worktreeRoot, headSHA, summary, agent.GetByAgentType)
		if err != nil {
			logging.Debug(ctx, "review token hydration skipped", slog.String("error", err.Error()))
			return summary
		}
		return enriched
	}
}

func reviewAgentRunTokenEnricher(worktreeRoot, headSHA string) func(context.Context, reviewtypes.AgentRun) reviewtypes.AgentRun {
	return func(ctx context.Context, run reviewtypes.AgentRun) reviewtypes.AgentRun {
		enriched, err := hydrateReviewAgentRunTokensFromCurrentState(ctx, worktreeRoot, headSHA, run, agent.GetByAgentType)
		if err != nil {
			logging.Debug(ctx, "review agent token hydration skipped", slog.String("error", err.Error()))
			return run
		}
		return enriched
	}
}

func composeSingleAgentSinks(in singleAgentSinkInputs) []reviewtypes.Sink {
	if !in.isTTY || !in.canPrompt {
		fmt.Fprintf(in.out, "Running review with %s...\n", in.agentName)
		return []reviewtypes.Sink{DumpSink{W: in.out}}
	}
	return []reviewtypes.Sink{
		NewTUISink([]string{in.agentName}, in.cancelRun, in.out, os.Stdin),
		DumpSink{W: in.out},
	}
}

func runConfigWithReviewConfig(base reviewtypes.RunConfig, cfg settings.ReviewConfig) reviewtypes.RunConfig {
	applyReviewConfig(&base, cfg)
	return base
}

func applyReviewConfig(runCfg *reviewtypes.RunConfig, cfg settings.ReviewConfig) {
	runCfg.Skills = cfg.Skills
	runCfg.AlwaysPrompt = cfg.Prompt
}

// findTUISink returns the first *TUISink in the slice (if any). Used by the
// caller to wire Start/Wait around the run without re-running composition.
func findTUISink(sinks []reviewtypes.Sink) (*TUISink, bool) {
	for _, s := range sinks {
		if t, ok := s.(*TUISink); ok {
			return t, true
		}
	}
	return nil, false
}

// perAgentConfiguredReviewer is an AgentReviewer adapter that overrides the
// RunConfig passed to the underlying reviewer's Start method. This lets
// RunMulti pass a single shared RunConfig at the API boundary while each
// agent in a multi-agent run still sees its own skills and always-prompt.
type perAgentConfiguredReviewer struct {
	inner reviewtypes.AgentReviewer
	cfg   reviewtypes.RunConfig
}

func (r *perAgentConfiguredReviewer) Name() string { return r.inner.Name() }
func (r *perAgentConfiguredReviewer) Start(ctx context.Context, _ reviewtypes.RunConfig) (reviewtypes.Process, error) {
	return r.inner.Start(ctx, r.cfg) //nolint:wrapcheck // transparent adapter; callers see inner's error type directly
}

// Compile-time interface check.
var _ reviewtypes.AgentReviewer = (*perAgentConfiguredReviewer)(nil)

// currentHeadSHA returns the current HEAD commit hash as a 40-char hex string.
func currentHeadSHA(ctx context.Context, repoRoot string) (string, error) {
	return gitexec.HeadSHA(ctx, repoRoot) //nolint:wrapcheck // gitexec already wraps
}
