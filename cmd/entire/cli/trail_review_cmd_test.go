package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestTrailReviewCommentsPath(t *testing.T) {
	got := trailReviewCommentsPath("trail id/with slash", trailReviewListOptions{
		Status:           "open,resolved",
		Severity:         "high,medium",
		Stale:            "any",
		IncludeDismissed: true,
		Limit:            25,
		Offset:           50,
	})
	want := "/api/v1/trails/trail%20id%2Fwith%20slash/reviews/comments?include_dismissed=true&limit=25&offset=50&severity=high%2Cmedium&status=open%2Cresolved"
	if got != want {
		t.Fatalf("trailReviewCommentsPath = %q, want %q", got, want)
	}
}

func TestPrintTrailReviewDashboard(t *testing.T) {
	high := "high"
	medium := "medium"
	path := "src/auth/session.ts"
	line := 88
	comments := []api.TrailReviewComment{
		{
			ID:       "comment-high-123",
			ReviewID: "review-1",
			Title:    trailReviewStrPtr("Missing expiry skew handling"),
			Severity: &high,
			Status:   trailReviewStatusOpen,
			Location: api.TrailReviewLocation{
				Granularity: "line",
				FilePath:    &path,
				StartLine:   &line,
			},
		},
		{
			ID:       "comment-medium-123",
			ReviewID: "review-1",
			Title:    trailReviewStrPtr("Retry loop can spin forever"),
			Severity: &medium,
			Status:   trailReviewStatusResolved,
			Location: api.TrailReviewLocation{Granularity: "whole_change"},
		},
	}
	var out strings.Builder
	printTrailReviewDashboard(&out, trailReviewTarget{Trail: api.TrailResource{
		ID:     "trl_1",
		Number: 42,
		Title:  "Add token refresh",
		Status: "in_review",
		Branch: "feat/token-refresh",
		Base:   "main",
	}}, comments, false, defaultTrailReviewListOptions())
	text := out.String()
	for _, want := range []string{
		"Trail #42  Add token refresh",
		"Open findings: 1  high 1  medium 0  low 0",
		"Resolved: 1",
		"High",
		"src/auth/session.ts:88",
		"Missing expiry skew handling",
		"Actions:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, text)
		}
	}
}

func TestFetchTrailReviewCommentsAndPatchStatus(t *testing.T) {
	var gotPatchBody api.TrailReviewCommentPatchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trails/trl_1/reviews/comments":
			if got := r.URL.Query().Get("status"); got != "open" {
				t.Fatalf("status query = %q, want open", got)
			}
			_ = json.NewEncoder(w).Encode(api.TrailReviewCommentsResponse{Comments: []api.TrailReviewComment{
				{ID: "cmt_1", TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusOpen, Location: api.TrailReviewLocation{Granularity: "whole_change"}},
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/trails/trl_1/reviews/rvw_1/comments/cmt_1":
			if err := json.NewDecoder(r.Body).Decode(&gotPatchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(api.TrailReviewComment{ID: "cmt_1", TrailID: "trl_1", ReviewID: "rvw_1", Status: trailReviewStatusResolved})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	comments, hasMore, err := fetchTrailReviewComments(context.Background(), client, "trl_1", defaultTrailReviewListOptions())
	if err != nil {
		t.Fatalf("fetchTrailReviewComments: %v", err)
	}
	if hasMore || len(comments) != 1 || comments[0].ID != "cmt_1" {
		t.Fatalf("comments = %#v, hasMore=%v", comments, hasMore)
	}
	updated, err := patchTrailReviewCommentStatus(context.Background(), client, "trl_1", comments[0], trailReviewStatusResolved, "fixed")
	if err != nil {
		t.Fatalf("patchTrailReviewCommentStatus: %v", err)
	}
	if updated.Status != trailReviewStatusResolved {
		t.Fatalf("updated status = %q", updated.Status)
	}
	if gotPatchBody.Status != trailReviewStatusResolved || gotPatchBody.StatusReason == nil || *gotPatchBody.StatusReason != "fixed" {
		t.Fatalf("patch body = %#v", gotPatchBody)
	}
}

func TestStartTrailReviewSendsIdempotencyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/trails/trl_1/reviews" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if got := r.Header.Get("Idempotency-Key"); got != "key-1" {
			t.Fatalf("Idempotency-Key = %q, want key-1", got)
		}
		var body api.TrailReviewStartRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.HeadSHA == nil || *body.HeadSHA != "abc" {
			t.Fatalf("HeadSHA = %#v", body.HeadSHA)
		}
		_ = json.NewEncoder(w).Encode(api.TrailReviewStartResponse{ReviewID: "rvw_1", TrailID: "trl_1", CodeVersionID: "cv_1"})
	}))
	defer srv.Close()
	t.Setenv(api.BaseURLEnvVar, srv.URL)
	client := api.NewClient("tok")

	started, err := startTrailReview(context.Background(), client, "trl_1", api.TrailReviewStartRequest{HeadSHA: trailReviewStrPtr("abc")}, "key-1")
	if err != nil {
		t.Fatalf("startTrailReview: %v", err)
	}
	if started.ReviewID != "rvw_1" {
		t.Fatalf("ReviewID = %q", started.ReviewID)
	}
}

func trailReviewStrPtr(s string) *string { return &s }
