package cli

// review_bridge.go wires cli-package implementations into the review
// subpackage's NewCommand Deps struct. Functions that need checkpoint
// access (headHasReviewCheckpoint) and per-agent reviewer constructors
// (launchableReviewerFor) live here to avoid the import cycle:
//   review → checkpoint → codex → review
//   review → claudecode/codex/geminicli → review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/api"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// buildReviewDeps builds the review.Deps struct used by review.NewCommand.
func buildReviewDeps() cliReview.Deps {
	return cliReview.Deps{
		GetAgentsWithHooksInstalled: GetAgentsWithHooksInstalled,
		NewSilentError: func(err error) error {
			return NewSilentError(err)
		},
		HeadHasReviewCheckpoint: headHasReviewCheckpoint,
		ReviewCheckpointContext: reviewCheckpointContext,
		ReviewerFor:             launchableReviewerFor,
		PostReviewToTrail:       postReviewToTrail,
	}
}

// postReviewToTrail posts the final review verdict to the current branch's
// trail as a finding, implementing the review subpackage's "trail" output
// destination. It lives in the cli package because the data API client and
// auth flow do.
func postReviewToTrail(ctx context.Context, out io.Writer, profileName, verdict string) error {
	if strings.TrimSpace(verdict) == "" {
		return errors.New("no review output to post")
	}
	input := reviewTrailFindingInput(profileName, verdict)
	return runAuthenticatedDataAPI(ctx, out, false, func(ctx context.Context, client *api.Client) error {
		target, err := resolveTrailReviewTarget(ctx, client, "")
		if err != nil {
			return err
		}
		if _, err := createTrailReviewFinding(ctx, client, target.Trail.ID, input); err != nil {
			return err
		}
		if target.Trail.Number > 0 {
			fmt.Fprintf(out, "Posted the review verdict to trail #%d as a finding.\n", target.Trail.Number)
		} else {
			fmt.Fprintln(out, "Posted the review verdict to the trail as a finding.")
		}
		if link := trailWebURL(target); link != "" {
			fmt.Fprintf(out, "View the trail: %s\n", link)
		}
		return nil
	})
}

// reviewTrailFindingInput builds the trail finding payload for a review verdict.
// The verdict spans the whole change, so it uses "whole_change" granularity: the
// API requires a valid granularity and rejects a zero/empty value with a 400.
func reviewTrailFindingInput(profileName, verdict string) api.TrailReviewCommentInput {
	body := strings.TrimSpace(verdict)
	if p := strings.TrimSpace(profileName); p != "" {
		body = fmt.Sprintf("Review verdict (profile: %s)\n\n%s", p, body)
	}
	return api.TrailReviewCommentInput{
		ClientID: generateTrailReviewClientID(),
		Body:     stringPtr(body),
		Location: api.TrailReviewLocationCreateRequest{Granularity: "whole_change"},
	}
}

// trailWebURL builds the browser URL for a trail, matching the server's
// `<base>/<forge>/<owner>/<repo>/trails/<number>/<branch>` layout (the web UI
// shares the API origin). Returns "" when the target lacks the parts needed for
// a stable link.
func trailWebURL(target trailReviewTarget) string {
	if target.Trail.Number <= 0 || target.Host == "" || target.Owner == "" || target.Repo == "" {
		return ""
	}
	base := strings.TrimRight(api.BaseURL(), "/")
	return fmt.Sprintf("%s/%s/%s/%s/trails/%d/%s",
		base, target.Host, target.Owner, target.Repo, target.Trail.Number, target.Trail.Branch)
}

// launchableReviewerFor returns the AgentReviewer for agents with a review-runner
// adapter, or nil for agents that are known to Entire but not yet wired into
// `entire review` fan-out. This lives in the cli package to avoid the import cycle:
//
//	review/cmd.go → claudecode/codex/geminicli → review
func launchableReviewerFor(agentName string) reviewtypes.AgentReviewer {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return claudecode.NewReviewer()
	case string(agent.AgentNameCodex):
		return codex.NewReviewer()
	case string(agent.AgentNameGemini):
		return geminicli.NewReviewer()
	default:
		return nil
	}
}
