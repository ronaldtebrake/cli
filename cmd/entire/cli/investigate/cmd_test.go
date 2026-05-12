package investigate_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/investigate"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubSpawner is a minimal spawn.Spawner used in tests. It returns a cmd
// that always succeeds, so production loop code can run without spawning a
// real agent.
type stubSpawner struct{ name string }

func (s stubSpawner) Name() string { return s.name }
func (s stubSpawner) BuildCmd(ctx context.Context, env []string, _ string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "true")
	cmd.Env = env
	return cmd
}

// silentPassthrough returns the same error unchanged. Mirrors review's
// test helper.
func silentPassthrough(err error) error { return err }

// setupInvestigateRepo creates a fresh git repo with one commit and chdirs
// into it. Mirrors review's setupCmdTestRepo.
func setupInvestigateRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	return tmp
}

// captureLoopRun returns a LoopRun stub that records the LoopInput it was
// given. Useful for tests that want to assert flag plumbing without
// spawning real agents.
func captureLoopRun() (capture *investigate.LoopInput, fn func(ctx context.Context, in investigate.LoopInput, ldeps investigate.LoopDeps) (investigate.LoopResult, error)) {
	captured := &investigate.LoopInput{}
	return captured, func(_ context.Context, in investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
		*captured = in
		return investigate.LoopResult{
			Outcome: investigate.OutcomeQuorum,
			State:   nil,
		}, nil
	}
}

// newTestDeps builds a Deps wired with passthrough silent error and
// stub spawners for the named agents.
func newTestDeps(t *testing.T, installed []types.AgentName, launchable []string) investigate.Deps {
	t.Helper()
	launchableSet := make(map[string]struct{}, len(launchable))
	for _, n := range launchable {
		launchableSet[n] = struct{}{}
	}
	return investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return installed },
		NewSilentError:              silentPassthrough,
		SpawnerFor: func(name string) spawn.Spawner {
			if _, ok := launchableSet[name]; ok {
				return stubSpawner{name: name}
			}
			return nil
		},
		LaunchFix: func(_ context.Context, _ string, _ string) error { return nil },
	}
}

func TestNewCommand_RejectsConflictingInputs(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)

	// Need a seed file path to reach the args-and-topic conflict path,
	// but validation runs before any I/O so it doesn't have to exist.
	cmd.SetArgs([]string{"some-seed.md", "--topic=bar"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when [seed-doc] and --topic are both set")
	}
}

func TestNewCommand_RejectsContinueWithSeed(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--continue=abcdef012345", "--topic=bar"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --continue and --topic are both set")
	}
}

func TestNewCommand_RejectsEditWithFindings(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--edit", "--findings"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --edit and --findings are both set")
	}
}

func TestNewCommand_FixSubcommand_Help(t *testing.T) {
	t.Parallel()
	deps := investigate.Deps{NewSilentError: silentPassthrough}
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"fix", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "Launch a coding agent") {
		t.Errorf("--help output missing fix description: %s", out.String())
	}
}

func TestNewCommand_AttachSubcommandWiredFromDeps(t *testing.T) {
	t.Parallel()
	attach := &cobra.Command{
		Use:   "attach",
		Short: "tag a session as an investigation",
		RunE:  func(_ *cobra.Command, _ []string) error { return nil },
	}
	deps := investigate.Deps{
		NewSilentError: silentPassthrough,
		AttachCmd:      attach,
	}
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "attach") {
		t.Errorf("--help output missing attach subcommand: %s", out.String())
	}
}

func TestNewCommand_NotInGitRepoReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--topic=foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error outside a git repo")
	}
	if !strings.Contains(errBuf.String(), "Not a git repository") {
		t.Errorf("stderr should mention 'Not a git repository', got: %s", errBuf.String())
	}
}

func TestNewCommand_AgentsFlagOverrideUsed(t *testing.T) {
	setupInvestigateRepo(t)

	// Persist a settings file with two agents; --agents flag must override.
	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"agent-default-1", "agent-default-2"},
		MaxTurns: 3,
	}); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"override-a", "override-b"}, []string{"override-a", "override-b"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--topic=test investigation",
		"--agents=override-a,override-b",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got, want := captured.Agents, []string{"override-a", "override-b"}; !equalStringSlices(got, want) {
		t.Errorf("LoopInput.Agents = %v, want %v", got, want)
	}
}

func TestNewCommand_FindingsBranchListsManifests(t *testing.T) {
	setupInvestigateRepo(t)

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--findings"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Empty store → "No local investigations found." message.
	if !strings.Contains(out.String(), "No local investigations found") {
		t.Errorf("stdout should report empty list, got: %s", out.String())
	}
}

// TestNewCommand_FreshRunWritesManifest exercises the end-to-end fresh-run
// path with a stub LoopRun. Verifies the manifest file is written and the
// footer hint is printed.
func TestNewCommand_FreshRunWritesManifest(t *testing.T) {
	setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"stub-agent"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"stub-agent"}, []string{"stub-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--topic=test investigation"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if captured.RunID == "" {
		t.Fatal("LoopInput.RunID was empty — fresh-run path didn't generate one")
	}
	// Manifest should mention how to run fix.
	if !strings.Contains(out.String(), "entire investigate fix") {
		t.Errorf("expected fix hint in output, got:\n%s", out.String())
	}
}

