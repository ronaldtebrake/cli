package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

const reviewCommandBinary = "entire review"

func runReviewFindings(ctx context.Context, cmd *cobra.Command, handle string, silentErr func(error) error) error {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return wrapReviewSilentError(silentErr, errors.New("not a git repository"))
	}
	manifests, err := loadLocalReviewManifests(ctx, worktreeRoot)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No local review findings found.")
		return nil
	}
	if handle = strings.TrimSpace(handle); handle != "" {
		manifest, findErr := findReviewManifestByHandle(manifests, handle)
		if findErr != nil {
			cmd.SilenceUsage = true
			fmt.Fprintln(cmd.ErrOrStderr(), findErr.Error())
			printReviewFindingsHandles(cmd.ErrOrStderr(), manifests)
			return wrapReviewSilentError(silentErr, findErr)
		}
		printReviewManifestDetail(cmd.OutOrStdout(), manifest)
		return nil
	}
	if interactive.IsTerminalWriter(cmd.OutOrStdout()) && interactive.CanPromptInteractively() {
		manifest, pickErr := promptForReviewManifest(ctx, manifests)
		if pickErr != nil {
			return pickErr
		}
		printReviewManifestDetail(cmd.OutOrStdout(), manifest)
		return nil
	}
	printReviewFindingsList(cmd.OutOrStdout(), manifests)
	return nil
}

func findReviewManifestByHandle(manifests []LocalReviewManifest, handle string) (LocalReviewManifest, error) {
	var matched []LocalReviewManifest
	for _, manifest := range manifests {
		if reviewManifestHasHandle(manifest, handle) {
			matched = append(matched, manifest)
		}
	}
	switch len(matched) {
	case 0:
		return LocalReviewManifest{}, fmt.Errorf("no local review findings match %q", handle)
	case 1:
		return matched[0], nil
	default:
		return LocalReviewManifest{}, fmt.Errorf("local review findings handle %q is ambiguous", handle)
	}
}

func wrapReviewSilentError(silentErr func(error) error, err error) error {
	if silentErr == nil {
		return err
	}
	return silentErr(err)
}

func promptForReviewManifest(ctx context.Context, manifests []LocalReviewManifest) (LocalReviewManifest, error) {
	options := make([]huh.Option[int], len(manifests))
	for i, manifest := range manifests {
		options[i] = huh.NewOption(reviewManifestListLabel(manifest), i)
	}
	picked := 0
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[int]().
			Title("Select review findings").
			Options(options...).
			Height(min(len(options)+1, 10)).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return LocalReviewManifest{}, fmt.Errorf("review findings picker: %w", err)
	}
	return manifests[picked], nil
}

// reviewPickerHeight reserves the title + description lines huh.MultiSelect
// subtracts from Height before sizing its option viewport. Shared by the
// profile master picker.
func reviewPickerHeight(optionCount int) int {
	return min(optionCount+3, 14)
}

func writeReviewCompletionFooter(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Review complete.")
	handle := reviewManifestHandle(manifest)
	if handle == "" {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Browse findings:")
	fmt.Fprintf(w, "  %s --findings %s\n", reviewCommandBinary, handle)
}

func reviewManifestHandle(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if source.SessionID != "" {
			return source.SessionID
		}
	}
	if !manifest.CreatedAt.IsZero() {
		return reviewManifestTimeHandle(manifest.CreatedAt)
	}
	return ""
}

func printReviewFindingsList(w io.Writer, manifests []LocalReviewManifest) {
	fmt.Fprintln(w, "Review Findings")
	fmt.Fprintln(w)
	for _, manifest := range manifests {
		fmt.Fprintf(w, "%s\n", reviewManifestListLabel(manifest))
		if handle := reviewManifestHandle(manifest); handle != "" {
			fmt.Fprintf(w, "  view: %s --findings %s\n", reviewCommandBinary, handle)
		}
	}
}

func printReviewFindingsHandles(w io.Writer, manifests []LocalReviewManifest) {
	handles := reviewManifestHandleList(manifests)
	if len(handles) == 0 {
		return
	}
	fmt.Fprintln(w, "Available findings:")
	for _, handle := range handles {
		fmt.Fprintf(w, "  %s\n", handle)
	}
}

func printReviewManifestDetail(w io.Writer, manifest LocalReviewManifest) {
	fmt.Fprintf(w, "Review findings from %s\n\n", reviewManifestListLabel(manifest))
	for _, source := range manifest.Sources {
		printRenderedReviewSection(w, source.Label, source.Output)
	}
	if strings.TrimSpace(manifest.AggregateOutput) != "" {
		printRenderedReviewSection(w, "Aggregate summary", manifest.AggregateOutput)
	}
}

func printRenderedReviewSection(w io.Writer, title string, body string) {
	markdown := fmt.Sprintf("## %s\n\n%s\n", title, strings.TrimSpace(body))
	rendered, err := mdrender.RenderForWriter(w, markdown)
	if err != nil {
		rendered = markdown
	}
	fmt.Fprint(w, rendered)
	if !strings.HasSuffix(rendered, "\n") {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
}

func reviewManifestListLabel(manifest LocalReviewManifest) string {
	handle := reviewManifestHandle(manifest)
	if handle == "" {
		handle = "unknown-session"
	}
	agents := make([]string, 0, len(manifest.Sources))
	for _, source := range manifest.Sources {
		if source.Label != "" {
			agents = append(agents, source.Label)
			continue
		}
		agents = append(agents, source.Agent)
	}
	preview := reviewManifestPreview(manifest)
	if preview != "" {
		return fmt.Sprintf("%s · local · %s · %s", handle, strings.Join(agents, ", "), preview)
	}
	return fmt.Sprintf("%s · local · %s", handle, strings.Join(agents, ", "))
}

func reviewManifestPreview(manifest LocalReviewManifest) string {
	for _, source := range manifest.Sources {
		if text := strings.TrimSpace(source.Output); text != "" {
			return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
		}
	}
	if text := strings.TrimSpace(manifest.AggregateOutput); text != "" {
		return stringutil.TruncateRunes(strings.Join(strings.Fields(text), " "), 70, "...")
	}
	return ""
}

func reviewManifestHasHandle(manifest LocalReviewManifest, handle string) bool {
	return slices.Contains(reviewManifestHandles(manifest), handle)
}

func reviewManifestHandleList(manifests []LocalReviewManifest) []string {
	handles := []string{}
	for _, manifest := range manifests {
		for _, handle := range reviewManifestHandles(manifest) {
			if slices.Contains(handles, handle) {
				continue
			}
			handles = append(handles, handle)
		}
	}
	return handles
}

func reviewManifestHandles(manifest LocalReviewManifest) []string {
	handles := []string{}
	add := func(handle string) {
		handle = strings.TrimSpace(handle)
		if handle == "" {
			return
		}
		if slices.Contains(handles, handle) {
			return
		}
		handles = append(handles, handle)
	}
	for _, source := range manifest.Sources {
		add(source.SessionID)
	}
	if !manifest.CreatedAt.IsZero() {
		add(reviewManifestTimeHandle(manifest.CreatedAt))
	}
	return handles
}

func reviewManifestTimeHandle(t time.Time) string {
	return t.UTC().Format("20060102T150405")
}
