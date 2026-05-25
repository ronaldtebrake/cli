package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

const (
	defaultTrailReviewLimit    = 100
	trailReviewStatusAny       = "any"
	trailReviewStaleCurrent    = "current"
	trailReviewStaleAny        = "any"
	trailReviewStatusOpen      = "open"
	trailReviewStatusResolved  = "resolved"
	trailReviewStatusDismissed = "dismissed"
)

type trailReviewListOptions struct {
	Status           string
	Severity         string
	Stale            string
	IncludeDismissed bool
	Limit            int
	Offset           int
	JSON             bool
}

type trailReviewTarget struct {
	Host  string
	Owner string
	Repo  string
	Trail api.TrailResource
}

func newTrailReviewCmd() *cobra.Command {
	opts := defaultTrailReviewListOptions()

	cmd := &cobra.Command{
		Use:   "review [<number>]",
		Short: "Review a trail's agent findings",
		Long: `Review a trail's agent-native code-review findings.

Running 'entire trail review' shows the review dashboard for the current
branch's trail. Pass a trail number to review another trail in the same repo.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseOptionalTrailNumber(args)
			if err != nil {
				return err
			}
			return runTrailReviewDashboard(cmd, number, opts)
		},
	}
	addTrailReviewListFlags(cmd, &opts)

	cmd.AddCommand(newTrailReviewStartCmd())
	cmd.AddCommand(newTrailReviewCommentsCmd())
	cmd.AddCommand(newTrailReviewShowCmd())
	cmd.AddCommand(newTrailReviewApplyCmd())
	cmd.AddCommand(newTrailReviewStatusCmd("resolve", trailReviewStatusResolved, "Resolve a review finding"))
	cmd.AddCommand(newTrailReviewStatusCmd("dismiss", trailReviewStatusDismissed, "Dismiss a review finding"))
	cmd.AddCommand(newTrailReviewStatusCmd("reopen", trailReviewStatusOpen, "Reopen a review finding"))
	cmd.AddCommand(newTrailReviewWatchCmd())
	cmd.AddCommand(newTrailReviewSubmitCmd("approve", "APPROVE", "Approve a trail"))
	cmd.AddCommand(newTrailReviewSubmitCmd("request-changes", "REQUEST_CHANGES", "Request changes on a trail"))

	return cmd
}

func defaultTrailReviewListOptions() trailReviewListOptions {
	return trailReviewListOptions{
		Status: trailReviewStatusOpen,
		Stale:  trailReviewStaleCurrent,
		Limit:  defaultTrailReviewLimit,
	}
}

func addTrailReviewListFlags(cmd *cobra.Command, opts *trailReviewListOptions) {
	cmd.Flags().StringVar(&opts.Status, "status", opts.Status, "Filter by comma-separated status(es): open,resolved,dismissed; use 'any' for all")
	cmd.Flags().StringVar(&opts.Severity, "severity", "", "Filter by comma-separated severity value(s): high,medium,low")
	cmd.Flags().StringVar(&opts.Stale, "stale", opts.Stale, "Filter stale state: current,stale,any")
	cmd.Flags().BoolVar(&opts.IncludeDismissed, "include-dismissed", false, "Include dismissed findings")
	cmd.Flags().IntVarP(&opts.Limit, "limit", "n", opts.Limit, "Maximum number of findings to show")
	cmd.Flags().IntVar(&opts.Offset, "offset", 0, "Pagination offset")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
}

type trailReviewStartOptions struct {
	BaseRef string
	HeadRef string
	BaseSHA string
	HeadSHA string
	JSON    bool
	Watch   bool
}

func newTrailReviewStartCmd() *cobra.Command {
	var opts trailReviewStartOptions
	cmd := &cobra.Command{
		Use:   "start [<number>]",
		Short: "Start an agent-native review for a trail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseOptionalTrailNumber(args)
			if err != nil {
				return err
			}
			return runTrailReviewStart(cmd, number, opts)
		},
	}
	cmd.Flags().StringVar(&opts.BaseRef, "base-ref", "", "Base ref covered by the review (defaults to trail base)")
	cmd.Flags().StringVar(&opts.HeadRef, "head-ref", "", "Head ref covered by the review (defaults to current branch)")
	cmd.Flags().StringVar(&opts.BaseSHA, "base-sha", "", "Base SHA covered by the review")
	cmd.Flags().StringVar(&opts.HeadSHA, "head-sha", "", "Head SHA covered by the review (defaults to HEAD)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&opts.Watch, "watch", false, "After starting, stream trail review events")
	return cmd
}

func newTrailReviewCommentsCmd() *cobra.Command {
	opts := defaultTrailReviewListOptions()
	cmd := &cobra.Command{
		Use:   "comments [<number>]",
		Short: "List review findings for a trail",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseOptionalTrailNumber(args)
			if err != nil {
				return err
			}
			return runTrailReviewComments(cmd, number, opts)
		},
	}
	addTrailReviewListFlags(cmd, &opts)
	return cmd
}

func newTrailReviewShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [<number>] <comment-id>",
		Short: "Show a review finding",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, commentID, err := parseTrailNumberAndCommentID(args)
			if err != nil {
				return err
			}
			return runTrailReviewShow(cmd, number, commentID)
		},
	}
	return cmd
}

type trailReviewApplyOptions struct {
	Resolve bool
	Check   bool
}

func newTrailReviewApplyCmd() *cobra.Command {
	var opts trailReviewApplyOptions
	cmd := &cobra.Command{
		Use:   "apply [<number>] <comment-id>",
		Short: "Apply a review finding's unified-diff suggestion",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, commentID, err := parseTrailNumberAndCommentID(args)
			if err != nil {
				return err
			}
			return runTrailReviewApply(cmd, number, commentID, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Resolve, "resolve", false, "Mark the finding resolved after applying")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "Only check whether the patch applies; do not modify files")
	return cmd
}

func newTrailReviewStatusCmd(use, status, short string) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   use + " [<number>] <comment-id>",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, commentID, err := parseTrailNumberAndCommentID(args)
			if err != nil {
				return err
			}
			return runTrailReviewSetStatus(cmd, number, commentID, status, message)
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Status reason to record")
	return cmd
}

func newTrailReviewWatchCmd() *cobra.Command {
	var (
		jsonOutput bool
		showPings  bool
		once       bool
	)
	cmd := &cobra.Command{
		Use:   "watch [<number>]",
		Short: "Tail a trail's code-review events live",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseOptionalTrailNumber(args)
			if err != nil {
				return err
			}
			return runTrailWatchWithOptions(cmd, number, jsonOutput, showPings, once)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print each event as a single JSON line")
	cmd.Flags().BoolVar(&showPings, "show-pings", false, "Print SSE keepalive pings (otherwise suppressed)")
	cmd.Flags().BoolVar(&once, "once", false, "Open one SSE connection then exit instead of reconnecting")
	return cmd
}

func newTrailReviewSubmitCmd(use, event, short string) *cobra.Command {
	var body string
	cmd := &cobra.Command{
		Use:   use + " [<number>]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			number, err := parseOptionalTrailNumber(args)
			if err != nil {
				return err
			}
			return runTrailReviewSubmit(cmd, number, event, body)
		},
	}
	cmd.Flags().StringVarP(&body, "message", "m", "", "Review message")
	return cmd
}

func runTrailReviewDashboard(cmd *cobra.Command, number int, opts trailReviewListOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	comments, hasMore, err := fetchTrailReviewComments(cmd.Context(), client, target.Trail.ID, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return encodeTrailReviewJSON(cmd.OutOrStdout(), target, comments, hasMore)
	}
	printTrailReviewDashboard(cmd.OutOrStdout(), target, comments, hasMore, opts)
	return nil
}

func runTrailReviewComments(cmd *cobra.Command, number int, opts trailReviewListOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	comments, hasMore, err := fetchTrailReviewComments(cmd.Context(), client, target.Trail.ID, opts)
	if err != nil {
		return err
	}
	if opts.JSON {
		return encodeTrailReviewJSON(cmd.OutOrStdout(), target, comments, hasMore)
	}
	printTrailReviewComments(cmd.OutOrStdout(), comments, hasMore)
	return nil
}

func runTrailReviewShow(cmd *cobra.Command, number int, commentID string) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	comment, _ = hydrateTrailReviewCommentSuggestions(cmd.Context(), client, target.Trail.ID, comment) // best-effort detail enrichment
	printTrailReviewCommentDetail(cmd.OutOrStdout(), comment)
	return nil
}

func runTrailReviewApply(cmd *cobra.Command, number int, commentID string, opts trailReviewApplyOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	comment, state, err := hydrateTrailReviewCommentWithState(cmd.Context(), client, target.Trail.ID, comment)
	if err != nil {
		return err
	}
	if err := verifyTrailReviewHead(cmd.Context(), state); err != nil {
		return err
	}
	applied, err := applyTrailReviewSuggestions(cmd.Context(), comment, opts.Check, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if opts.Check {
		fmt.Fprintf(cmd.OutOrStdout(), "%d suggested change(s) apply cleanly.\n", applied)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Applied %d suggested change(s).\n", applied)
	if opts.Resolve {
		updated, err := patchTrailReviewCommentStatus(cmd.Context(), client, target.Trail.ID, comment, trailReviewStatusResolved, "Applied via Entire CLI")
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Updated comment %s: %s → %s\n", updated.ID, comment.Status, updated.Status)
	}
	return nil
}

func runTrailReviewSetStatus(cmd *cobra.Command, number int, commentID, status, message string) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	comment, err := resolveTrailReviewComment(cmd.Context(), client, target.Trail.ID, commentID)
	if err != nil {
		return err
	}
	oldStatus := comment.Status
	if message == "" {
		message = defaultTrailReviewStatusReason(status)
	}
	updated, err := patchTrailReviewCommentStatus(cmd.Context(), client, target.Trail.ID, comment, status, message)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated comment %s: %s → %s\n", updated.ID, oldStatus, updated.Status)
	return nil
}

func runTrailReviewStart(cmd *cobra.Command, number int, opts trailReviewStartOptions) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	request, err := buildTrailReviewStartRequest(cmd.Context(), target, opts)
	if err != nil {
		return err
	}
	idempotencyKey := trailReviewStartIdempotencyKey(target.Trail.ID, request)
	started, err := startTrailReview(cmd.Context(), client, target.Trail.ID, request, idempotencyKey)
	if err != nil {
		return err
	}
	if opts.JSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(started); err != nil {
			return fmt.Errorf("encode review start response: %w", err)
		}
	} else {
		printTrailReviewStart(cmd.OutOrStdout(), target, started)
	}
	if opts.Watch {
		return runTrailWatchWithOptions(cmd, number, false, false, false)
	}
	return nil
}

func runTrailReviewSubmit(cmd *cobra.Command, number int, event, body string) error {
	client, target, err := authenticatedTrailReviewTarget(cmd, number)
	if err != nil {
		return err
	}
	if target.Trail.Number <= 0 {
		return fmt.Errorf("trail %s has no number; cannot submit review", target.Trail.ID)
	}
	if event == "REQUEST_CHANGES" && strings.TrimSpace(body) == "" {
		return errors.New("request-changes requires --message")
	}
	resp, err := submitTrailReview(cmd.Context(), client, target, event, body)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s submitted by %s on %s\n", resp.Review.Event, resp.Review.Author, abbreviateMaybe(resp.Review.CommitSHA, 12))
	return nil
}

func authenticatedTrailReviewTarget(cmd *cobra.Command, number int) (*api.Client, trailReviewTarget, error) {
	client, err := NewAuthenticatedAPIClient(trailInsecureHTTP(cmd))
	if err != nil {
		return nil, trailReviewTarget{}, fmt.Errorf("authentication required: %w", err)
	}
	target, err := resolveTrailReviewTarget(cmd.Context(), client, number)
	if err != nil {
		return nil, trailReviewTarget{}, err
	}
	return client, target, nil
}

func resolveTrailReviewTarget(ctx context.Context, client *api.Client, number int) (trailReviewTarget, error) {
	host, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return trailReviewTarget{}, fmt.Errorf("failed to resolve repository: %w", err)
	}

	var found *api.TrailResource
	if number > 0 {
		found, err = findTrailByNumber(ctx, client, host, owner, repo, number)
		if err != nil {
			return trailReviewTarget{}, err
		}
		if found == nil {
			return trailReviewTarget{}, fmt.Errorf("no trail #%d found in %s/%s/%s", number, host, owner, repo)
		}
	} else {
		branch, branchErr := GetCurrentBranch(ctx)
		if branchErr != nil {
			return trailReviewTarget{}, fmt.Errorf("no trail number given and current branch is unknown: %w", branchErr)
		}
		found, err = findTrailByBranch(ctx, client, host, owner, repo, branch)
		if err != nil {
			return trailReviewTarget{}, err
		}
		if found == nil {
			return trailReviewTarget{}, fmt.Errorf("no trail found for branch %q (run 'entire trail create' or pass a trail number)", branch)
		}
	}
	if found.ID == "" {
		return trailReviewTarget{}, errors.New("trail has no id yet")
	}
	return trailReviewTarget{Host: host, Owner: owner, Repo: repo, Trail: *found}, nil
}

func fetchTrailReviewComments(ctx context.Context, client *api.Client, trailID string, opts trailReviewListOptions) ([]api.TrailReviewComment, bool, error) {
	if opts.Limit <= 0 {
		return nil, false, errors.New("limit must be greater than 0")
	}
	if opts.Offset < 0 {
		return nil, false, errors.New("offset must be non-negative")
	}
	resp, err := client.Get(ctx, trailReviewCommentsPath(trailID, opts))
	if err != nil {
		return nil, false, fmt.Errorf("list review comments: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return nil, false, err
	}
	var out api.TrailReviewCommentsResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return nil, false, fmt.Errorf("decode review comments: %w", err)
	}
	return out.Comments, out.HasMore, nil
}

func trailReviewCommentsPath(trailID string, opts trailReviewListOptions) string {
	q := url.Values{}
	if opts.Status != "" && opts.Status != trailReviewStatusAny {
		q.Set("status", opts.Status)
	}
	if opts.Severity != "" {
		q.Set("severity", opts.Severity)
	}
	if opts.Stale != "" && opts.Stale != trailReviewStaleAny {
		q.Set("stale", opts.Stale)
	}
	if opts.IncludeDismissed {
		q.Set("include_dismissed", "true")
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/comments"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

func resolveTrailReviewComment(ctx context.Context, client *api.Client, trailID, commentID string) (api.TrailReviewComment, error) {
	opts := trailReviewListOptions{
		Status:           trailReviewStatusAny,
		Stale:            trailReviewStaleAny,
		IncludeDismissed: true,
		Limit:            defaultTrailReviewLimit,
	}
	var matches []api.TrailReviewComment
	for {
		comments, hasMore, err := fetchTrailReviewComments(ctx, client, trailID, opts)
		if err != nil {
			return api.TrailReviewComment{}, err
		}
		for _, comment := range comments {
			if comment.ID == commentID {
				return comment, nil
			}
			if strings.HasPrefix(comment.ID, commentID) {
				matches = append(matches, comment)
			}
		}
		if !hasMore {
			break
		}
		opts.Offset += opts.Limit
	}
	switch len(matches) {
	case 0:
		return api.TrailReviewComment{}, fmt.Errorf("no review comment %q found", commentID)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].ID
		}
		sort.Strings(ids)
		return api.TrailReviewComment{}, fmt.Errorf("ambiguous review comment %q (matches: %s)", commentID, strings.Join(ids, ", "))
	}
}

func hydrateTrailReviewCommentSuggestions(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment) (api.TrailReviewComment, error) {
	hydrated, _, err := hydrateTrailReviewCommentWithState(ctx, client, trailID, comment)
	return hydrated, err
}

func hydrateTrailReviewCommentWithState(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment) (api.TrailReviewComment, api.TrailReviewStateResponse, error) {
	state, err := fetchTrailReviewState(ctx, client, trailID, comment.ReviewID)
	if err != nil {
		return api.TrailReviewComment{}, api.TrailReviewStateResponse{}, err
	}
	for _, candidate := range state.Comments {
		if candidate.ID == comment.ID {
			return candidate, state, nil
		}
	}
	return api.TrailReviewComment{}, api.TrailReviewStateResponse{}, fmt.Errorf("review %s did not include comment %s", comment.ReviewID, comment.ID)
}

func fetchTrailReviewState(ctx context.Context, client *api.Client, trailID, reviewID string) (api.TrailReviewStateResponse, error) {
	resp, err := client.Get(ctx, trailReviewStatePath(trailID, reviewID))
	if err != nil {
		return api.TrailReviewStateResponse{}, fmt.Errorf("get review state: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailReviewStateResponse{}, err
	}
	var out api.TrailReviewStateResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.TrailReviewStateResponse{}, fmt.Errorf("decode review state: %w", err)
	}
	return out, nil
}

func trailReviewStatePath(trailID, reviewID string) string {
	q := url.Values{}
	q.Set("include_dismissed", "true")
	q.Set("stale", trailReviewStaleAny)
	q.Set("limit", strconv.Itoa(defaultTrailReviewLimit))
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/" + url.PathEscape(reviewID) + "?" + q.Encode()
}

func verifyTrailReviewHead(ctx context.Context, state api.TrailReviewStateResponse) error {
	want := strings.TrimSpace(optionalStringValue(state.CodeVersion.HeadSHA))
	if want == "" {
		return nil
	}
	got, err := resolveGitRev(ctx, "HEAD")
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("review was created for head %s, but current HEAD is %s; check out the reviewed commit or start a new review", abbreviateMaybe(want, 12), abbreviateMaybe(got, 12))
	}
	return nil
}

func applyTrailReviewSuggestions(ctx context.Context, comment api.TrailReviewComment, checkOnly bool, w io.Writer) (int, error) {
	if len(comment.SuggestedChanges) == 0 {
		return 0, fmt.Errorf("comment %s has no suggested changes", comment.ID)
	}
	applied := 0
	for _, change := range comment.SuggestedChanges {
		patch := strings.TrimSpace(stringPtrValue(change.Patch))
		if change.ChangeType != "unified_diff" || patch == "" {
			fmt.Fprintf(w, "Skipping suggested change %s (%s): only unified_diff patches are supported.\n", change.ID, change.ChangeType)
			continue
		}
		if err := runGitApply(ctx, patch, true); err != nil {
			return applied, fmt.Errorf("suggested change %s does not apply cleanly: %w", change.ID, err)
		}
		if !checkOnly {
			if err := runGitApply(ctx, patch, false); err != nil {
				return applied, fmt.Errorf("apply suggested change %s: %w", change.ID, err)
			}
		}
		applied++
	}
	if applied == 0 {
		return 0, fmt.Errorf("comment %s has no supported unified_diff suggested changes", comment.ID)
	}
	return applied, nil
}

func runGitApply(ctx context.Context, patch string, checkOnly bool) error {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	args := []string{"-C", root, "apply"}
	if checkOnly {
		args = append(args, "--check")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = bytes.NewBufferString(patch + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("git apply: %w: %s", err, msg)
		}
		return fmt.Errorf("git apply: %w", err)
	}
	return nil
}

func patchTrailReviewCommentStatus(ctx context.Context, client *api.Client, trailID string, comment api.TrailReviewComment, status, reason string) (api.TrailReviewComment, error) {
	body := api.TrailReviewCommentPatchRequest{Status: status}
	if strings.TrimSpace(reason) != "" {
		body.StatusReason = stringPtr(reason)
	}
	resp, err := client.Patch(ctx, trailReviewCommentPath(trailID, comment.ReviewID, comment.ID), body)
	if err != nil {
		return api.TrailReviewComment{}, fmt.Errorf("update review comment: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailReviewComment{}, err
	}
	var updated api.TrailReviewComment
	if err := api.DecodeJSON(resp, &updated); err != nil {
		return api.TrailReviewComment{}, fmt.Errorf("decode updated review comment: %w", err)
	}
	return updated, nil
}

func trailReviewCommentPath(trailID, reviewID, commentID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews/" + url.PathEscape(reviewID) + "/comments/" + url.PathEscape(commentID)
}

func startTrailReview(ctx context.Context, client *api.Client, trailID string, request api.TrailReviewStartRequest, idempotencyKey string) (api.TrailReviewStartResponse, error) {
	headers := http.Header{}
	if idempotencyKey != "" {
		headers.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := client.PostWithHeaders(ctx, trailReviewStartPath(trailID), request, headers)
	if err != nil {
		return api.TrailReviewStartResponse{}, fmt.Errorf("start review: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailReviewStartResponse{}, err
	}
	var out api.TrailReviewStartResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.TrailReviewStartResponse{}, fmt.Errorf("decode review start response: %w", err)
	}
	return out, nil
}

func trailReviewStartPath(trailID string) string {
	return "/api/v1/trails/" + url.PathEscape(trailID) + "/reviews"
}

func buildTrailReviewStartRequest(ctx context.Context, target trailReviewTarget, opts trailReviewStartOptions) (api.TrailReviewStartRequest, error) {
	baseRef := strings.TrimSpace(opts.BaseRef)
	if baseRef == "" {
		baseRef = target.Trail.Base
	}
	headRef := strings.TrimSpace(opts.HeadRef)
	if headRef == "" {
		if branch, err := GetCurrentBranch(ctx); err == nil {
			headRef = branch
		}
	}
	headSHA := strings.TrimSpace(opts.HeadSHA)
	if headSHA == "" {
		sha, err := resolveGitRev(ctx, "HEAD")
		if err != nil {
			return api.TrailReviewStartRequest{}, err
		}
		headSHA = sha
	}
	baseSHA := strings.TrimSpace(opts.BaseSHA)
	if baseSHA == "" && baseRef != "" {
		if sha, err := resolveGitRev(ctx, baseRef); err == nil {
			baseSHA = sha
		}
	}
	return api.TrailReviewStartRequest{
		HeadSHA: optionalStringPtr(headSHA),
		BaseSHA: optionalStringPtr(baseSHA),
		BaseRef: optionalStringPtr(baseRef),
		HeadRef: optionalStringPtr(headRef),
	}, nil
}

func resolveGitRev(ctx context.Context, ref string) (string, error) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("git rev-parse %s: empty output", ref)
	}
	return sha, nil
}

func trailReviewStartIdempotencyKey(trailID string, request api.TrailReviewStartRequest) string {
	parts := []string{"trail-review-start", trailID, optionalStringValue(request.BaseSHA), optionalStringValue(request.HeadSHA), optionalStringValue(request.BaseRef), optionalStringValue(request.HeadRef)}
	return strings.Join(parts, ":")
}

func submitTrailReview(ctx context.Context, client *api.Client, target trailReviewTarget, event, body string) (api.TrailSubmitReviewResponse, error) {
	path := trailsBasePath(target.Host, target.Owner, target.Repo) + "/" + strconv.Itoa(target.Trail.Number) + "/review"
	resp, err := client.Post(ctx, path, api.TrailSubmitReviewRequest{Event: event, Body: body})
	if err != nil {
		return api.TrailSubmitReviewResponse{}, fmt.Errorf("submit trail review: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return api.TrailSubmitReviewResponse{}, err
	}
	var out api.TrailSubmitReviewResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return api.TrailSubmitReviewResponse{}, fmt.Errorf("decode trail review response: %w", err)
	}
	return out, nil
}

func encodeTrailReviewJSON(w io.Writer, target trailReviewTarget, comments []api.TrailReviewComment, hasMore bool) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"trail":    target.Trail,
		"comments": comments,
		"has_more": hasMore,
	})
}

func printTrailReviewDashboard(w io.Writer, target trailReviewTarget, comments []api.TrailReviewComment, hasMore bool, opts trailReviewListOptions) {
	trail := target.Trail
	fmt.Fprintf(w, "Trail #%d  %s\n", trail.Number, trail.Title)
	fmt.Fprintf(w, "Status: %s   Branch: %s   Base: %s\n", trail.Status, trail.Branch, trail.Base)
	fmt.Fprintf(w, "ID: %s\n\n", trail.ID)

	counts := countTrailReviewComments(comments)
	fmt.Fprintf(w, "Open findings: %d  high %d  medium %d  low %d\n", counts.Open, counts.OpenHigh, counts.OpenMedium, counts.OpenLow)
	fmt.Fprintf(w, "Resolved: %d        Dismissed: %d     Stale: %d\n", counts.Resolved, counts.Dismissed, counts.Stale)
	if hasMore {
		fmt.Fprintf(w, "Showing first %d findings; rerun with --offset for more.\n", opts.Limit)
	}
	fmt.Fprintln(w)

	if len(comments) == 0 {
		fmt.Fprintln(w, "No review findings found.")
		return
	}

	for _, severity := range []string{"high", "medium", "low", ""} {
		group := filterCommentsBySeverity(comments, severity)
		if len(group) == 0 {
			continue
		}
		title := severityTitle(severity)
		fmt.Fprintln(w, title)
		for _, comment := range group {
			fmt.Fprintf(w, "  %s %s   %s   %s\n", severityInitial(comment.Severity), trailReviewLocationDisplay(comment.Location), abbreviateMaybe(comment.ID, 12), trailReviewCommentTitle(comment))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  entire trail review show <comment-id>")
	fmt.Fprintln(w, "  entire trail review apply <comment-id> --resolve")
	fmt.Fprintln(w, "  entire trail review resolve <comment-id> -m \"fixed in <sha>\"")
	fmt.Fprintln(w, "  entire trail review dismiss <comment-id> -m \"not applicable\"")
	fmt.Fprintln(w, "  entire trail review watch")
}

func printTrailReviewComments(w io.Writer, comments []api.TrailReviewComment, hasMore bool) {
	if len(comments) == 0 {
		fmt.Fprintln(w, "No review findings found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEV\tSTATUS\tLOCATION\tTITLE")
	for _, comment := range comments {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", abbreviateMaybe(comment.ID, 12), severityDisplay(comment.Severity), comment.Status, trailReviewLocationDisplay(comment.Location), trailReviewCommentTitle(comment))
	}
	_ = tw.Flush()
	if hasMore {
		fmt.Fprintln(w, "More findings available; rerun with --offset for the next page.")
	}
}

func printTrailReviewCommentDetail(w io.Writer, comment api.TrailReviewComment) {
	fmt.Fprintf(w, "Comment:  %s\n", comment.ID)
	fmt.Fprintf(w, "Review:   %s\n", comment.ReviewID)
	fmt.Fprintf(w, "Status:   %s\n", comment.Status)
	fmt.Fprintf(w, "Severity: %s\n", severityDisplay(comment.Severity))
	if comment.Confidence != nil {
		fmt.Fprintf(w, "Confidence: %.2f\n", *comment.Confidence)
	}
	fmt.Fprintf(w, "Location: %s\n", trailReviewLocationDisplay(comment.Location))
	if title := trailReviewCommentTitle(comment); title != "" {
		fmt.Fprintf(w, "Title:    %s\n", title)
	}
	if body := stringPtrValue(comment.Body); body != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(body))
	}
	if len(comment.SuggestedChanges) > 0 {
		fmt.Fprintln(w, "\nSuggested changes:")
		for _, change := range comment.SuggestedChanges {
			fmt.Fprintf(w, "- %s (%s)\n", change.ID, change.ChangeType)
			if change.ExpectedFilePath != nil && *change.ExpectedFilePath != "" {
				fmt.Fprintf(w, "  file: %s\n", *change.ExpectedFilePath)
			}
			if patch := stringPtrValue(change.Patch); patch != "" {
				fmt.Fprintf(w, "  patch:\n%s\n", strings.TrimSpace(patch))
			}
			if instruction := stringPtrValue(change.Instruction); instruction != "" {
				fmt.Fprintf(w, "  instruction: %s\n", instruction)
			}
		}
	}
}

func printTrailReviewStart(w io.Writer, target trailReviewTarget, started api.TrailReviewStartResponse) {
	fmt.Fprintf(w, "Started review %s for trail #%d (%s)\n", started.ReviewID, target.Trail.Number, target.Trail.Title)
	fmt.Fprintf(w, "  Code version: %s\n", started.CodeVersionID)
	if started.BaseSHA != nil || started.HeadSHA != nil {
		fmt.Fprintf(w, "  Base: %s\n", abbreviateMaybe(optionalStringValue(started.BaseSHA), 12))
		fmt.Fprintf(w, "  Head: %s\n", abbreviateMaybe(optionalStringValue(started.HeadSHA), 12))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next:")
	fmt.Fprintf(w, "  entire trail review watch %d\n", target.Trail.Number)
	fmt.Fprintf(w, "  entire trail review comments %d\n", target.Trail.Number)
}

type trailReviewCommentCounts struct {
	Open       int
	OpenHigh   int
	OpenMedium int
	OpenLow    int
	Resolved   int
	Dismissed  int
	Stale      int
}

func countTrailReviewComments(comments []api.TrailReviewComment) trailReviewCommentCounts {
	var counts trailReviewCommentCounts
	for _, comment := range comments {
		switch comment.Status {
		case trailReviewStatusResolved:
			counts.Resolved++
		case trailReviewStatusDismissed:
			counts.Dismissed++
		case trailReviewStatusOpen:
			counts.Open++
			switch strings.ToLower(stringPtrValue(comment.Severity)) {
			case "high":
				counts.OpenHigh++
			case "medium":
				counts.OpenMedium++
			case "low":
				counts.OpenLow++
			}
		}
		if comment.StaleOutcome == "stale" {
			counts.Stale++
		}
	}
	return counts
}

func filterCommentsBySeverity(comments []api.TrailReviewComment, severity string) []api.TrailReviewComment {
	var out []api.TrailReviewComment
	for _, comment := range comments {
		got := strings.ToLower(stringPtrValue(comment.Severity))
		if severity == "" {
			if got != "high" && got != "medium" && got != "low" {
				out = append(out, comment)
			}
			continue
		}
		if got == severity {
			out = append(out, comment)
		}
	}
	return out
}

func trailReviewLocationDisplay(loc api.TrailReviewLocation) string {
	file := stringPtrValue(loc.FilePath)
	if file == "" {
		return loc.Granularity
	}
	if loc.StartLine == nil {
		return file
	}
	if loc.EndLine != nil && *loc.EndLine != *loc.StartLine {
		return fmt.Sprintf("%s:%d-%d", file, *loc.StartLine, *loc.EndLine)
	}
	return fmt.Sprintf("%s:%d", file, *loc.StartLine)
}

func trailReviewCommentTitle(comment api.TrailReviewComment) string {
	if title := strings.TrimSpace(stringPtrValue(comment.Title)); title != "" {
		return title
	}
	body := strings.TrimSpace(stringPtrValue(comment.Body))
	if body == "" {
		return "(untitled finding)"
	}
	return truncateOneLine(body, 80)
}

func severityTitle(severity string) string {
	switch severity {
	case "high":
		return "High"
	case "medium":
		return "Medium"
	case "low":
		return "Low"
	default:
		return "Other"
	}
}

func severityDisplay(severity *string) string {
	if severity == nil || strings.TrimSpace(*severity) == "" {
		return "-"
	}
	return *severity
}

func severityInitial(severity *string) string {
	switch strings.ToLower(stringPtrValue(severity)) {
	case "high":
		return "H"
	case "medium":
		return "M"
	case "low":
		return "L"
	default:
		return "-"
	}
}

func defaultTrailReviewStatusReason(status string) string {
	switch status {
	case trailReviewStatusResolved:
		return "Resolved via Entire CLI"
	case trailReviewStatusDismissed:
		return "Dismissed via Entire CLI"
	case trailReviewStatusOpen:
		return "Reopened via Entire CLI"
	default:
		return "Updated via Entire CLI"
	}
}

func parseOptionalTrailNumber(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid trail number %q", args[0])
	}
	return n, nil
}

func parseTrailNumberAndCommentID(args []string) (int, string, error) {
	if len(args) == 1 {
		return 0, args[0], nil
	}
	n, err := parseOptionalTrailNumber(args[:1])
	if err != nil {
		return 0, "", err
	}
	return n, args[1], nil
}

func optionalStringPtr(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func stringPtr(s string) *string {
	return &s
}

func stringPtrValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optionalStringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func abbreviateMaybe(s string, n int) string {
	if len(s) <= n || n <= 0 {
		return s
	}
	return s[:n]
}

func truncateOneLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