// TestNewCommand_FreshRunRejectsNonLaunchableAgent verifies the spawner
// guard fires before the bootstrap step.
func TestNewCommand_FreshRunRejectsNonLaunchableAgent(t *testing.T) {
	setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"missing-spawner"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	deps := newTestDeps(t, []types.AgentName{"missing-spawner"}, nil) // installed but not launchable
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--topic=foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when configured agent has no spawner")
	}
	if !strings.Contains(errBuf.String(), "spawner missing") {
		t.Errorf("stderr should mention 'spawner missing', got: %s", errBuf.String())
	}
}

func TestNewCommand_FreshRunRejectsAgentWithoutHooks(t *testing.T) {
	setupInvestigateRepo(t)

	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:   []string{"hookless"},
		MaxTurns: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Spawner exists but agent isn't in the installed list.
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              silentPassthrough,
		SpawnerFor:                  func(_ string) spawn.Spawner { return stubSpawner{name: "hookless"} },
		LaunchFix:                   func(_ context.Context, _ string, _ string) error { return nil },
	}
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--topic=foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when configured agent has no hooks")
	}
	if !strings.Contains(errBuf.String(), "entire configure --agent") {
		t.Errorf("stderr should hint at `entire configure --agent`, got: %s", errBuf.String())
	}
}

