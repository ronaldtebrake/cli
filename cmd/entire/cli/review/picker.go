// Package review — see env.go for package-level rationale.
//
// picker.go implements the interactive review skills picker and agent selection
// helpers. pickConfig presents a huh multi-select per installed agent and saves
// the selection to clone-local review preferences.
package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/skilldiscovery"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// AgentChoice is one row in the spawn-time picker. Name is the agent
// registry key (used for marker/override); Label is the picker-visible
// string ("<name>   (N skills configured)" or "<name>   (prompt-only)").
type AgentChoice struct {
	Name  string
	Label string
}

// newAccessibleForm creates a huh form with Entire's standard theme,
// switching to accessibility mode when ACCESSIBLE is set. Thin wrapper
// around uiform.New preserved so existing call sites don't change.
func newAccessibleForm(groups ...*huh.Group) *huh.Form {
	return uiform.New(groups...)
}

// ConfirmFirstRunSetup prints a banner framing the picker as first-run
// setup (rather than the review itself) and waits for the user to confirm.
// Returns false if the user cancels; caller should bail gracefully.
//
// Signposting matters here because `entire review` with no config silently
// drops into the picker — users running the command to start a review can
// mistake the picker for the review. The banner + confirmation makes the
// setup phase explicit, and the trailing "running review now" line in the
// caller closes the loop on what comes next.
func ConfirmFirstRunSetup(ctx context.Context, out io.Writer) bool {
	fmt.Fprintln(out, "No review profiles found — let's set one up first.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "You'll choose a review type and worker agents. They're saved to")
	fmt.Fprintln(out, "local review preferences; configure later with `entire inspect --configure`.")
	fmt.Fprintln(out, "After setup, you can start the review immediately.")
	fmt.Fprintln(out)

	proceed := true
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Set up review now?").
			Affirmative("Yes").
			Negative("Cancel").
			Value(&proceed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		fmt.Fprintln(out, "Setup cancelled.")
		return false
	}
	if !proceed {
		fmt.Fprintln(out, "Setup cancelled.")
	}
	return proceed
}

// RunReviewGuidedSetup is the simple config path for `entire review`.
// It intentionally avoids the per-agent skills picker: users choose the review
// profile and worker agents, then Entire fills in opinionated per-agent
// defaults. Advanced skill-level editing remains available via --edit.
func RunReviewGuidedSetup(
	ctx context.Context,
	out io.Writer,
	installed []types.AgentName,
	reviewerFor func(string) reviewtypes.AgentReviewer,
	profileName string,
	firstRun bool,
	s *settings.EntireSettings,
) (string, settings.ReviewProfileConfig, error) {
	if firstRun {
		if !ConfirmFirstRunSetup(ctx, out) {
			return "", settings.ReviewProfileConfig{}, ErrPickerCancelled
		}
	}

	launchable := launchableInstalledAgentNames(installed, reviewerFor)
	if len(launchable) == 0 {
		return "", settings.ReviewProfileConfig{}, errors.New("no agents with review runner adapters and hooks installed; run `entire configure --agent claude-code`, `entire configure --agent codex`, or `entire configure --agent gemini`")
	}

	profileName = strings.TrimSpace(profileName)
	profileWasProvided := profileName != ""
	if profileName == "" {
		profileName = DefaultProfileName
	}
	currentDefault := ""
	if s != nil {
		currentDefault = strings.TrimSpace(s.ReviewDefaultProfile)
	}
	customTask := ""
	if !profileWasProvided {
		pickedProfile, pickedTask, err := promptForReviewFocus(ctx, currentDefault)
		if err != nil {
			return "", settings.ReviewProfileConfig{}, err
		}
		profileName = pickedProfile
		customTask = pickedTask
	}

	// Seed the flow from the existing profile (if any) so re-configuring edits
	// the current crew/master rather than starting from scratch.
	var existing settings.ReviewProfileConfig
	if s != nil {
		existing = s.ReviewProfiles[profileName]
	}
	existing.Agents = nonZeroAgentConfigs(existing.Agents)

	profile, err := promptForReviewCrew(ctx, profileName, launchable, existing)
	if err != nil {
		return "", settings.ReviewProfileConfig{}, err
	}
	if customTask != "" {
		profile.Task = customTask
	}
	if len(profile.Agents) > 1 {
		masterAgent, masterModel, err := promptForStandaloneMaster(ctx, launchable, existing)
		if err != nil {
			return "", settings.ReviewProfileConfig{}, err
		}
		profile.MasterAgent = masterAgent
		profile.MasterModel = masterModel
		profile.Master = ""
	}
	fmt.Fprintf(out, "Saved %q review profile with %s.\n", profileName, strings.Join(sortedProfileAgentNames(profile), ", "))
	fmt.Fprintln(out)
	return profileName, profile, nil
}

// RunReviewConfigPicker presents a huh multi-select for each installed agent
// that has curated review skills, and saves the selection to
// clone-local review preferences. Previously-saved skills are pre-checked via
// huh.Option.Selected(true), mirroring how `entire enable` preserves prior
// selections in its own agent picker.
//
// getInstalled is injected to avoid an import cycle with the cli package.
func launchableInstalledAgentNames(installed []types.AgentName, reviewerFor func(string) reviewtypes.AgentReviewer) []string {
	names := make([]string, 0, len(installed))
	for _, name := range installed {
		if reviewerFor != nil && reviewerFor(string(name)) == nil {
			continue
		}
		names = append(names, string(name))
	}
	sort.Strings(names)
	return names
}

// customProfileName is the profile name used when the user writes a custom
// task in the focus picker.
const customProfileName = "custom"

// reviewFocusCustomSentinel is the focus-picker option value for "write your
// own task". Distinct from real profile names.
const reviewFocusCustomSentinel = "__custom_task__"

// promptForReviewFocus asks what the crew should review. The presets set a
// named profile (task + default skills); "Custom…" lets the user write the
// shared task directly. Returns (profileName, customTask); customTask is
// non-empty only for the custom path. current pre-selects the profile being
// edited.
func promptForReviewFocus(ctx context.Context, current string) (string, string, error) {
	current = strings.TrimSpace(current)
	picked := DefaultProfileName
	presets := []struct{ label, value string }{
		{"General — correctness, regressions, tests", DefaultProfileName},
		{"Security — auth, injection, secrets", "security"},
		{"Accessibility — keyboard, screen readers, contrast", "accessibility"},
	}
	options := make([]huh.Option[string], 0, len(presets)+1)
	for _, p := range presets {
		label := p.label
		if p.value == current {
			label += "  (current)"
			picked = p.value // pre-select the profile being edited
		}
		options = append(options, huh.NewOption(label, p.value))
	}
	customLabel := "Custom… — describe your own task"
	if current == customProfileName {
		customLabel += "  (current)"
		picked = reviewFocusCustomSentinel
	}
	options = append(options, huh.NewOption(customLabel, reviewFocusCustomSentinel))

	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("What should they review?").
			Options(options...).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", "", fmt.Errorf("review focus picker: %w", err)
	}
	if picked != reviewFocusCustomSentinel {
		return picked, "", nil
	}

	task := ""
	taskForm := newAccessibleForm(huh.NewGroup(
		huh.NewText().
			Title("Describe the review task").
			Description("What should the crew look for? This becomes the shared task for every worker.").
			Value(&task),
	))
	if err := taskForm.RunWithContext(ctx); err != nil {
		return "", "", fmt.Errorf("custom task input: %w", err)
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return DefaultProfileName, "", nil // empty custom task → fall back to general
	}
	return customProfileName, task, nil
}

