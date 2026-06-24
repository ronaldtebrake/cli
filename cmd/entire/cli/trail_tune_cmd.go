package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

type trailTuneOptions struct {
	runner       string // optional: limit to one runner (id, with or without "trail-")
	run          bool   // headless apply vs. print prompt
	assumeYes    bool   // skip the create-defaults confirmation
	sources      []string
	limit        int
	insecureHTTP bool
}

func newTrailTuneCmd() *cobra.Command {
	var (
		run       bool
		assumeYes bool
		sources   []string
		limit     int
	)

	cmd := &cobra.Command{
		Use:   "tune [<runner>]",
		Short: "Tailor trail runner prompts to this repository",
		Long: `Tune the .entire/runners/*.json prompt templates to fit this repository.

The shipped runner templates (risk, confidence, drift, …) are written for a
generic web/backend app. "tune" gathers signal about THIS repo — its docs and
structure, merged PRs and issues, checkpoint churn hotspots, and past trail
findings — and produces an instruction prompt to rewrite the templates so their
dimensions and score bands fit what actually matters here.

By default it prints that prompt to stdout, ready to paste into your agent.
With --run it executes the prompt headlessly through your configured summary
provider and rewrites the runner files in place (review with git diff).

If the repo has no runners yet, tune offers to create the default set first
(use --yes to skip the confirmation), then tailors them.

If <runner> is given (e.g. "risk" or "trail-risk"), only that runner is tuned.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := ""
			if len(args) == 1 {
				runner = args[0]
			}
			return runTrailTune(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trailTuneOptions{
				runner:       runner,
				run:          run,
				assumeYes:    assumeYes,
				sources:      sources,
				limit:        limit,
				insecureHTTP: trailInsecureHTTP(cmd),
			})
		},
	}

	cmd.Flags().BoolVar(&run, "run", false,
		"Run the configured summary provider headlessly to rewrite the runner files in place (default: print the prompt)")
	cmd.Flags().StringSliceVar(&sources, "sources", nil,
		"Comma-separated data sources to gather: repo, prs, checkpoints, trails, all (default: all)")
	cmd.Flags().IntVar(&limit, "limit", 20, "How many recent PRs/issues/trails to sample")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false,
		"Skip the confirmation when creating the default runner set in a repo that has none")

	return cmd
}

func runTrailTune(ctx context.Context, w, errW io.Writer, opts trailTuneOptions) error {
	src, err := parseTuneSources(opts.sources)
	if err != nil {
		return err
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// Onboarding: a repo with no runners yet gets the default set scaffolded
	// (on confirmation), which tune then tailors below.
	if err := ensureRunnersPresent(w, errW, repoRoot, opts.assumeYes); err != nil {
		return err
	}

	runners, err := loadTuneRunners(repoRoot, opts.runner)
	if err != nil {
		return err
	}

	fmt.Fprintln(errW, "Gathering repository signal…")
	brief := gatherTuningContext(ctx, errW, repoRoot, src, opts.limit, opts.insecureHTTP)
	prompt := buildTunePrompt(brief, runners)

	if !opts.run {
		fmt.Fprintln(w, prompt)
		fmt.Fprintf(errW, "\n%d runner(s) in scope. Paste the prompt above into your agent, or re-run with --run to apply headlessly.\n", len(runners))
		return nil
	}

	return applyTuneWithAgent(ctx, w, errW, runners, prompt)
}

// applyTuneWithAgent runs the prompt through the configured summary provider
// (prompt -> text), parses the runner-id -> template map it returns, and
// surgically rewrites each runner file's prompt.template in place.
func applyTuneWithAgent(ctx context.Context, w, errW io.Writer, runners []tuneRunner, prompt string) error {
	// Reuse the summary-provider resolution (selection + persistence), but pull
	// the raw TextGenerator rather than provider.Generator: the latter is a
	// summarize.Generator that turns a transcript Input into a checkpoint
	// Summary, whereas we need plain prompt->text generation here.
	provider, err := resolveCheckpointSummaryProvider(ctx, errW)
	if err != nil {
		return err
	}
	ag, err := agent.Get(provider.Name)
	if err != nil {
		return fmt.Errorf("loading provider %s: %w", provider.Name, err)
	}
	textGen, ok := agent.AsTextGenerator(ag)
	if !ok {
		return fmt.Errorf("provider %s cannot generate text", provider.Name)
	}

	fmt.Fprintf(errW, "Tuning %d runner(s) with %s…\n", len(runners), provider.DisplayName)
	out, err := textGen.GenerateText(ctx, prompt, provider.Model)
	if err != nil {
		return fmt.Errorf("agent run failed: %w", err)
	}

	templates, err := parseTuneOutput(out)
	if err != nil {
		return err
	}

	byID := make(map[string]tuneRunner, len(runners))
	for _, r := range runners {
		byID[normalizeRunnerID(r.ID)] = r
	}

	updated, skipped := 0, 0
	for id, tmpl := range templates {
		r, ok := byID[normalizeRunnerID(id)]
		if !ok {
			fmt.Fprintf(errW, "skip %q: not a runner in scope\n", id)
			skipped++
			continue
		}
		if err := validateNewTemplate(r.Template, tmpl); err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if dropped := droppedPlaceholders(r.Template, tmpl); len(dropped) > 0 {
			fmt.Fprintf(errW, "note: %s no longer references %v\n", r.ID, dropped)
		}
		newRaw, err := replaceRunnerTemplate(r.Raw, tmpl)
		if err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if bytes.Equal(newRaw, r.Raw) {
			continue // model returned the current template verbatim — benign no-op
		}
		if err := os.WriteFile(r.Path, newRaw, 0o644); err != nil { //nolint:gosec // runner configs are repo-committed, world-readable config
			return fmt.Errorf("writing %s: %w", r.Path, err)
		}
		fmt.Fprintf(w, "updated %s\n", filepath.Base(r.Path))
		updated++
	}

	if updated > 0 {
		fmt.Fprintf(w, "\nUpdated %d runner(s). Review with: git diff .entire/runners\n", updated)
		return nil
	}
	// Nothing applied. An empty proposal set ({}) is a legitimate no-op; but if
	// the model proposed templates and every one was rejected or out of scope,
	// that is a failed run, not a clean no-op.
	if skipped > 0 {
		return fmt.Errorf("model proposed %d template(s) but all were rejected or out of scope (see messages above)", skipped)
	}
	fmt.Fprintln(w, "No runner changes proposed.")
	return nil
}
