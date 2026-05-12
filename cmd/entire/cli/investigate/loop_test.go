package investigate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/stretchr/testify/require"
)

// fakeSpawner is a minimal Spawner used by the loop tests. The constructor
// returns an exec.Cmd that writes a synthetic timeline turn block to the
// timeline file resolved via $ENTIRE_INVESTIGATE_TIMELINE_DOC.
type fakeSpawner struct {
	name       string
	onBuildCmd func(ctx context.Context, env []string, prompt string) *exec.Cmd
}

func (s *fakeSpawner) Name() string { return s.name }

func (s *fakeSpawner) BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd {
	return s.onBuildCmd(ctx, env, prompt)
}

// shellCmd builds an exec.Cmd that runs a /bin/sh script with the supplied
// env. We use /bin/sh for portability; the scripts in this file only use
// POSIX features.
func shellCmd(ctx context.Context, env []string, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = env
	return cmd
}

// appendTurnScript writes a "## Turn $ENTIRE_INVESTIGATE_TURN — <agent>"
// block with the given stance to the timeline file. Used by happy-path
// tests where the loop expects a parseable stance.
func appendTurnScript(agent, stance string) string {
	// Heading uses an em-dash so we exercise the prompt-template format.
	return fmt.Sprintf(
		`{
  printf '\n## Turn %%s — %%s\n**Stance:** %%s\n\n### Changes\n- did things\n\n### Rationale\nbecause\n\n### Open concerns\nnone\n' "$ENTIRE_INVESTIGATE_TURN" "%s" "%s"
} >> "$ENTIRE_INVESTIGATE_TIMELINE_DOC"
`,
		agent, stance,
	)
}

// failScript exits non-zero without touching the timeline.
const failScript = `exit 1`

// noopScript exits 0 without touching the timeline.
const noopScript = `exit 0`

// makeLoopFiles seeds the findings + timeline + transcript dirs for a
// loop test. Returns the absolute paths.
func makeLoopFiles(t *testing.T) (findings, timeline, transcripts string) {
	t.Helper()
	dir := t.TempDir()
	findings = filepath.Join(dir, "findings.md")
	timeline = filepath.Join(dir, "timeline.md")
	transcripts = filepath.Join(dir, "transcripts")
	if err := os.WriteFile(findings, []byte("# Findings\n"), 0o600); err != nil {
		t.Fatalf("write findings: %v", err)
	}
	if err := os.WriteFile(timeline, []byte("# Timeline\n"), 0o600); err != nil {
		t.Fatalf("write timeline: %v", err)
	}
	return findings, timeline, transcripts
}

// stableSpawner returns a SpawnerFor that always uses the supplied script
// for the named agent.
func stableSpawner(scripts map[string]string) func(string) spawn.Spawner {
	return func(agent string) spawn.Spawner {
		script, ok := scripts[agent]
		if !ok {
			return nil
		}
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				return shellCmd(ctx, env, script)
			},
		}
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("loop tests rely on /bin/sh; skipping on Windows")
	}
}

// --- table tests for ParseStanceFromTimeline ------------------------------