// promptForProfileToRun asks which configured profile to inspect. It pre-selects
// the default but never runs without an explicit choice, so a bare
// `entire inspect` doesn't silently spawn a crew.
func promptForProfileToRun(ctx context.Context, s *settings.EntireSettings) (string, error) {
	profiles := nonZeroProfiles(s.ReviewProfiles)
	names := sortedProfileNames(profiles)
	if len(names) == 0 {
		return "", errors.New("no configured profiles to choose from")
	}
	defaultName := strings.TrimSpace(s.ReviewDefaultProfile)
	picked := defaultName
	if _, ok := profiles[picked]; !ok {
		picked = names[0]
	}
	options := make([]huh.Option[string], 0, len(names))
	for _, name := range names {
		p := profiles[name]
		p.Agents = nonZeroAgentConfigs(p.Agents)
		workers := make([]string, 0, len(p.Agents))
		for _, w := range sortedProfileAgentNames(p) {
			workers = append(workers, reviewAgentName(w, p.Agents[w]))
		}
		label := name
		if name == defaultName {
			label += " (default)"
		}
		if len(workers) > 0 {
			label += " — " + strings.Join(workers, ", ")
		}
		options = append(options, huh.NewOption(label, name))
	}
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which profile should inspect the branch?").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("profile picker: %w", err)
	}
	return picked, nil
}

// crewSlot is one worker slot in the review crew: an agent plus an optional
// model ("" means the agent's own default). Duplicate slots — the same agent
// and model — are allowed; each becomes its own worker.
type crewSlot struct {
	agent string
	model string
}

