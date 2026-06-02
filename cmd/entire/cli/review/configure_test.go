package review

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestModelInList(t *testing.T) {
	models := []agent.ModelInfo{{ID: "opus"}, {ID: "sonnet"}}
	if !modelInList("opus", models) {
		t.Error("expected opus to be in list")
	}
	if modelInList("gpt-5", models) {
		t.Error("gpt-5 should not be in list")
	}
}

func configureTestDeps(adapter ...string) Deps {
	set := map[string]struct{}{}
	for _, a := range adapter {
		set[a] = struct{}{}
	}
	return Deps{
		ReviewerFor: func(name string) reviewtypes.AgentReviewer {
			if _, ok := set[name]; ok {
				return &stubReviewer{name: name}
			}
			return nil
		},
	}
}

func TestBuildConfiguredProfile_FromFlags(t *testing.T) {
	deps := configureTestDeps("claude-code", "codex")
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{
			Agents: []string{"claude-code", "codex"},
			Master: "codex",
			Models: []string{"claude-code=opus"},
		},
		&settings.EntireSettings{},
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if len(profile.Agents) != 2 {
		t.Fatalf("agents = %d, want 2: %#v", len(profile.Agents), profile.Agents)
	}
	if got := profile.Agents["claude-code"].Model; got != "opus" {
		t.Errorf("claude-code model = %q, want opus", got)
	}
	if profile.Master != "codex" {
		t.Errorf("master = %q, want codex", profile.Master)
	}
	if profile.Task == "" {
		t.Error("task should default to the built-in general task")
	}
}

func TestBuildConfiguredProfile_RejectsNonAdapterAgent(t *testing.T) {
	deps := configureTestDeps("claude-code")
	_, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{"cursor"}},
		&settings.EntireSettings{},
		deps,
	)
	if err == nil {
		t.Fatal("expected error for agent without a review-runner adapter")
	}
}

func TestBuildConfiguredProfile_PreservesExistingTaskAndMasterModel(t *testing.T) {
	deps := configureTestDeps("claude-code", "codex")
	s := &settings.EntireSettings{
		ReviewProfiles: map[string]settings.ReviewProfileConfig{
			"general": {
				Task:        "Custom task text.",
				MasterModel: "opus",
				Agents: map[string]settings.ReviewConfig{
					"claude-code": {Skills: []string{"/review"}},
				},
				Master: "claude-code",
			},
		},
	}
	// Only change the worker set; task + master_model must survive.
	profile, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{"claude-code", "codex"}, Master: "claude-code"},
		s,
		deps,
	)
	if err != nil {
		t.Fatalf("buildConfiguredProfile: %v", err)
	}
	if profile.Task != "Custom task text." {
		t.Errorf("task = %q, want preserved custom task", profile.Task)
	}
	if profile.MasterModel != "opus" {
		t.Errorf("master_model = %q, want preserved opus", profile.MasterModel)
	}
}

func TestBuildConfiguredProfile_InvalidModelSpec(t *testing.T) {
	deps := configureTestDeps("claude-code")
	_, err := buildConfiguredProfile(
		context.Background(),
		"general",
		reviewConfigureOptions{Agents: []string{"claude-code"}, Models: []string{"no-equals"}},
		&settings.EntireSettings{},
		deps,
	)
	if err == nil {
		t.Fatal("expected error for malformed --set-model spec")
	}
}
