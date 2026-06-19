package cli

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const testWholeChangeGranularity = "whole_change"

func TestReviewTrailFindingInput(t *testing.T) {
	// Regression: a review verdict is not tied to a file/line, so the finding
	// must use whole_change granularity. An empty granularity is rejected by
	// the API with a 400.
	in := reviewTrailFindingInputWithKind("general", "  the verdict  ", "verdict")
	if in.Location.Granularity != testWholeChangeGranularity {
		t.Errorf("granularity = %q, want whole_change", in.Location.Granularity)
	}
	if in.ClientID == "" {
		t.Error("client id (idempotency key) should be set")
	}
	if in.Body == nil || !strings.Contains(*in.Body, "the verdict") || !strings.Contains(*in.Body, "general") {
		t.Errorf("body = %v, want it to include the profile and (trimmed) verdict", in.Body)
	}

	// No profile: the body is exactly the trimmed verdict.
	bare := reviewTrailFindingInputWithKind("", "  bare verdict  ", "verdict")
	if bare.Body == nil || *bare.Body != "bare verdict" {
		t.Errorf("body = %v, want exactly %q", bare.Body, "bare verdict")
	}
	if bare.Location.Granularity != testWholeChangeGranularity {
		t.Errorf("granularity = %q, want whole_change", bare.Location.Granularity)
	}
}

func TestReviewTrailFindingInputsSplitsTopLevelBullets(t *testing.T) {
	verdict := `REQUEST CHANGES - multiple issues.

- **[P1] First issue:** fix sandbox-runs-queue.ts:1144
  with continuation detail
- **[P2] Second issue:** fix daytona.ts:919
  - nested detail stays with second
- **[Low] Third issue:** remove the note from daytona-command-native-plan.md:1`

	inputs := reviewTrailFindingInputs("general", verdict)
	if len(inputs) != 3 {
		t.Fatalf("inputs = %d, want 3", len(inputs))
	}
	bodies := make([]string, len(inputs))
	for i, in := range inputs {
		if in.Location.Granularity != "line" {
			t.Fatalf("input %d granularity = %q, want line", i, in.Location.Granularity)
		}
		if in.Location.FilePath == nil || in.Location.StartLine == nil {
			t.Fatalf("input %d missing inferred line location: %+v", i, in.Location)
		}
		if in.Severity == nil {
			t.Fatalf("input %d missing inferred severity", i)
		}
		if in.ClientID == "" {
			t.Fatalf("input %d missing client id", i)
		}
		if in.Body == nil {
			t.Fatalf("input %d body is nil", i)
		}
		bodies[i] = *in.Body
		if !strings.Contains(bodies[i], "Review finding (profile: general)") {
			t.Fatalf("body %d missing finding/profile header: %q", i, bodies[i])
		}
	}
	if !strings.Contains(bodies[0], "First issue") || !strings.Contains(bodies[0], "with continuation detail") {
		t.Fatalf("first body did not preserve first finding: %q", bodies[0])
	}
	if !strings.Contains(bodies[1], "Second issue") || !strings.Contains(bodies[1], "nested detail stays with second") {
		t.Fatalf("second body did not preserve nested detail: %q", bodies[1])
	}
	if strings.Contains(bodies[0], "Second issue") || strings.Contains(bodies[1], "Third issue") {
		t.Fatalf("bodies were not split cleanly: %#v", bodies)
	}
}

func TestReviewTrailFindingInputsAcceptsRunnerStyleJSONLastLine(t *testing.T) {
	verdict := `Intermediate prose that should be ignored for posting.
{"summary":"","comments":[{"severity":"high","confidence":0.92,"body":"` + "`" + `daytona.ts` + "`" + ` rejects public repos without a token; allow public clones or mint a token.","location":{"granularity":"line","file_path":"daytona.ts","start_line":901}},{"severity":"P2","confidence":0.7,"body":"Delete failures are swallowed, orphaning provider snapshots.","location":{"granularity":"range","file_path":"daytona.ts","start_line":966,"end_line":970}}]}`

	inputs := reviewTrailFindingInputs("general", verdict)
	if len(inputs) != 2 {
		t.Fatalf("inputs = %d, want 2", len(inputs))
	}
	if inputs[0].Body == nil || strings.Contains(*inputs[0].Body, "Review finding") {
		t.Fatalf("structured JSON body should be the native comment body, got %v", inputs[0].Body)
	}
	if inputs[0].Severity == nil || *inputs[0].Severity != "high" {
		t.Fatalf("severity[0] = %v, want high", inputs[0].Severity)
	}
	if inputs[0].Confidence == nil || *inputs[0].Confidence != 0.92 {
		t.Fatalf("confidence[0] = %v, want 0.92", inputs[0].Confidence)
	}
	if inputs[0].Location.Granularity != "line" || inputs[0].Location.FilePath == nil || *inputs[0].Location.FilePath != "daytona.ts" || inputs[0].Location.StartLine == nil || *inputs[0].Location.StartLine != 901 {
		t.Fatalf("location[0] = %+v, want daytona.ts:901", inputs[0].Location)
	}
	if inputs[1].Severity == nil || *inputs[1].Severity != "medium" {
		t.Fatalf("severity[1] = %v, want normalized medium", inputs[1].Severity)
	}
	if inputs[1].Location.Granularity != "range" || inputs[1].Location.EndLine == nil || *inputs[1].Location.EndLine != 970 {
		t.Fatalf("location[1] = %+v, want range ending 970", inputs[1].Location)
	}
}

func TestReviewTrailFindingInputsSingleVerdictUnchanged(t *testing.T) {
	inputs := reviewTrailFindingInputs("general", "APPROVE - no actionable findings.")
	if len(inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(inputs))
	}
	if inputs[0].Body == nil || !strings.Contains(*inputs[0].Body, "Review verdict (profile: general)") {
		t.Fatalf("single body = %v, want verdict/profile header", inputs[0].Body)
	}
}

func TestSplitReviewVerdictFindingsNumberedList(t *testing.T) {
	items := splitReviewVerdictFindings("Verdict\n\n1. First\n2. Second")
	if len(items) != 2 || items[0] != "First" || items[1] != "Second" {
		t.Fatalf("items = %#v, want numbered findings", items)
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