// promptForReviewCrew builds the review crew on a single screen. It seeds one
// slot per launchable agent (the guided default is "all agents"), then lets the
// user add, edit, or remove slots from a list until Done. Duplicate slots (same
// agent and model) are allowed; each becomes its own worker.
func promptForReviewCrew(ctx context.Context, profileName string, launchable []string, existing settings.ReviewProfileConfig) (settings.ReviewProfileConfig, error) {
	// Seed from the existing profile's workers when editing one; otherwise the
	// guided default is one slot per launchable agent.
	slots := make([]crewSlot, 0, len(launchable))
	if len(existing.Agents) > 0 {
		for _, w := range sortedProfileAgentNames(existing) {
			cfg := existing.Agents[w]
			slots = append(slots, crewSlot{agent: reviewAgentName(w, cfg), model: strings.TrimSpace(cfg.Model)})
		}
	} else {
		for _, name := range launchable {
			slots = append(slots, crewSlot{agent: name})
		}
	}

	const (
		actAdd     = "add"
		actDone    = "done"
		slotPrefix = "slot:"
	)
	for {
		options := make([]huh.Option[string], 0, len(slots)+2)
		for i, s := range slots {
			options = append(options, huh.NewOption(fmt.Sprintf("%d  %s", i+1, slotLabel(s)), slotPrefix+strconv.Itoa(i)))
		}
		options = append(options, huh.NewOption("+ Add slot", actAdd))
		if len(slots) > 0 {
			options = append(options, huh.NewOption(fmt.Sprintf("Done · %d slot(s)", len(slots)), actDone))
		}
		picked := actDone
		if len(slots) == 0 {
			picked = actAdd
		}
		form := newAccessibleForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title("Review crew").
				Description("Select a slot to edit or remove it; + Add slot to add one.").
				Options(options...).
				Height(reviewPickerHeight(len(options))).
				Value(&picked),
		))
		if err := form.RunWithContext(ctx); err != nil {
			return settings.ReviewProfileConfig{}, fmt.Errorf("review crew picker: %w", err)
		}

		switch {
		case picked == actAdd:
			slot, err := promptCrewSlot(ctx, launchable)
			if err != nil {
				return settings.ReviewProfileConfig{}, err
			}
			slots = append(slots, slot)
		case picked == actDone:
			if len(slots) == 0 {
				continue
			}
			return buildCrewProfile(ctx, profileName, slots), nil
		case strings.HasPrefix(picked, slotPrefix):
			idx, convErr := strconv.Atoi(strings.TrimPrefix(picked, slotPrefix))
			if convErr != nil || idx < 0 || idx >= len(slots) {
				continue
			}
			action, err := promptSlotAction(ctx, slots[idx])
			if err != nil {
				return settings.ReviewProfileConfig{}, err
			}
			switch action {
			case "edit":
				slot, err := promptCrewSlot(ctx, launchable)
				if err != nil {
					return settings.ReviewProfileConfig{}, err
				}
				slots[idx] = slot
			case "remove":
				slots = append(slots[:idx], slots[idx+1:]...)
			}
		}
	}
}

// promptCrewSlot prompts for one slot: an agent then a model.
func promptCrewSlot(ctx context.Context, launchable []string) (crewSlot, error) {
	agentName, err := promptCrewAgent(ctx, launchable)
	if err != nil {
		return crewSlot{}, err
	}
	model, err := promptCrewModel(ctx, agentName)
	if err != nil {
		return crewSlot{}, err
	}
	return crewSlot{agent: agentName, model: model}, nil
}

// buildCrewProfile turns an ordered slot list into a profile. Each slot becomes
// a worker keyed by workerIDForAgentModel, which disambiguates duplicates
// (claude-code, claude-code-2, claude-code:opus, …).
func buildCrewProfile(ctx context.Context, profileName string, slots []crewSlot) settings.ReviewProfileConfig {
	profile := settings.ReviewProfileConfig{
		Task:   profileTask(profileName, settings.ReviewProfileConfig{}),
		Agents: make(map[string]settings.ReviewConfig, len(slots)),
	}
	for _, s := range slots {
		cfg := defaultReviewAgentConfig(profileName, s.agent)
		// Set Agent explicitly so the worker is valid even when the agent has no
		// default skills/prompt (e.g. Pi): IsZero is false once Agent is set, and
		// reviewAgentName resolves the real agent for the worker.
		cfg.Agent = s.agent
		cfg.Model = s.model
		profile.Agents[workerIDForAgentModel(s.agent, s.model, profile.Agents)] = cfg
	}
	profile.Master = defaultReviewMaster(ctx, profile.Agents)
	return profile
}

// promptSlotAction asks what to do with an existing slot row.
func promptSlotAction(ctx context.Context, slot crewSlot) (string, error) {
	picked := "cancel"
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(slotLabel(slot)).
			Options(
				huh.NewOption("Edit", "edit"),
				huh.NewOption("Remove", "remove"),
				huh.NewOption("Cancel", "cancel"),
			).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("slot action: %w", err)
	}
	return picked, nil
}

func slotLabel(s crewSlot) string {
	model := strings.TrimSpace(s.model)
	if model == "" {
		model = "default model"
	}
	return labelForSimpleAgent(s.agent) + " · " + model
}

// promptCrewAgent picks the agent for a new slot. Auto-selects when only one
// launchable agent exists.
func promptCrewAgent(ctx context.Context, launchable []string) (string, error) {
	if len(launchable) == 1 {
		return launchable[0], nil
	}
	options := make([]huh.Option[string], 0, len(launchable))
	for _, name := range launchable {
		options = append(options, huh.NewOption(labelForSimpleAgent(name), name))
	}
	picked := launchable[0]
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Add a slot — which agent?").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("review crew agent: %w", err)
	}
	return picked, nil
}

