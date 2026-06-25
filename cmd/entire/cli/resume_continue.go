package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"charm.land/huh/v2"
)

type restoredSessionStartPrompt func(context.Context) (bool, error)
type restoredSessionPicker func(context.Context, io.Writer, []strategy.RestoredSession) (strategy.RestoredSession, bool, error)
type restoredSessionLauncher func(context.Context, io.Writer, strategy.RestoredSession) error
type restoredSessionDisplayer func(io.Writer, []strategy.RestoredSession) error
type restoredSessionSummaryPrinter func(io.Writer, []strategy.RestoredSession)

type restoredSessionContinueOptions struct {
	CanPrompt          bool
	PreferredSessionID string
	PromptStartAgent   restoredSessionStartPrompt
	PromptSession      restoredSessionPicker
	Launch             restoredSessionLauncher
	Display            restoredSessionDisplayer
	PrintSummary       restoredSessionSummaryPrinter
}

func continueRestoredSessions(ctx context.Context, w io.Writer, sessions []strategy.RestoredSession, opts restoredSessionContinueOptions) error {
	if len(sessions) == 0 {
		return nil
	}

	display := opts.Display
	if display == nil {
		display = displayRestoredSessions
	}
	launch := opts.Launch
	if launch == nil {
		launch = launchTrailRestoredSession
	}
	promptStart := opts.PromptStartAgent
	if promptStart == nil {
		promptStart = promptStartRestoredAgent
	}
	promptSession := opts.PromptSession
	if promptSession == nil {
		promptSession = promptTrailRestoredSession
	}

	if opts.PreferredSessionID != "" {
		session, ok := findTrailRestoredSession(sessions, opts.PreferredSessionID)
		if !ok {
			return fmt.Errorf("session %q was not found in the restored checkpoint", opts.PreferredSessionID)
		}
		return continueSelectedRestoredSession(ctx, w, session, opts.CanPrompt, promptStart, launch, display, opts.PrintSummary)
	}

	if !opts.CanPrompt {
		return display(w, sessions)
	}

	startAgent, err := promptStart(ctx)
	if err != nil {
		return err
	}
	if !startAgent {
		return display(w, sessions)
	}

	if opts.PrintSummary != nil {
		opts.PrintSummary(w, sessions)
	}
	if len(sessions) == 1 {
		return launch(ctx, w, sessions[0])
	}

	selected, ok, err := promptSession(ctx, w, sessions)
	if err != nil || !ok {
		return err
	}
	return launch(ctx, w, selected)
}

func continueSelectedRestoredSession(
	ctx context.Context,
	w io.Writer,
	session strategy.RestoredSession,
	canPrompt bool,
	promptStart restoredSessionStartPrompt,
	launch restoredSessionLauncher,
	display restoredSessionDisplayer,
	printSummary restoredSessionSummaryPrinter,
) error {
	sessions := []strategy.RestoredSession{session}
	if !canPrompt {
		return display(w, sessions)
	}

	startAgent, err := promptStart(ctx)
	if err != nil {
		return err
	}
	if !startAgent {
		return display(w, sessions)
	}

	if printSummary != nil {
		printSummary(w, sessions)
	}
	return launch(ctx, w, session)
}

func promptStartRestoredAgent(ctx context.Context) (bool, error) {
	startAgent := true
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Start the agent now?").
				Description("Entire restored the checkpoint session log.").
				Affirmative("Start agent").
				Negative("Show commands").
				Value(&startAgent),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, fmt.Errorf("failed to choose resume action: %w", err)
	}
	return startAgent, nil
}
