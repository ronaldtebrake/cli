package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/importclaude"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "import",
		Short:  "Import pre-existing agent history into Entire (experimental)",
		Hidden: true,
		RunE:   func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newImportClaudeCodeCmd())
	return cmd
}

func newImportClaudeCodeCmd() *cobra.Command {
	var pathFlag string
	var dryRun bool
	var sessions []string

	cmd := &cobra.Command{
		Use:   "claude-code",
		Short: "Import existing Claude Code transcripts as local, read-only checkpoints",
		Long: `Import pre-existing Claude Code transcripts for this repo (the past month)
as local-only, read-only checkpoints. Imported history is searchable and
explainable but is never pushed and not rewindable.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				c.SilenceUsage = true
				fmt.Fprintln(c.ErrOrStderr(), "Not a git repository. Run 'entire enable' from within a git repository.")
				return NewSilentError(err)
			}
			repo, err := openRepository(ctx)
			if err != nil {
				return fmt.Errorf("open repository: %w", err)
			}
			defer repo.Close()

			res, err := importclaude.Run(ctx, repo, importclaude.Options{
				RepoRoot: repoRoot, OverridePath: pathFlag, SessionFilter: sessions,
				Now: time.Now(), DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			verb := "Imported"
			if dryRun {
				verb = "Would import"
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %d turn(s) from %d session(s) (%d already imported).\n",
				verb, res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&pathFlag, "path", "", "Override the Claude projects directory to import from")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be imported without writing")
	cmd.Flags().StringSliceVar(&sessions, "session", nil, "Import only these session IDs (repeatable)")
	return cmd
}