// promptCrewModel picks a model for a slot: Default, an advertised model, or a
// Custom… free-text value. Returns "" for the agent's own default.
func promptCrewModel(ctx context.Context, agentName string) (string, error) {
	models := listAgentModelOptions(ctx, agentName)
	options := make([]huh.Option[string], 0, len(models)+2)
	options = append(options, huh.NewOption("Default (agent's own default model)", reviewModelDefaultSentinel))
	for _, m := range models {
		label := m.ID
		if m.Note != "" {
			label = m.ID + "  — " + m.Note
		}
		options = append(options, huh.NewOption(label, m.ID))
	}
	options = append(options, huh.NewOption("Custom… (type any value)", reviewModelCustomSentinel))

	picked := reviewModelDefaultSentinel
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Model for " + labelForSimpleAgent(agentName)).
			Description("Pick a model, Default, or Custom… to type any value.").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("review crew model: %w", err)
	}
	switch picked {
	case reviewModelDefaultSentinel:
		return "", nil
	case reviewModelCustomSentinel:
		model := ""
		customForm := newAccessibleForm(huh.NewGroup(
			huh.NewInput().
				Title("Custom model for " + labelForSimpleAgent(agentName)).
				Description("Any value accepted by the agent CLI; leave blank for the default.").
				Value(&model),
		))
		if err := customForm.RunWithContext(ctx); err != nil {
			return "", fmt.Errorf("custom model input: %w", err)
		}
		return strings.TrimSpace(model), nil
	default:
		return picked, nil
	}
}

func labelForSimpleAgent(name string) string {
	ag, err := agent.Get(types.AgentName(name))
	if err != nil {
		return name
	}
	return string(ag.Type())
}

// reviewModelCustomSentinel is the select value for "type a custom model".
// It cannot collide with a real model id (which never contains spaces).
const reviewModelCustomSentinel = "__custom__"

// reviewModelDefaultSentinel is the crew multiselect value for "use the agent's
// own default model". Resolves to an empty Model string; distinct from a real
// model id and from reviewModelCustomSentinel.
const reviewModelDefaultSentinel = "__default__"

func listAgentModelOptions(ctx context.Context, agentName string) []agent.ModelInfo {
	ag, err := agent.Get(types.AgentName(agentName))
	if err != nil {
		return nil
	}
	lister, ok := agent.AsModelLister(ag)
	if !ok {
		return nil
	}
	models, err := lister.ListModels(ctx)
	if err != nil {
		return nil
	}
	return models
}

func modelInList(id string, models []agent.ModelInfo) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// promptForStandaloneMaster picks the standalone master (judge) as its own
// agent + model, independent of the worker slots. Candidates are launchable
// agents that can write text. Returns (agentName, model).
func promptForStandaloneMaster(ctx context.Context, launchable []string, existing settings.ReviewProfileConfig) (string, string, error) {
	candidates := make([]string, 0, len(launchable))
	for _, name := range launchable {
		if agentSupportsTextGeneration(ctx, name) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", "", errors.New("no installed agent can write the final report")
	}

	agentName := candidates[0]
	// Pre-select the existing master when re-configuring.
	if cur, _, ok := profileMasterIdentity(existing); ok {
		for _, c := range candidates {
			if c == cur {
				agentName = cur
				break
			}
		}
	}
	if len(candidates) > 1 {
		options := make([]huh.Option[string], 0, len(candidates))
		for _, name := range candidates {
			options = append(options, huh.NewOption(labelForSimpleAgent(name), name))
		}
		form := newAccessibleForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title("Choose the review master").
				Description("Evaluates the workers' reports and writes the final verdict.").
				Options(options...).
				Height(reviewPickerHeight(len(options))).
				Value(&agentName),
		))
		if err := form.RunWithContext(ctx); err != nil {
			return "", "", fmt.Errorf("master agent picker: %w", err)
		}
	}
	model, err := promptCrewModel(ctx, agentName)
	if err != nil {
		return "", "", fmt.Errorf("master model picker: %w", err)
	}
	return agentName, model, nil
}

func ConfirmRunReviewNow(ctx context.Context, out io.Writer) (bool, error) {
	runNow := true
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Start review now?").
			Affirmative("Start review").
			Negative("Not now").
			Value(&runNow),
	))
	if err := form.RunWithContext(ctx); err != nil {
		// Aborting the confirm (Ctrl+C / Esc) is a clean "not now", not a
		// command error. Surface it as picker-cancelled so the caller maps it
		// to a silent exit via handlePickerError.
		fmt.Fprintln(out, "Not started. Run `entire inspect` when ready.")
		return false, ErrPickerCancelled
	}
	if !runNow {
		fmt.Fprintln(out, "Not started. Run `entire inspect` when ready.")
	}
	return runNow, nil
}

func RunReviewConfigPicker(ctx context.Context, out io.Writer, getInstalled func(context.Context) []types.AgentName) (map[string]settings.ReviewConfig, error) {
	return RunReviewProfileConfigPicker(ctx, out, getInstalled, DefaultProfileName)
}

