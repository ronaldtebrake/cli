package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/runnerdefaults"

	"charm.land/huh/v2"
)

// ensureRunnersPresent scaffolds the default runner set when a repo has none
// yet, so `tune` doubles as onboarding. It returns (true, nil) when it created
// the defaults, (false, nil) when runners already existed, and an error when
// the user declined or creation failed. Writing is gated on confirmation
// (interactive prompt, or the --yes flag for non-interactive runs).
func ensureRunnersPresent(w, errW io.Writer, repoRoot string, assumeYes bool) (created bool, err error) {
	dir := filepath.Join(repoRoot, ".entire", "runners")
	existing, _ := filepath.Glob(filepath.Join(dir, "*.json")) //nolint:errcheck // bad pattern only; treated as "none found"
	if len(existing) > 0 {
		return false, nil
	}

	defaults, err := runnerdefaults.Files()
	if err != nil {
		return false, fmt.Errorf("loading default runners: %w", err)
	}

	if !assumeYes {
		if !interactive.CanPromptInteractively() {
			return false, fmt.Errorf("no runner configs found under %s; re-run with --yes to create the default set (%d runners)", dir, len(defaults))
		}
		confirmed, err := confirmCreateRunners(len(defaults))
		if err != nil {
			return false, err
		}
		if !confirmed {
			return false, errors.New("no runner configs created (declined)")
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // config dir, conventional perms
		return false, fmt.Errorf("creating %s: %w", dir, err)
	}
	for _, f := range defaults {
		dest := filepath.Join(dir, f.Name)
		if err := os.WriteFile(dest, f.Data, 0o644); err != nil { //nolint:gosec // runner configs are repo-committed, world-readable config
			return false, fmt.Errorf("writing %s: %w", dest, err)
		}
		fmt.Fprintf(w, "created %s\n", filepath.Join(".entire", "runners", f.Name))
	}
	fmt.Fprintf(errW, "Created %d default runner(s); tailoring them to this repo…\n", len(defaults))
	return true, nil
}

func confirmCreateRunners(n int) (bool, error) {
	var ok bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("No trail runners found. Create the default set (%d runners) in .entire/runners/?", n)).
				Description("Written from the built-in defaults, then tailored to this repo.").
				Value(&ok),
		),
	)
	if err := form.Run(); err != nil {
		return false, fmt.Errorf("runner-creation prompt cancelled: %w", err)
	}
	return ok, nil
}
