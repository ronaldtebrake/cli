package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestReviewTrailFindingInput(t *testing.T) {
	// Regression: a review verdict is not tied to a file/line, so the finding
	// must use whole_change granularity. An empty granularity is rejected by
	// the API with a 400.
	in := reviewTrailFindingInput("general", "  the verdict  ")
	if in.Location.Granularity != "whole_change" {
		t.Errorf("granularity = %q, want whole_change", in.Location.Granularity)
	}
	if in.ClientID == "" {
		t.Error("client id (idempotency key) should be set")
	}
	if in.Body == nil || !strings.Contains(*in.Body, "the verdict") || !strings.Contains(*in.Body, "general") {
		t.Errorf("body = %v, want it to include the profile and (trimmed) verdict", in.Body)
	}

	// No profile: the body is exactly the trimmed verdict.
	bare := reviewTrailFindingInput("", "  bare verdict  ")
	if bare.Body == nil || *bare.Body != "bare verdict" {
		t.Errorf("body = %v, want exactly %q", bare.Body, "bare verdict")
	}
	if bare.Location.Granularity != "whole_change" {
		t.Errorf("granularity = %q, want whole_change", bare.Location.Granularity)
	}
}

func TestTrailWebURL(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "https://entire.io")

	cases := []struct {
		name   string
		target trailReviewTarget
		want   string
	}{
		{
			name: "full target",
			target: trailReviewTarget{
				Host:  "gh",
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Number: 466, Branch: "review-profiles"},
			},
			want: "https://entire.io/gh/entireio/cli/trails/466/review-profiles",
		},
		{
			name: "no trail number yields no link",
			target: trailReviewTarget{
				Host:  "gh",
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Branch: "review-profiles"},
			},
			want: "",
		},
		{
			name: "missing forge yields no link",
			target: trailReviewTarget{
				Owner: "entireio",
				Repo:  "cli",
				Trail: api.TrailResource{Number: 1, Branch: "main"},
			},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trailWebURL(c.target); got != c.want {
				t.Errorf("trailWebURL() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTrailWebURL_HonorsCustomBase(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "https://entire.example.com/")
	target := trailReviewTarget{
		Host:  "gh",
		Owner: "acme",
		Repo:  "app",
		Trail: api.TrailResource{Number: 7, Branch: "feat/x"},
	}
	want := "https://entire.example.com/gh/acme/app/trails/7/feat/x"
	if got := trailWebURL(target); got != want {
		t.Errorf("trailWebURL() = %q, want %q", got, want)
	}
}