func RunReviewProfileConfigPicker(ctx context.Context, out io.Writer, getInstalled func(context.Context) []types.AgentName, profileName string) (map[string]settings.ReviewConfig, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		profileName = DefaultProfileName
	}
	installed := getInstalled(ctx)
	if len(installed) == 0 {
		return nil, errors.New(
			"no agents with hooks installed; " +
				"run 'entire configure --agent <name>' to install hooks for one, " +
				"or 'entire enable' to set up the repo",
		)
	}

	// Narrow to agents that have a curated skills list; others need manual
	// editing of clone-local preferences under review.<agent-name>.
	type configurableAgent struct {
		name types.AgentName
		ag   agent.Agent
	}
	var configurable []configurableAgent
	for _, name := range installed {
		if !skilldiscovery.IsEligible(string(name)) {
			continue
		}
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		configurable = append(configurable, configurableAgent{name: name, ag: ag})
	}
	if len(configurable) == 0 {
		prefsPath, pathErr := settings.ClonePreferencesPath(ctx)
		if pathErr != nil {
			return nil, errors.New(
				"no installed agents have curated review skills; " +
					"install an eligible agent and run `entire inspect --edit`, " +
					"or edit clone-local review preferences under review.<agent-name>",
			)
		}
		return nil, fmt.Errorf(
			"no installed agents have curated review skills; "+
				"install an eligible agent and run `entire inspect --edit`, "+
				"or edit clone-local review preferences (%s) under review.<agent-name>",
			prefsPath,
		)
	}

	// Load existing profile config so we can pre-check saved skills and seed
	// saved prompts. A load error here means the settings file is malformed;
	// log at Warn so users debugging "my saved skills aren't pre-checked" can
	// see why, but keep going with an empty prefill — runReview already
	// surfaces the same error distinctly when it's the first load.
	existing := map[string]settings.ReviewConfig{}
	existingMaster := ""
	if s, err := settings.Load(ctx); err != nil {
		logging.Warn(ctx, "settings.Load failed when pre-filling picker", slog.String("error", err.Error()))
	} else if s != nil {
		if profile, ok := s.ReviewProfiles[profileName]; ok {
			existing = profile.Agents
			existingMaster = profile.Master
		}
	}

	// Up-front header: make the order and count obvious so users can spot
	// when an agent they expected isn't being offered (e.g., hooks not
	// installed for it yet).
	labels := make([]string, 0, len(configurable))
	for _, c := range configurable {
		labels = append(labels, string(c.ag.Type()))
	}
	fmt.Fprintf(out, "Configuring review for %d agent(s): %s\n", len(configurable), strings.Join(labels, ", "))
	fmt.Fprintln(out, "(Previously-saved skills are pre-checked. Space to toggle, enter to confirm.)")
	fmt.Fprintln(out)

	selected := map[string]settings.ReviewConfig{}
	for i, c := range configurable {
		curated := skilldiscovery.CuratedBuiltinsFor(string(c.name))

		// Discover + dedupe + filter hints.
		var discovered []agent.DiscoveredSkill
		if d, ok := c.ag.(agent.SkillDiscoverer); ok {
			if ds, dErr := d.DiscoverReviewSkills(ctx); dErr == nil {
				discovered = ds
			} else {
				logging.Debug(ctx, "review discovery failed",
					slog.String("agent", string(c.name)), slog.String("error", dErr.Error()))
			}
		}
		builtinNames := builtinNameSet(curated)
		discovered = filterOutBuiltinCollisions(discovered, builtinNames)

		discoveredSet := make(map[string]struct{}, len(discovered))
		for _, d := range discovered {
			discoveredSet[d.Name] = struct{}{}
		}
		activeHints := skilldiscovery.ActiveInstallHintsFor(string(c.name), discoveredSet)

		// Pre-populate pick slices from saved config so the picker preselects
		// them. The header promises "previously-saved skills are pre-checked";
		// without this split + Option.Selected(true) in BuildReviewPickerFields,
		// --edit with accept-defaults silently wipes the agent's saved skills.
		builtinPicks, discoveredPicks := SplitSavedPicks(
			existing[string(c.name)].Skills, curated, discovered,
		)
		prompt := existing[string(c.name)].Prompt
		model := existing[string(c.name)].Model

		fields := BuildReviewPickerFields(
			string(c.name), curated, discovered, activeHints, prompt,
			&builtinPicks, &discoveredPicks, &prompt,
		)
		fields = append(fields, huh.NewInput().
			Title("Model (optional)").
			Description("Leave blank to use the agent default; pass any model string accepted by the agent CLI.").
			Value(&model))

		// Prepend a non-blocking header Note so the agent being configured
		// is always clearly visible.
		header := huh.NewNote().
			Title(string(c.ag.Type())).
			Description(fmt.Sprintf("Agent %d of %d · pick review skills and optional instructions", i+1, len(configurable)))
		fields = append([]huh.Field{header}, fields...)

		form := newAccessibleForm(huh.NewGroup(fields...))
		if err := form.RunWithContext(ctx); err != nil {
			return nil, fmt.Errorf("picker for %s: %w", c.name, err)
		}

		cfg := settings.ReviewConfig{
			Model:  strings.TrimSpace(model),
			Skills: dedupeStrings(append(builtinPicks, discoveredPicks...)),
			Prompt: strings.TrimSpace(prompt),
		}
		if !cfg.IsZero() {
			selected[string(c.name)] = cfg
		}
	}
	// Merge the picker's output with existing entries the picker could not
	// surface. Without the merge, save would replace s.Review wholesale and
	// silently drop entries the user had configured for external agents,
	// uncurated agents, or agents whose hooks are temporarily uninstalled.
	offered := make(map[string]struct{}, len(configurable))
	for _, c := range configurable {
		offered[string(c.name)] = struct{}{}
	}
	merged := MergePickerResults(existing, offered, selected)

	// The emptiness check runs on `merged`, not `selected`.
	if len(merged) == 0 {
		return nil, errors.New("no review skills or prompt configured")
	}

	masterAgent, err := pickReviewMasterAgentPreference(ctx, merged, existingMaster)
	if err != nil {
		return nil, err
	}
	if err := saveReviewProfileConfig(ctx, profileName, merged, masterAgent); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Saved crew profile %q to local review preferences. Edit later with `entire inspect --edit --profile %s`.\n", profileName, profileName)
	return merged, nil
}