func TestParseStanceFromTimeline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		agent      string
		turn       int
		wantStance string
		wantFound  bool
	}{
		{
			name:  "approve_em_dash",
			body:  "## Turn 1 — claude-code\n**Stance:** approve\n",
			agent: "claude-code", turn: 1,
			wantStance: stanceApprove, wantFound: true,
		},
		{
			name:  "request_changes_double_hyphen",
			body:  "## Turn 2 -- codex\n**Stance:** request-changes\n",
			agent: "codex", turn: 2,
			wantStance: stanceRequestChanges, wantFound: true,
		},
		{
			name:  "abstain_single_hyphen",
			body:  "## Turn 3 - gemini-cli\n**Stance:** abstain\n",
			agent: "gemini-cli", turn: 3,
			wantStance: stanceAbstain, wantFound: true,
		},
		{
			name:  "extra_whitespace",
			body:  "##   Turn   4   —   claude-code   \n\n  **Stance:**    approve   \n",
			agent: "claude-code", turn: 4,
			wantStance: stanceApprove, wantFound: true,
		},
		{
			name:  "case_insensitive_keyword",
			body:  "## Turn 5 — claude-code\n**stance:** approve\n",
			agent: "claude-code", turn: 5,
			wantStance: stanceApprove, wantFound: true,
		},
		{
			name:  "unknown_stance_keyword",
			body:  "## Turn 6 — claude-code\n**Stance:** wibble\n",
			agent: "claude-code", turn: 6,
			wantStance: stanceUnknown, wantFound: true,
		},
		{
			name:  "missing_heading",
			body:  "## Turn 7 — claude-code\n**Stance:** approve\n",
			agent: "codex", turn: 7,
			wantStance: stanceUnknown, wantFound: false,
		},
		{
			name:  "missing_stance_line",
			body:  "## Turn 8 — claude-code\n\nsome prose without stance\n## Turn 9 — codex\n**Stance:** approve\n",
			agent: "claude-code", turn: 8,
			wantStance: stanceUnknown, wantFound: true,
		},
		{
			name:  "stance_in_later_block_does_not_leak",
			body:  "## Turn 10 — claude-code\nsome prose only\n## Turn 11 — codex\n**Stance:** approve\n",
			agent: "claude-code", turn: 10,
			wantStance: stanceUnknown, wantFound: true,
		},
		{
			name:  "wrong_turn_number",
			body:  "## Turn 12 — claude-code\n**Stance:** approve\n",
			agent: "claude-code", turn: 13,
			wantStance: stanceUnknown, wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := filepath.Join(t.TempDir(), "timeline.md")
			if err := os.WriteFile(f, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			gotStance, _, gotFound := ParseStanceFromTimeline(f, tc.agent, tc.turn)
			if gotStance != tc.wantStance {
				t.Errorf("stance = %q, want %q", gotStance, tc.wantStance)
			}
			if gotFound != tc.wantFound {
				t.Errorf("found = %v, want %v", gotFound, tc.wantFound)
			}
		})
	}
}

func TestParseStanceFromTimeline_MissingFile(t *testing.T) {
	t.Parallel()
	stance, note, found := ParseStanceFromTimeline(filepath.Join(t.TempDir(), "nope.md"), "claude-code", 1)
	if stance != stanceUnknown {
		t.Errorf("stance = %q, want unknown", stance)
	}
	if found {
		t.Errorf("found = true, want false")
	}
	if note == "" {
		t.Errorf("note = empty, want error description")
	}
}

// --- loop integration tests ----------------------------------------------

func TestRunInvestigateLoop_QuorumReachedFirstRound(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	store := NewStateStoreWithDir(t.TempDir())

	in := LoopInput{
		RunID:       "111111111111",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex", "gemini-cli"},
		MaxTurns:    3,
		Quorum:      0, // default to len(Agents)
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": appendTurnScript("claude-code", "approve"),
			"codex":       appendTurnScript("codex", "approve"),
			"gemini-cli":  appendTurnScript("gemini-cli", "approve"),
		}),
		States:        store,
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeQuorum {
		t.Fatalf("Outcome = %s, want quorum (state.Stances=%+v)", res.Outcome, res.State.Stances)
	}
	if len(res.State.Stances) != 3 {
		t.Errorf("stances = %d, want 3", len(res.State.Stances))
	}
	for i, s := range res.State.Stances {
		if s.Stance != stanceApprove {
			t.Errorf("stance[%d] = %q, want approve", i, s.Stance)
		}
	}
}

func TestRunInvestigateLoop_QuorumDefault(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "222222222222",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		Quorum:      0, // → default to 2
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": appendTurnScript("claude-code", "approve"),
			"codex":       appendTurnScript("codex", "approve"),
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeQuorum {
		t.Fatalf("Outcome = %s, want quorum", res.Outcome)
	}
	if res.State.Quorum != 2 {
		t.Errorf("Quorum = %d, want 2 (default to len(Agents))", res.State.Quorum)
	}
}

func TestRunInvestigateLoop_Stalled(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "333333333333",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    2, // 4 overall turns, never reaching approve quorum
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": appendTurnScript("claude-code", "request-changes"),
			"codex":       appendTurnScript("codex", "request-changes"),
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}
	if res.State.Turn != 4 {
		t.Errorf("Turn = %d, want 4", res.State.Turn)
	}
	if got := len(res.State.Stances); got != 4 {
		t.Errorf("Stances = %d, want 4", got)
	}
}

