package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const DefaultProfileName = "general"

const (
	defaultGeneralTask       = "Review this change for correctness, regressions, API design, missing tests, maintainability, and user-facing behavior changes. Report only actionable findings with concrete evidence."
	defaultSecurityTask      = "Review this change for security vulnerabilities: authentication and authorization bugs, injection risks, secrets exposure, unsafe dependency or deserialization behavior, privilege-boundary mistakes, insecure defaults, and data leakage. Report only actionable findings with concrete evidence."
	defaultAccessibilityTask = "Review this change for accessibility regressions: keyboard navigation, focus management, semantic markup, labels, ARIA correctness, color contrast, reduced-motion behavior, screen-reader behavior, and inclusive error states. Report only actionable findings with concrete evidence."
)

// profileTask returns the configured task, or a built-in task for conventional
// profile names when the config leaves task empty.
func profileTask(name string, cfg settings.ReviewProfileConfig) string {
	if strings.TrimSpace(cfg.Task) != "" {
		return strings.TrimSpace(cfg.Task)
	}
	switch strings.ToLower(name) {
	case "", DefaultProfileName:
		return defaultGeneralTask
	case "security":
		return defaultSecurityTask
	case "accessibility", "a11y":
		return defaultAccessibilityTask
	default:
		return defaultGeneralTask
	}
}

// selectReviewProfile resolves the profile to run. No legacy fallback is used:
// users must configure review_profiles (the command is experimental, so there
// is intentionally no migration from the old review map).
func selectReviewProfile(s *settings.EntireSettings, override string) (string, settings.ReviewProfileConfig, error) {
	if s == nil || len(s.ReviewProfiles) == 0 {
		return "", settings.ReviewProfileConfig{}, errors.New("no review profiles configured; run `entire review --edit` or add review_profiles to Entire preferences")
	}
	profiles := nonZeroProfiles(s.ReviewProfiles)
	if len(profiles) == 0 {
		return "", settings.ReviewProfileConfig{}, errors.New("no review profiles configured; every profile is empty")
	}

	name := strings.TrimSpace(override)
	if name == "" {
		name = strings.TrimSpace(s.ReviewDefaultProfile)
	}
	if name == "" {
		if _, ok := profiles[DefaultProfileName]; ok {
			name = DefaultProfileName
		} else if len(profiles) == 1 {
			for only := range profiles {
				name = only
			}
		} else {
			return "", settings.ReviewProfileConfig{}, fmt.Errorf(
				"multiple review profiles configured (%s); pass a profile name or set review_default_profile",
				strings.Join(sortedProfileNames(profiles), ", "))
		}
	}

	cfg, ok := profiles[name]
	if !ok {
		return "", settings.ReviewProfileConfig{}, fmt.Errorf(
			"review profile %q is not configured; configured profiles: %s",
			name, strings.Join(sortedProfileNames(profiles), ", "))
	}
	if len(nonZeroAgentConfigs(cfg.Agents)) == 0 {
		return "", settings.ReviewProfileConfig{}, fmt.Errorf("review profile %q has no configured agents", name)
	}
	return name, cfg, nil
}

func nonZeroProfiles(in map[string]settings.ReviewProfileConfig) map[string]settings.ReviewProfileConfig {
	out := make(map[string]settings.ReviewProfileConfig, len(in))
	for name, cfg := range in {
		name = strings.TrimSpace(name)
		if name == "" || cfg.IsZero() {
			continue
		}
		out[name] = cfg
	}
	return out
}

func sortedProfileNames(in map[string]settings.ReviewProfileConfig) []string {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func nonZeroAgentConfigs(in map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	out := make(map[string]settings.ReviewConfig, len(in))
	for name, cfg := range in {
		name = strings.TrimSpace(name)
		if name == "" || cfg.IsZero() {
			continue
		}
		out[name] = cfg
	}
	return out
}

func sortedProfileAgentNames(profile settings.ReviewProfileConfig) []string {
	names := make([]string, 0, len(profile.Agents))
	for name := range profile.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func defaultReviewProfileForInstalledAgents(
	ctx context.Context,
	profileName string,
	installed []types.AgentName,
	reviewerFor func(string) reviewtypes.AgentReviewer,
) (settings.ReviewProfileConfig, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		profileName = DefaultProfileName
	}
	installedNames := make([]string, 0, len(installed))
	for _, name := range installed {
		installedNames = append(installedNames, string(name))
	}
	sort.Strings(installedNames)

	agents := make(map[string]settings.ReviewConfig, len(installedNames))
	for _, name := range installedNames {
		if reviewerFor != nil && reviewerFor(name) == nil {
			continue
		}
		cfg := defaultReviewAgentConfig(profileName, name)
		if cfg.IsZero() {
			continue
		}
		agents[name] = cfg
	}
	if len(agents) == 0 {
		return settings.ReviewProfileConfig{}, errors.New("no launchable agents with hooks installed; run `entire configure --agent claude-code`, `entire configure --agent codex`, or `entire configure --agent gemini`")
	}
	return settings.ReviewProfileConfig{
		Task:   profileTask(profileName, settings.ReviewProfileConfig{}),
		Agents: agents,
		Master: defaultReviewMaster(ctx, agents),
	}, nil
}

func defaultReviewAgentConfig(profileName, agentName string) settings.ReviewConfig {
	focus := defaultProfileFocus(profileName)
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		if strings.EqualFold(profileName, "security") {
			return settings.ReviewConfig{Skills: []string{"/security-review"}}
		}
		return settings.ReviewConfig{Skills: []string{"/review"}, Prompt: focus}
	case string(agent.AgentNameCodex):
		return settings.ReviewConfig{Skills: []string{"/review"}, Prompt: focus}
	case string(agent.AgentNameGemini):
		prompt := "Review the change according to the profile task."
		if focus != "" {
			prompt += " " + focus
		}
		return settings.ReviewConfig{Prompt: prompt}
	default:
		return settings.ReviewConfig{}
	}
}

func defaultProfileFocus(profileName string) string {
	switch strings.ToLower(strings.TrimSpace(profileName)) {
	case "security":
		return "Focus specifically on security issues."
	case "accessibility", "a11y":
		return "Focus specifically on accessibility issues."
	default:
		return ""
	}
}

func defaultReviewMaster(ctx context.Context, configured map[string]settings.ReviewConfig) string {
	for _, preferred := range []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameCodex), string(agent.AgentNameGemini)} {
		if _, ok := configured[preferred]; ok && agentSupportsTextGeneration(ctx, preferred) {
			return preferred
		}
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if agentSupportsTextGeneration(ctx, name) {
			return name
		}
	}
	return ""
}

func agentSupportsTextGeneration(_ context.Context, name string) bool {
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		return false
	}
	_, ok := agent.AsTextGenerator(ag)
	return ok
}

func saveDefaultReviewProfile(ctx context.Context, profileName string, profile settings.ReviewProfileConfig) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load review preferences before save: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}
	if prefs.ReviewProfiles == nil {
		prefs.ReviewProfiles = map[string]settings.ReviewProfileConfig{}
	}
	prefs.ReviewProfiles[profileName] = profile
	if prefs.ReviewDefaultProfile == "" {
		prefs.ReviewDefaultProfile = profileName
	}
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save review preferences: %w", err)
	}
	return nil
}