func TestNewCommand_ContinueLoadsExistingState(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	// Create a state file in the conventional location.
	commonDir := filepath.Join(tmp, ".git")
	stateDir := filepath.Join(commonDir, "entire-investigations", "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "abcdef012345"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "resumed topic",
		Agents:      []string{"resumed-agent"},
		MaxTurns:    2,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		TimelineDoc: filepath.Join(tmp, "find-timeline.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"resumed-agent"}, []string{"resumed-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if captured.RunID != runID {
		t.Errorf("LoopInput.RunID = %q, want %q", captured.RunID, runID)
	}
	if captured.Topic != "resumed topic" {
		t.Errorf("LoopInput.Topic = %q, want %q", captured.Topic, "resumed topic")
	}
	if !strings.Contains(out.String(), "Resuming investigation") {
		t.Errorf("expected 'Resuming investigation' banner, got: %s", out.String())
	}
}

// TestNewCommand_ContinueLoadsAlwaysPromptFromSettings verifies that the
// configured settings.Investigate.AlwaysPrompt is reloaded on resume —
// without this, a Ctrl+C plus --continue silently loses the user's
// "be skeptical, cite line numbers"-style preamble.
func TestNewCommand_ContinueLoadsAlwaysPromptFromSettings(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	const wantPrompt = "Be skeptical and cite line numbers."
	if err := saveInvestigateSettings(&settings.InvestigateConfig{
		Agents:       []string{"resumed-agent"},
		MaxTurns:     2,
		AlwaysPrompt: wantPrompt,
	}); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(tmp, ".git", "entire-investigations", "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "fedcba654321"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "resumed topic",
		Agents:      []string{"resumed-agent"},
		MaxTurns:    2,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		TimelineDoc: filepath.Join(tmp, "find-timeline.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"resumed-agent"}, []string{"resumed-agent"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if captured.AlwaysPrompt != wantPrompt {
		t.Errorf("LoopInput.AlwaysPrompt = %q, want %q (must survive --continue)", captured.AlwaysPrompt, wantPrompt)
	}
}

// TestNewCommand_ContinueRejectsAgentShrink verifies that resuming with a
// `--agents` override shorter than the persisted NextAgentIdx is refused
// with an actionable error rather than crashing the loop with index-out-
// of-range. Adversarial input (hand-edited state file or careless
// --agents) must not panic.
func TestNewCommand_ContinueRejectsAgentShrink(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	stateDir := filepath.Join(tmp, ".git", "entire-investigations", "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "ababababcdcd"
	st := &investigate.RunState{
		RunID:        runID,
		Topic:        "shrink test",
		Agents:       []string{"a", "b", "c", "d"},
		NextAgentIdx: 3, // points at "d" in the persisted list
		MaxTurns:     2,
		FindingsDoc:  filepath.Join(tmp, "find.md"),
		TimelineDoc:  filepath.Join(tmp, "find-timeline.md"),
		StartingSHA:  "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	deps := newTestDeps(t, []types.AgentName{"a", "b"}, []string{"a", "b"})
	// LoopRun MUST NOT be invoked — we expect the bounds check to short-
	// circuit before reaching the loop.
	deps.LoopRun = func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
		t.Fatal("LoopRun must not run when persisted NextAgentIdx exceeds available agents")
		return investigate.LoopResult{}, nil
	}

	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID, "--agents", "a,b"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for agent-shrink resume")
	}
	if !strings.Contains(errBuf.String(), "exceeds available agents") {
		t.Errorf("stderr should explain the bounds violation; got: %s", errBuf.String())
	}
}

// TestNewCommand_ContinueWarnsOnSettingsLoadFailure verifies that a
// corrupt settings file on resume surfaces a visible warning instead of
// silently dropping the configured AlwaysPrompt. Without this, a user who
// breaks their settings.json mid-run would see the agent's behaviour
// change with no explanation.
func TestNewCommand_ContinueWarnsOnSettingsLoadFailure(t *testing.T) {
	tmp := setupInvestigateRepo(t)

	// Write a malformed settings.json so settings.Load fails.
	if err := os.MkdirAll(filepath.Join(tmp, ".entire"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"), []byte("{broken-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(tmp, ".git", "entire-investigations", "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	store := investigate.NewStateStoreWithDir(stateDir)
	runID := "cdcdcdcdcdcd"
	st := &investigate.RunState{
		RunID:       runID,
		Topic:       "warn test",
		Agents:      []string{"a"},
		MaxTurns:    1,
		FindingsDoc: filepath.Join(tmp, "find.md"),
		TimelineDoc: filepath.Join(tmp, "find-timeline.md"),
		StartingSHA: "deadbeef",
	}
	if err := store.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	captured, runFn := captureLoopRun()
	deps := newTestDeps(t, []types.AgentName{"a"}, []string{"a"})
	deps.LoopRun = runFn

	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", runID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "could not reload settings on --continue") {
		t.Errorf("stderr should warn about settings load failure; got: %s", errBuf.String())
	}
	if captured.AlwaysPrompt != "" {
		t.Errorf("AlwaysPrompt = %q, want empty when settings unavailable", captured.AlwaysPrompt)
	}
}

// TestNewCommand_ContinueWithMissingState surfaces an actionable error.
func TestNewCommand_ContinueWithMissingState(t *testing.T) {
	setupInvestigateRepo(t)

	deps := newTestDeps(t, nil, nil)
	cmd := investigate.NewCommand(deps)
	errBuf := &bytes.Buffer{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(errBuf)
	cmd.SetArgs([]string{"--continue", "abcdef012345"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing run id")
	}
	if !strings.Contains(errBuf.String(), "no run state found") {
		t.Errorf("stderr should mention missing run, got: %s", errBuf.String())
	}
}

// --- helpers ---------------------------------------------------------------

// saveInvestigateSettings writes an InvestigateConfig into the CWD's
// .entire/settings.json. Mirrors review.SaveReviewConfig.
func saveInvestigateSettings(cfg *settings.InvestigateConfig) error {
	ctx := context.Background()
	s, err := settings.Load(ctx)
	if err != nil {
		return err
	}
	if s == nil {
		s = &settings.EntireSettings{}
	}
	s.Investigate = cfg
	return settings.Save(ctx, s)
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNewCommand_RunsMigrationBeforeDispatch(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.json"),
		[]byte(`{"investigate":{"agents":["claude-code"]}}`), 0o644))

	var promptCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              func(err error) error { return err },
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			promptCalled = true
			return false, nil // decline so the command continues without changes
		},
	}

	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--findings"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	// Execute may fail downstream of the migration prompt; the contract
	// here is only that the prompt fires before flag dispatch.
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // see comment above
	require.True(t, promptCalled, "migration prompt must fire before flag dispatch")
}

func TestRunInvestigate_SoftWarnDeclinedReturnsNil(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)

	var loopCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName { return nil },
		NewSilentError:              func(err error) error { return err },
		HeadHasInvestigateCheckpoint: func(_ context.Context) (bool, string) {
			return true, "checkpoint abc123"
		},
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return false, nil // decline
		},
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			loopCalled = true
			return investigate.LoopResult{}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--topic", "foo"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // soft-warn decline must not run the loop
	require.False(t, loopCalled, "loop must not run when user declines soft warn")
}

func TestRunInvestigate_SoftWarnAcceptedRunsLoop(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".entire/settings.local.json"),
		[]byte(`{"investigate":{"agents":["claude-code"],"max_turns":1}}`), 0o644))

	var loopCalled bool
	deps := investigate.Deps{
		GetAgentsWithHooksInstalled: func(_ context.Context) []types.AgentName {
			return []types.AgentName{types.AgentName("claude-code")}
		},
		NewSilentError: func(err error) error { return err },
		SpawnerFor:     func(_ string) spawn.Spawner { return stubSpawner{name: "claude-code"} },
		HeadHasInvestigateCheckpoint: func(_ context.Context) (bool, string) {
			return true, "checkpoint xyz"
		},
		PromptYN: func(_ context.Context, _ string, _ bool) (bool, error) {
			return true, nil // accept
		},
		LoopRun: func(_ context.Context, _ investigate.LoopInput, _ investigate.LoopDeps) (investigate.LoopResult, error) {
			loopCalled = true
			return investigate.LoopResult{Outcome: investigate.OutcomeQuorum}, nil
		},
	}
	cmd := investigate.NewCommand(deps)
	cmd.SetArgs([]string{"--topic", "foo"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.ExecuteContext(context.Background()) //nolint:errcheck // soft-warn accept proceeds; ignore downstream errors
	require.True(t, loopCalled, "loop must run when user accepts soft warn")
}