func TestRunInvestigateLoop_PausedOnTwoFailures(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "444444444444",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3,
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": failScript,
			"codex":       failScript,
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomePaused {
		t.Fatalf("Outcome = %s, want paused", res.Outcome)
	}
	if res.Err == nil {
		t.Errorf("res.Err = nil, want underlying error")
	}
	// Both turns should have run and recorded a stance "unknown" with a
	// note describing the spawn failure.
	if len(res.State.Stances) != 2 {
		t.Errorf("Stances = %d, want 2", len(res.State.Stances))
	}
	for i, s := range res.State.Stances {
		if s.Stance != stanceUnknown {
			t.Errorf("stance[%d] = %q, want unknown", i, s.Stance)
		}
		if !strings.Contains(s.Note, "spawn error") {
			t.Errorf("stance[%d].Note = %q, want spawn-error description", i, s.Note)
		}
	}
}

func TestRunInvestigateLoop_UnknownStanceWhenTimelineMissing(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "555555555555",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1, // 1 overall turn, no quorum possible (Quorum=1 default, but stance unknown)
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": noopScript, // exits 0 without writing the timeline
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	// With one agent, default Quorum=1, but the only stance is "unknown" so
	// no quorum is reached → Stalled at end of turn budget.
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}
	if len(res.State.Stances) != 1 {
		t.Fatalf("Stances = %d, want 1", len(res.State.Stances))
	}
	if got := res.State.Stances[0].Stance; got != stanceUnknown {
		t.Errorf("stance = %q, want unknown", got)
	}
}