// MergePickerResults combines the picker's output with existing review
// config entries that the picker did not surface. Agents in `offered` are
// fully controlled by the picker: if they appear in `selected` with a
// non-zero config the entry is set, otherwise the entry is removed.
// Agents not in `offered` keep their existing config untouched.
//
// Exported so tests can drive it directly — the picker itself
// can't run headless.
func MergePickerResults(existing map[string]settings.ReviewConfig, offered map[string]struct{}, selected map[string]settings.ReviewConfig) map[string]settings.ReviewConfig {
	merged := make(map[string]settings.ReviewConfig, len(existing)+len(selected))
	for name, cfg := range existing {
		if _, wasOffered := offered[name]; !wasOffered {
			merged[name] = cfg
		}
	}
	for name, cfg := range selected {
		merged[name] = cfg
	}
	return merged
}

func saveReviewProfileConfig(ctx context.Context, profileName string, agents map[string]settings.ReviewConfig, master string) error {
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
	// Merge into any existing profile so the advanced skills picker only
	// rewrites what it actually edits (agents + master). Profile-level fields
	// the picker never surfaces — custom `task` text and `master_model` — are
	// preserved instead of being clobbered with built-in defaults.
	profile := prefs.ReviewProfiles[profileName]
	profile.Agents = agents
	profile.Master = master
	if strings.TrimSpace(profile.Task) == "" {
		profile.Task = profileTask(profileName, settings.ReviewProfileConfig{})
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

func pickReviewMasterAgentPreference(ctx context.Context, review map[string]settings.ReviewConfig, current string) (string, error) {
	choices := reviewMasterAgentChoices(review)
	switch len(choices) {
	case 0:
		return current, nil
	case 1:
		return choices[0].Name, nil
	default:
		return promptForReviewMasterAgent(ctx, choices, current)
	}
}

// defaultAgentPick returns the saved choice if it is still offered, otherwise
// the first choice. Shared by the master picker.
func defaultAgentPick(choices []AgentChoice, saved string) string {
	if pick, ok := savedAgentPick(choices, saved); ok {
		return pick
	}
	if len(choices) == 0 {
		return ""
	}
	return choices[0].Name
}

func savedAgentPick(choices []AgentChoice, saved string) (string, bool) {
	for _, choice := range choices {
		if choice.Name == saved {
			return saved, true
		}
	}
	return "", false
}

func reviewMasterAgentChoices(configured map[string]settings.ReviewConfig) []AgentChoice {
	choices := make([]AgentChoice, 0, len(configured))
	for name, cfg := range configured {
		if cfg.IsZero() {
			continue
		}
		agentName := reviewAgentName(name, cfg)
		ag, err := agent.Get(types.AgentName(agentName))
		if err != nil {
			continue
		}
		if _, ok := agent.AsTextGenerator(ag); !ok {
			continue
		}
		label := string(ag.Type())
		if name != agentName || strings.TrimSpace(cfg.Model) != "" {
			label = reviewWorkerLabel(name, cfg)
		}
		choices = append(choices, AgentChoice{Name: name, Label: label})
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].Name < choices[j].Name })
	return choices
}

func promptForReviewMasterAgent(ctx context.Context, choices []AgentChoice, saved string) (string, error) {
	options := make([]huh.Option[string], 0, len(choices))
	for _, choice := range choices {
		options = append(options, huh.NewOption(choice.Label, choice.Name))
	}
	picked := defaultAgentPick(choices, saved)
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Choose review master").
			Description("The master critically evaluates worker reports and writes the final report.").
			Options(options...).
			Height(reviewPickerHeight(len(options))).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("review master picker: %w", err)
	}
	return picked, nil
}

// ComputeEligibleConfiguredForProfile returns the sorted list of agents in a
// profile that are both configured and have hooks installed.
func ComputeEligibleConfiguredForProfile(profile settings.ReviewProfileConfig, installed []types.AgentName) []AgentChoice {
	return eligibleAgentChoices(profile.Agents, installed)
}

