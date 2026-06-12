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
		return "", settings.ReviewProfileConfig{}, errors.New("no crew profiles configured; run `entire inspect --configure` or add review_profiles to Entire preferences")
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

func reviewAgentName(workerName string, cfg settings.ReviewConfig) string {
	if strings.TrimSpace(cfg.Agent) != "" {
		return strings.TrimSpace(cfg.Agent)
	}
	return strings.TrimSpace(workerName)
}

func reviewWorkerLabel(workerName string, cfg settings.ReviewConfig) string {
	agentName := reviewAgentName(workerName, cfg)
	parts := []string{workerName}
	var details []string
	if agentName != "" && agentName != workerName {
		details = append(details, agentName)
	}
	if strings.TrimSpace(cfg.Model) != "" {
		details = append(details, "model "+strings.TrimSpace(cfg.Model))
	}
	if len(details) > 0 {
		parts = append(parts, "  ("+strings.Join(details, ", ")+")")
	}
	return strings.Join(parts, "")
}

// profileMasterIdentity resolves the master's agent name and model, and reports
// whether the profile has a master at all. A standalone master (MasterAgent
// set) wins and need not be one of the workers; otherwise the legacy worker
// master (Master keying into Agents) is resolved.
func profileMasterIdentity(profile settings.ReviewProfileConfig) (string, string, bool) {
	if ma := strings.TrimSpace(profile.MasterAgent); ma != "" {
		return ma, strings.TrimSpace(profile.MasterModel), true
	}
	workerName, cfg, err := selectProfileWorker(profile, profile.Master)
	if err != nil {
		return "", "", false
	}
	model := strings.TrimSpace(profile.MasterModel)
	if model == "" {
		model = strings.TrimSpace(cfg.Model)
	}
	return reviewAgentName(workerName, cfg), model, true
}

func resolveProfileMaster(profile settings.ReviewProfileConfig) (string, string) {
	agentName, model, _ := profileMasterIdentity(profile)
	return agentName, model
}

// masterDisplayLabel renders the master for UI/dump output: "agent · model" for
// a standalone master, or the worker label for a legacy worker master.
func masterDisplayLabel(profile settings.ReviewProfileConfig, agentName, model string) string {
	if strings.TrimSpace(profile.MasterAgent) != "" {
		if strings.TrimSpace(model) != "" {
			return labelForSimpleAgent(agentName) + " · " + model
		}
		return labelForSimpleAgent(agentName)
	}
	if workerName, cfg, err := selectProfileWorker(profile, profile.Master); err == nil {
		return reviewWorkerLabel(workerName, cfg)
	}
	return agentName
}

func selectProfileWorker(profile settings.ReviewProfileConfig, selector string) (string, settings.ReviewConfig, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "", settings.ReviewConfig{}, errors.New("empty review worker selector")
	}
	if cfg, ok := profile.Agents[selector]; ok && !cfg.IsZero() {
		return selector, cfg, nil
	}
	var matches []string
	for workerName, cfg := range profile.Agents {
		if cfg.IsZero() {
			continue
		}
		if reviewAgentName(workerName, cfg) == selector {
			matches = append(matches, workerName)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 1:
		return matches[0], profile.Agents[matches[0]], nil
	case 0:
		configured := sortedProfileAgentNames(profile)
		if len(configured) == 0 {
			return "", settings.ReviewConfig{}, fmt.Errorf("review worker or agent %q is not configured", selector)
		}
		return "", settings.ReviewConfig{}, fmt.Errorf("review worker or agent %q is not configured; configured workers: %s", selector, strings.Join(configured, ", "))
	default:
		return "", settings.ReviewConfig{}, fmt.Errorf("agent %q has multiple review workers (%s); choose one by worker name", selector, strings.Join(matches, ", "))
	}
}

func workerIDForAgentModel(agentName, model string, existing map[string]settings.ReviewConfig) string {
	base := strings.TrimSpace(agentName)
	if strings.TrimSpace(model) != "" {
		base += ":" + sanitizeWorkerIDPart(model)
	}
	if base == "" {
		base = "worker"
	}
	candidate := base
	for i := 2; ; i++ {
		if _, exists := existing[candidate]; !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func sanitizeWorkerIDPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		keep := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if keep {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "model"
	}
	return out
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
		return settings.ReviewProfileConfig{}, errors.New("no agents with review runner adapters and hooks installed; run `entire configure --agent claude-code`, `entire configure --agent codex`, `entire configure --agent gemini`, or `entire configure --agent pi`")
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
	case string(agent.AgentNameGemini), string(agent.AgentNamePi):
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
	for _, preferred := range []string{string(agent.AgentNameClaudeCode), string(agent.AgentNameCodex), string(agent.AgentNameGemini), string(agent.AgentNamePi)} {
		for _, workerName := range sortedReviewConfigKeys(configured) {
			cfg := configured[workerName]
			if reviewAgentName(workerName, cfg) == preferred && agentSupportsTextGeneration(ctx, preferred) {
				return workerName
			}
		}
	}
	for _, workerName := range sortedReviewConfigKeys(configured) {
		if agentSupportsTextGeneration(ctx, reviewAgentName(workerName, configured[workerName])) {
			return workerName
		}
	}
	return ""
}

func sortedReviewConfigKeys(configured map[string]settings.ReviewConfig) []string {
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	return saveReviewProfile(ctx, profileName, profile, false)
}

func saveReviewProfile(ctx context.Context, profileName string, profile settings.ReviewProfileConfig, makeDefault bool) error {
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
	if makeDefault || prefs.ReviewDefaultProfile == "" {
		prefs.ReviewDefaultProfile = profileName
	}
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save review preferences: %w", err)
	}
	return nil
}