// TestBoundedFileWriter verifies the cap-and-mark behaviour the per-turn
// log uses to defend against agents that flood stdout. Writes report
// len(p) so exec.Cmd never sees io.ErrShortWrite (which would tear down
// the agent), but bytes past the limit are dropped and a single truncate
// marker is appended.
func TestBoundedFileWriter(t *testing.T) {
	t.Parallel()

	t.Run("writes under cap pass through", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := newBoundedFileWriter(&buf, 100)
		n, err := w.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if buf.String() != "hello" {
			t.Errorf("buf = %q, want hello", buf.String())
		}
	})

	t.Run("write at cap is fine; next write is dropped with marker", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := newBoundedFileWriter(&buf, 5)
		if _, err := w.Write([]byte("hello")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// Subsequent write must report all bytes consumed but NOT append
		// to the underlying buffer beyond the marker.
		n, err := w.Write([]byte("world more bytes"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len("world more bytes") {
			t.Errorf("n = %d, want %d (must report full consumption to avoid io.ErrShortWrite tearing down exec.Cmd)", n, len("world more bytes"))
		}
		got := buf.String()
		if !strings.HasPrefix(got, "hello") {
			t.Errorf("buf must start with hello; got %q", got)
		}
		if !strings.Contains(got, "[entire: log truncated") {
			t.Errorf("buf must contain truncation marker; got %q", got)
		}
		// A second overflow write must not double-append the marker.
		if _, err := w.Write([]byte("more")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got2 := buf.String(); strings.Count(got2, "[entire: log truncated") != 1 {
			t.Errorf("marker emitted multiple times; got: %q", got2)
		}
	})

	t.Run("partial write at boundary", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := newBoundedFileWriter(&buf, 3)
		// Single write exceeds the cap: take the first 3 bytes, drop the
		// rest, append the marker once.
		n, err := w.Write([]byte("hello world"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len("hello world") {
			t.Errorf("n = %d, want %d", n, len("hello world"))
		}
		if got := buf.String(); !strings.HasPrefix(got, "hel") || !strings.Contains(got, "[entire: log truncated") {
			t.Errorf("expected hel + marker; got %q", got)
		}
	})
}

// TestRunInvestigateLoop_MissingHeadingPausesAfterTwo verifies that an
// agent that exits cleanly but writes no `## Turn N — <agent>` block
// counts as a soft failure: two consecutive missing headings trip
// pause-on-failure rather than burning the whole turn budget silently.
func TestRunInvestigateLoop_MissingHeadingPausesAfterTwo(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "777777777777",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3, // 6 overall turns; pause should fire on turn 2
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			"claude-code": noopScript, // exits 0, never writes timeline
			"codex":       noopScript,
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomePaused {
		t.Fatalf("Outcome = %s, want paused (two consecutive missing-heading failures should pause)", res.Outcome)
	}
	if got := len(res.State.Stances); got != 2 {
		t.Fatalf("Stances = %d, want 2 (loop should pause after the second consecutive failure)", got)
	}
}

func TestRunInvestigateLoop_PersistsStateEachTurn(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	storeDir := t.TempDir()
	in := LoopInput{
		RunID:       "666666666666",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    1, // 2 overall turns, request-changes → Stalled
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}

	var counter int32
	// Wrap stableSpawner so the test can observe a fresh load between turns.
	spawnerFor := func(agent string) spawn.Spawner {
		script := appendTurnScript(agent, "request-changes")
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				cmd := shellCmd(ctx, env, script)
				atomic.AddInt32(&counter, 1)
				return cmd
			},
		}
	}

	deps := LoopDeps{
		SpawnerFor:    spawnerFor,
		States:        NewStateStoreWithDir(storeDir),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if res.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %s, want stalled", res.Outcome)
	}

	// A fresh StateStore in the same dir should see all stances.
	fresh := NewStateStoreWithDir(storeDir)
	loaded, err := fresh.Load(context.Background(), in.RunID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if len(loaded.Stances) != 2 {
		t.Errorf("loaded stances = %d, want 2", len(loaded.Stances))
	}
	if loaded.Turn != 2 {
		t.Errorf("Turn = %d, want 2", loaded.Turn)
	}
}

func TestRunInvestigateLoop_Resume(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	store := NewStateStoreWithDir(t.TempDir())

	// Pre-existing state: agent[0] has already gone in turn 1 and approved.
	resumeState := &RunState{
		RunID:           "777777777777",
		Topic:           "test",
		Agents:          []string{"claude-code", "codex"},
		MaxTurns:        1, // Already used by claude-code, so codex's only turn closes round 1
		Quorum:          2,
		CompletedRounds: 0,
		Turn:            1,
		NextAgentIdx:    1, // Next is codex.
		Stances: []TurnStance{
			{Round: 1, Turn: 1, Agent: "claude-code", Stance: stanceApprove},
		},
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
		StartedAt:   time.Now().Add(-time.Hour),
	}

	in := LoopInput{
		RunID:       resumeState.RunID,
		Topic:       resumeState.Topic,
		Agents:      resumeState.Agents,
		MaxTurns:    1,
		Quorum:      2,
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
		Resume:      resumeState,
	}

	var observedAgent string
	spawnerFor := func(agent string) spawn.Spawner {
		return &fakeSpawner{
			name: agent,
			onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
				observedAgent = agent
				return shellCmd(ctx, env, appendTurnScript(agent, "approve"))
			},
		}
	}

	deps := LoopDeps{
		SpawnerFor:    spawnerFor,
		States:        store,
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if observedAgent != "codex" {
		t.Errorf("first spawned agent on resume = %q, want codex", observedAgent)
	}
	if res.Outcome != OutcomeQuorum {
		t.Errorf("Outcome = %s, want quorum (resume completed round)", res.Outcome)
	}
}

func TestRunInvestigateLoop_PlanAndTimelineChangedFlags(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "888888888888",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1,
		Quorum:      1,
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: stableSpawner(map[string]string{
			// Only modify the timeline; findings stays untouched.
			"claude-code": appendTurnScript("claude-code", "approve"),
		}),
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	res, err := RunInvestigateLoop(context.Background(), in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	if len(res.State.Stances) != 1 {
		t.Fatalf("Stances = %d, want 1", len(res.State.Stances))
	}
	s := res.State.Stances[0]
	if s.PlanChanged {
		t.Errorf("PlanChanged = true, want false (findings was not edited)")
	}
	if !s.TimelineChanged {
		t.Errorf("TimelineChanged = false, want true (timeline was appended)")
	}
}

func TestRunInvestigateLoop_CancelledContext(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)
	in := LoopInput{
		RunID:       "999999999999",
		Topic:       "test",
		Agents:      []string{"claude-code", "codex"},
		MaxTurns:    3,
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	// Spawner returns a script that sleeps long enough to be cancelled.
	deps := LoopDeps{
		SpawnerFor: func(agent string) spawn.Spawner {
			return &fakeSpawner{
				name: agent,
				onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
					// 30s sleep; the test cancels after 50ms.
					return shellCmd(ctx, env, "sleep 30")
				},
			}
		},
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	res, err := RunInvestigateLoop(ctx, in, deps)
	if err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	// Either the in-progress turn was aborted (recorded as failure) and the
	// outer loop saw ctx.Err on next iteration → Cancelled, or the loop
	// drained back to OutcomePaused after two consecutive ctx-driven
	// failures. Both are acceptable terminal states; we only assert that
	// the loop terminated and persisted state.
	if res.Outcome != OutcomeCancelled && res.Outcome != OutcomePaused {
		t.Errorf("Outcome = %s, want cancelled or paused", res.Outcome)
	}
	if res.State == nil {
		t.Fatalf("State is nil")
	}
}

// TestRunInvestigateLoop_WritesPerTurnLogFile verifies that agent stdout is
// captured to the per-turn log file on disk. The previous --verbose flag
// (which also teed stdout to the terminal) was removed when the TUI became
// the default user-facing surface; the on-disk log is now the canonical
// place to inspect raw agent output.
func TestRunInvestigateLoop_WritesPerTurnLogFile(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)

	findings, timeline, transcripts := makeLoopFiles(t)

	in := LoopInput{
		RunID:       "aaaaaaaaaaaa",
		Topic:       "test",
		Agents:      []string{"claude-code"},
		MaxTurns:    1,
		Quorum:      1,
		FindingsDoc: findings,
		TimelineDoc: timeline,
		StartingSHA: "deadbeef",
	}
	deps := LoopDeps{
		SpawnerFor: func(agent string) spawn.Spawner {
			script := `echo SENTINEL_OUTPUT
` + appendTurnScript(agent, "approve")
			return &fakeSpawner{
				name: agent,
				onBuildCmd: func(ctx context.Context, env []string, _ string) *exec.Cmd {
					return shellCmd(ctx, env, script)
				},
			}
		},
		States:        NewStateStoreWithDir(t.TempDir()),
		TranscriptDir: transcripts,
	}

	if _, err := RunInvestigateLoop(context.Background(), in, deps); err != nil {
		t.Fatalf("RunInvestigateLoop: %v", err)
	}
	logPath := filepath.Join(transcripts, in.RunID, "turn-1-claude-code.log")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(body), "SENTINEL_OUTPUT") {
		t.Errorf("turn log file did not capture stdout: %q", string(body))
	}
}

func TestRunInvestigateLoop_RejectsInvalidInput(t *testing.T) {
	t.Parallel()
	store := NewStateStoreWithDir(t.TempDir())
	deps := LoopDeps{
		SpawnerFor:    func(string) spawn.Spawner { return nil },
		States:        store,
		TranscriptDir: t.TempDir(),
	}
	cases := []struct {
		name string
		in   LoopInput
	}{
		{"bad_run_id", LoopInput{RunID: "not-hex", Agents: []string{"a"}, FindingsDoc: "f", TimelineDoc: "t"}},
		{"empty_agents", LoopInput{RunID: "aaaaaaaaaaaa", Agents: nil, FindingsDoc: "f", TimelineDoc: "t"}},
		{"empty_findings", LoopInput{RunID: "aaaaaaaaaaaa", Agents: []string{"a"}, TimelineDoc: "t"}},
		{"empty_timeline", LoopInput{RunID: "aaaaaaaaaaaa", Agents: []string{"a"}, FindingsDoc: "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := RunInvestigateLoop(context.Background(), tc.in, deps)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestParseFindingsPreview_FirstNonEmptyLine(t *testing.T) {
	t.Parallel()
	body := "\n\n**Stance:** approve\n\nFound a missing nil check in pkg/foo.\nAnd more context.\n"
	got := parseFindingsPreview(body)
	require.Equal(t, "Found a missing nil check in pkg/foo.", got)
}

func TestParseFindingsPreview_EmptyBody(t *testing.T) {
	t.Parallel()
	require.Empty(t, parseFindingsPreview(""))
}

func TestParseFindingsPreview_OnlyStance(t *testing.T) {
	t.Parallel()
	require.Empty(t, parseFindingsPreview("**Stance:** approve\n"))
}

func TestParseFindingsPreview_TruncatesLongLine(t *testing.T) {
	t.Parallel()
	long := "lead " + strings.Repeat("x", 500)
	got := parseFindingsPreview(long)
	require.LessOrEqual(t, len(got), 200)
	require.True(t, strings.HasPrefix(got, "lead "))
}