func eligibleAgentChoices(configured map[string]settings.ReviewConfig, installed []types.AgentName) []AgentChoice {
	installedSet := make(map[types.AgentName]struct{}, len(installed))
	for _, name := range installed {
		installedSet[name] = struct{}{}
	}
	out := make([]AgentChoice, 0, len(configured))
	for name, cfg := range configured {
		if cfg.IsZero() {
			continue
		}
		if _, ok := installedSet[types.AgentName(reviewAgentName(name, cfg))]; !ok {
			continue
		}
		out = append(out, AgentChoice{Name: name, Label: labelForAgentChoice(name, cfg)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// labelForAgentChoice builds the picker-visible label for an agent row.
func labelForAgentChoice(name string, cfg settings.ReviewConfig) string {
	label := reviewWorkerLabel(name, cfg)
	switch {
	case len(cfg.Skills) > 0:
		return fmt.Sprintf("%s   (%d skills configured)", label, len(cfg.Skills))
	case cfg.Prompt != "":
		return label + "   (prompt-only)"
	default:
		return label
	}
}

// computeLaunchableEligibleForProfile returns the subset of
// ComputeEligibleConfiguredForProfile that also have a non-nil AgentReviewer.
// "Launchable" here is a historical shorthand for "has an Entire review-runner
// adapter"; it is not a claim about whether the agent's own CLI supports
// headless execution.
//
// reviewerFor is deps.ReviewerFor injected at the cmd layer; it returns nil for
// agents that are known to Entire but not yet wired into `entire review`.
func computeLaunchableEligibleForProfile(
	profile settings.ReviewProfileConfig,
	installed []types.AgentName,
	reviewerFor func(string) reviewtypes.AgentReviewer,
) []AgentChoice {
	eligible := ComputeEligibleConfiguredForProfile(profile, installed)
	return filterLaunchableEligibleForProfile(profile, eligible, reviewerFor)
}

func filterLaunchableEligibleForProfile(profile settings.ReviewProfileConfig, eligible []AgentChoice, reviewerFor func(string) reviewtypes.AgentReviewer) []AgentChoice {
	out := make([]AgentChoice, 0, len(eligible))
	for _, c := range eligible {
		cfg := profile.Agents[c.Name]
		if reviewerFor(reviewAgentName(c.Name, cfg)) != nil {
			out = append(out, c)
		}
	}
	return out
}

// PromptForAgent renders the single-select agent picker shown when more than
// one eligible agent is configured. Returns the chosen agent name. Respects
// accessibility mode via newAccessibleForm.
func PromptForAgent(ctx context.Context, eligible []AgentChoice) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("agent picker: %w", err)
	}
	if len(eligible) == 0 {
		return "", errors.New("no eligible agents to prompt for")
	}
	options := make([]huh.Option[string], 0, len(eligible))
	for _, c := range eligible {
		options = append(options, huh.NewOption(c.Label, c.Name))
	}
	picked := eligible[0].Name
	form := newAccessibleForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Which agent should run this review?").
			Options(options...).
			Value(&picked),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return "", fmt.Errorf("agent picker: %w", err)
	}
	return picked, nil
}

// SelectReviewAgent picks an agent from the configured review map.
//
// If override is non-empty, returns the config for that agent or an error
// listing the configured alternatives. Otherwise returns the alphabetically
// first configured agent — deterministic but user-overridable via --agent.
func SelectReviewAgent(review map[string]settings.ReviewConfig, override string) (string, settings.ReviewConfig, error) {
	if len(review) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	var names []string
	for name, cfg := range review {
		if !cfg.IsZero() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", settings.ReviewConfig{}, errors.New("no review config found")
	}
	sort.Strings(names)
	if override != "" {
		if cfg, ok := review[override]; ok && !cfg.IsZero() {
			return override, cfg, nil
		}
		return "", settings.ReviewConfig{}, fmt.Errorf(
			"agent %q is not configured for review; configured agents: %s",
			override, strings.Join(names, ", "),
		)
	}
	pick := names[0]
	return pick, review[pick], nil
}

// VerifyConfiguredSkillsInstalled is the spawn-time backstop for the
// silent-failure vector. For each skill in cfg.Skills, check it's either a
// curated built-in or returned by the agent's SkillDiscoverer; fail with a
// user-facing error if any skill is missing. Empty Skills (prompt-only
// config) short-circuits to nil — a freeform prompt has no skill list to
// validate against.
func VerifyConfiguredSkillsInstalled(ctx context.Context, ag agent.Agent, cfg settings.ReviewConfig) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	builtins := builtinNameSet(skilldiscovery.CuratedBuiltinsFor(string(ag.Name())))
	discoveredNames := map[string]struct{}{}
	if d, ok := ag.(agent.SkillDiscoverer); ok {
		if skills, err := d.DiscoverReviewSkills(ctx); err == nil {
			for _, s := range skills {
				discoveredNames[s.Name] = struct{}{}
			}
		} else {
			logging.Debug(ctx, "skill verification discovery failed",
				slog.String("agent", string(ag.Name())), slog.String("error", err.Error()))
		}
	}
	var missing []string
	for _, s := range cfg.Skills {
		if _, ok := builtins[s]; ok {
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			continue
		}
		missing = append(missing, s)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"configured review skill(s) not installed: %s\n"+
			"run `entire inspect --edit` to reconfigure, or install the plugin and retry",
		strings.Join(missing, ", "),
	)
}

// BuildReviewPickerFields composes the per-agent group fields for the
// review picker. Returns a slice of huh.Field in render order:
//
//	0: built-in commands (multiselect) OR note
//	1: installed plugin skills (multiselect) OR note
//	2: install hints (note with all active hint messages) — OMITTED if empty
//	3: additional instructions (text) — always present
func BuildReviewPickerFields(
	agentName string,
	builtins []skilldiscovery.CuratedSkill,
	discovered []agent.DiscoveredSkill,
	activeHints []skilldiscovery.InstallHint,
	previousPrompt string,
	builtinPicksOut, discoveredPicksOut *[]string,
	promptOut *string,
) []huh.Field {
	var fields []huh.Field

	if builtinPicksOut != nil && len(*builtinPicksOut) == 0 &&
		len(builtins) == 1 && strings.TrimSpace(previousPrompt) == "" {
		*builtinPicksOut = []string{builtins[0].Name}
	}

	builtinPreselected := preselectedSet(builtinPicksOut)
	discoveredPreselected := preselectedSet(discoveredPicksOut)

	if len(builtins) > 0 {
		opts := make([]huh.Option[string], 0, len(builtins))
		for _, b := range builtins {
			opt := huh.NewOption(b.Name, b.Name)
			if _, ok := builtinPreselected[b.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Built-in commands").
			Options(opts...).
			Height(len(opts) + 1)
		if builtinPicksOut != nil {
			ms = ms.Value(builtinPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Built-in commands").
			Description(fmt.Sprintf("No built-in review commands in %s.", agentName)))
	}

	if len(discovered) > 0 {
		opts := make([]huh.Option[string], 0, len(discovered))
		for _, d := range discovered {
			opt := huh.NewOption(d.Name, d.Name)
			if _, ok := discoveredPreselected[d.Name]; ok {
				opt = opt.Selected(true)
			}
			opts = append(opts, opt)
		}
		ms := huh.NewMultiSelect[string]().
			Title("Installed plugin skills").
			Options(opts...).
			Height(len(opts) + 1)
		if discoveredPicksOut != nil {
			ms = ms.Value(discoveredPicksOut)
		}
		fields = append(fields, ms)
	} else {
		fields = append(fields, huh.NewNote().
			Title("Installed plugin skills").
			Description("No plugin review skills detected on disk."))
	}

	if len(activeHints) > 0 {
		var sb strings.Builder
		for i, h := range activeHints {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("• ")
			sb.WriteString(h.Message)
		}
		fields = append(fields, huh.NewNote().
			Title("Install more").
			Description(sb.String()))
	}

	text := huh.NewText().
		Title("Additional instructions (optional)").
		Description("Added after selected skills. If no skills are selected, this becomes the full review prompt.")
	if promptOut != nil {
		*promptOut = previousPrompt
		text = text.Value(promptOut)
	}
	fields = append(fields, text)

	return fields
}

// SplitSavedPicks partitions a flat saved-skills list into the subset that
// matches built-in curated commands and the subset that matches discovered
// plugin skills. Skill names that match neither are dropped from both — they're
// preserved on the settings side via MergePickerResults when they belong to a
// picker-unaware agent entry.
func SplitSavedPicks(saved []string, builtins []skilldiscovery.CuratedSkill, discovered []agent.DiscoveredSkill) ([]string, []string) {
	builtinNames := make(map[string]struct{}, len(builtins))
	for _, b := range builtins {
		builtinNames[b.Name] = struct{}{}
	}
	discoveredNames := make(map[string]struct{}, len(discovered))
	for _, d := range discovered {
		discoveredNames[d.Name] = struct{}{}
	}
	var builtinPicks, discoveredPicks []string
	for _, s := range saved {
		if _, ok := builtinNames[s]; ok {
			builtinPicks = append(builtinPicks, s)
			continue
		}
		if _, ok := discoveredNames[s]; ok {
			discoveredPicks = append(discoveredPicks, s)
		}
	}
	return builtinPicks, discoveredPicks
}

// preselectedSet turns a slice pointer's current contents into a lookup
// set for the picker's "previously-saved" pre-selection.
func preselectedSet(slice *[]string) map[string]struct{} {
	if slice == nil || len(*slice) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(*slice))
	for _, s := range *slice {
		out[s] = struct{}{}
	}
	return out
}

func builtinNameSet(curated []skilldiscovery.CuratedSkill) map[string]struct{} {
	set := make(map[string]struct{}, len(curated))
	for _, c := range curated {
		set[c.Name] = struct{}{}
	}
	return set
}

// filterOutBuiltinCollisions drops any discovered skill whose name collides
// with a curated built-in. Built-in wins because it carries a richer,
// hand-authored description.
func filterOutBuiltinCollisions(discovered []agent.DiscoveredSkill, builtins map[string]struct{}) []agent.DiscoveredSkill {
	if len(discovered) == 0 || len(builtins) == 0 {
		return discovered
	}
	out := make([]agent.DiscoveredSkill, 0, len(discovered))
	for _, d := range discovered {
		if _, clash := builtins[d.Name]; clash {
			continue
		}
		out = append(out, d)
	}
	return out
}

func dedupeStrings(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
