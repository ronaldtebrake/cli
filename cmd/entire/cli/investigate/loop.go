package investigate

// loop.go implements the round-robin investigation loop driver.
//
// The loop runs a fixed list of agents in a strict round-robin order. For
// each turn it:
//
//  1. Hashes the findings + timeline files (SHA-256) BEFORE the turn.
//  2. Composes a prompt via ComposeInvestigatePrompt.
//  3. Spawns the agent via Spawner.BuildCmd with ENTIRE_INVESTIGATE_* env
//     populated by AppendInvestigateEnv.
//  4. Tees the agent's stdout to a per-turn log file on disk.
//  5. Waits for the agent to exit. Re-hashes the docs.
//  6. Parses the timeline doc for the freshly-added "## Turn N — <agent>"
//     block and reads its "**Stance:**" line.
//  7. Records a TurnStance in the persisted RunState and notifies the
//     ProgressSink (TUI dashboard or headless text writer).
//  8. Decides whether to terminate (quorum, stalled, paused, cancelled) or
//     advance to the next agent.
//
// The loop is single-threaded: each turn waits for the previous to exit
// before starting. This keeps the timeline file's append-order
// deterministic and avoids racing two agents on the same shared docs.
//
// Privacy note (per CLAUDE.md): operational metadata only is ever logged.
// Prompts, file bodies, agent stdout, and commit messages are NEVER logged.
// The per-turn log file is written to disk for the user's own debugging
// but never touched by logging.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// LoopDeps collects the runtime-injectable hooks RunInvestigateLoop needs.
// Production code passes real implementations; tests inject fakes.
type LoopDeps struct {
	// SpawnerFor maps an agent name → Spawner. Returns nil for an unknown
	// agent name, in which case the loop pauses with an error.
	SpawnerFor func(agentName string) spawn.Spawner

	// States persists/loads RunState across turns. In production this is a
	// real *StateStore rooted at <git-common-dir>/entire-investigations/state;
	// tests pass NewStateStoreWithDir(t.TempDir()).
	States *StateStore

	// TranscriptDir is the directory where per-turn agent stdout logs are
	// written. Layout: <TranscriptDir>/<run-id>/turn-<N>-<agent>.log
	TranscriptDir string

	// Progress receives turn lifecycle events. Production wires either a
	// tuiProgressSink (TTY) or textProgressSink (non-TTY); tests typically
	// inject a fake recorder. nil is treated as nullProgressSink (no-op).
	Progress ProgressSink

	// Now returns the current time. Defaults to time.Now if nil.
	Now func() time.Time
}

// LoopInput carries everything RunInvestigateLoop needs that isn't a hook.
type LoopInput struct {
	RunID        string    // 12-hex
	Topic        string    // human-readable subject of the investigation
	Agents       []string  // ordered, length >= 1
	MaxTurns     int       // per-agent turn budget; 0 → 3
	Quorum       int       // approvals needed; 0 → len(Agents)
	AlwaysPrompt string    // optional, appended verbatim to every prompt
	FindingsDoc  string    // absolute path
	TimelineDoc  string    // absolute path
	StartingSHA  string    // git HEAD when `entire investigate` was invoked
	PriorContext string    // optional, e.g. "## Prior Entire Context" excerpt
	Resume       *RunState // when non-nil, resume from this state
}

// LoopOutcome describes how the loop ended.
type LoopOutcome string

const (
	// OutcomeQuorum means the most recent completed round produced enough
	// approve stances to meet Quorum. The loop ends successfully.
	OutcomeQuorum LoopOutcome = "quorum"
	// OutcomeStalled means the per-agent turn budget was exhausted without
	// reaching quorum. The investigation produced findings but no
	// consensus.
	OutcomeStalled LoopOutcome = "stalled"
	// OutcomePaused means two consecutive agent invocations failed (process
	// error, non-zero exit). The loop stops so the user can investigate;
	// state is preserved for `--continue`.
	OutcomePaused LoopOutcome = "paused"
	// OutcomeCancelled means the context was cancelled (Ctrl+C, parent
	// command shutdown). State is preserved for resume.
	OutcomeCancelled LoopOutcome = "cancelled"
)

// LoopResult is the loop's final report.
type LoopResult struct {
	Outcome LoopOutcome
	State   *RunState
	// Err holds the most recent per-turn spawn error, if any. Informational:
	// when Outcome is Quorum/Stalled it is typically nil. When Outcome is
	// Paused this surfaces the underlying agent failure.
	Err error
}

// pauseAfterConsecutiveFailures is the number of back-to-back per-turn
// agent failures that trigger OutcomePaused. Two is the marvin convention:
// one failure could be transient, two strongly suggests a configuration
// problem the user must fix before continuing.
const pauseAfterConsecutiveFailures = 2

// defaultMaxTurns is the per-agent turn budget when LoopInput.MaxTurns is 0.
const defaultMaxTurns = 3

// turnLogFileMode is the file mode for per-turn agent stdout log files.
// Owner read/write, group read; matches the .entire/ permissions convention.
const turnLogFileMode os.FileMode = 0o640

// turnLogDirMode is the directory mode for the per-run transcript folder.
const turnLogDirMode os.FileMode = 0o750

// stanceApprove and friends pin the stance vocabulary so callers can compare
// without typo risk. The timeline parser normalises to one of these or
// "unknown".
const (
	stanceApprove        = "approve"
	stanceRequestChanges = "request-changes"
	stanceAbstain        = "abstain"
	stanceUnknown        = "unknown"
)

// RunInvestigateLoop runs the round-robin investigation loop until it
// reaches quorum, stalls, gets paused, or the context is cancelled.
//
// On every turn the function persists state via deps.States so a crash mid-
// turn leaves a recoverable RunState on disk. The returned LoopResult is
// always populated, even on context cancellation.
//
// The function returns (result, error) where error is non-nil only for
// programmer errors (invalid input, missing dependencies). Per-turn agent
// failures are reflected in result.Outcome and result.Err, not the return
// error.
func RunInvestigateLoop(ctx context.Context, in LoopInput, deps LoopDeps) (LoopResult, error) {
	if err := validateLoopInput(in); err != nil {
		return LoopResult{}, err
	}
	if deps.States == nil {
		return LoopResult{}, errors.New("LoopDeps.States is required")
	}
	if deps.SpawnerFor == nil {
		return LoopResult{}, errors.New("LoopDeps.SpawnerFor is required")
	}
	if deps.TranscriptDir == "" {
		return LoopResult{}, errors.New("LoopDeps.TranscriptDir is required")
	}
	if deps.Progress == nil {
		deps.Progress = nullProgressSink{}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	maxTurnsPerAgent := in.MaxTurns
	if maxTurnsPerAgent == 0 {
		maxTurnsPerAgent = defaultMaxTurns
	}
	quorum := in.Quorum
	if quorum == 0 {
		quorum = len(in.Agents)
	}
	maxOverall := maxTurnsPerAgent * len(in.Agents)

	state := initLoopState(in, now, maxTurnsPerAgent, quorum)

	// Persist the initial state once so external observers (status command,
	// `--continue`) see the run as soon as the loop starts.
	if err := deps.States.Save(ctx, state); err != nil {
		return LoopResult{State: state}, fmt.Errorf("save initial run state: %w", err)
	}

	transcriptRunDir := filepath.Join(deps.TranscriptDir, in.RunID)
	if err := os.MkdirAll(transcriptRunDir, turnLogDirMode); err != nil {
		return LoopResult{State: state}, fmt.Errorf("create transcript dir: %w", err)
	}

	cfg := turnConfig{
		input:            in,
		deps:             deps,
		now:              now,
		quorum:           quorum,
		maxPerAgent:      maxTurnsPerAgent,
		transcriptRunDir: transcriptRunDir,
	}
	consecutiveFails := 0
	var lastErr error

	for state.Turn < maxOverall {
		if ctx.Err() != nil {
			// Cancellation is a normal terminal outcome surfaced through
			// LoopResult.Outcome; the linter's nilerr flag would prefer we
			// return ctxErr, but the contract is "always-returns-result,
			// error only for programmer bugs".
			deps.Progress.RunFinished(OutcomeCancelled)
			//nolint:nilerr // ctx cancellation is reported via Outcome, not the error return
			return LoopResult{Outcome: OutcomeCancelled, State: state, Err: lastErr}, nil
		}

		outcome := runOneTurn(ctx, cfg, state)
		if outcome.failed {
			lastErr = outcome.err
			consecutiveFails++
			if consecutiveFails >= pauseAfterConsecutiveFailures {
				deps.Progress.RunFinished(OutcomePaused)
				return LoopResult{Outcome: OutcomePaused, State: state, Err: lastErr}, nil
			}
		} else {
			consecutiveFails = 0
		}
		advanceAgent(state)
		if saveErr := deps.States.Save(ctx, state); saveErr != nil {
			logging.Debug(ctx, "investigate: save state after turn",
				sErr(saveErr), sRun(in.RunID))
		}
		if state.NextAgentIdx == 0 {
			if approveCountInRound(state.Stances, outcome.round) >= quorum {
				deps.Progress.RunFinished(OutcomeQuorum)
				return LoopResult{Outcome: OutcomeQuorum, State: state, Err: nil}, nil
			}
		}
	}

	deps.Progress.RunFinished(OutcomeStalled)
	return LoopResult{Outcome: OutcomeStalled, State: state, Err: lastErr}, nil
}

// turnConfig packages the immutable knobs runOneTurn needs. Splitting these
// out from RunInvestigateLoop's call frame keeps the per-turn helper
// signature tight without re-deriving values on every iteration.
type turnConfig struct {
	input            LoopInput
	deps             LoopDeps
	now              func() time.Time
	quorum           int
	maxPerAgent      int
	transcriptRunDir string
}

// turnOutcome reports the post-turn state runOneTurn produces. The loop
// uses these flags to drive the consecutive-failure counter and the round
// boundary check; every other side effect (state mutation, persistence,
// logging) happens inside runOneTurn.
type turnOutcome struct {
	round  int
	failed bool
	err    error
}

// runOneTurn executes a single agent turn and records the resulting
// TurnStance on state.Stances. It mutates state.Turn (incremented) and
// state.Stances; the caller is responsible for advanceAgent and the post-
// turn Save.
func runOneTurn(ctx context.Context, cfg turnConfig, state *RunState) turnOutcome {
	in := cfg.input
	deps := cfg.deps
	agentName := state.Agents[state.NextAgentIdx]
	state.Turn++
	round := ((state.Turn - 1) / len(state.Agents)) + 1

	preFindings := hashFile(ctx, in.FindingsDoc)
	preTimeline := hashFile(ctx, in.TimelineDoc)

	deps.Progress.TurnStarted(agentName, state.Turn, round, cfg.maxPerAgent)

	spawner := deps.SpawnerFor(agentName)
	if spawner == nil {
		err := fmt.Errorf("no spawner for agent %q", agentName)
		recordFailureStance(state, round, agentName, err, cfg.now)
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, 0, true, err, "")
		return turnOutcome{round: round, failed: true, err: err}
	}

	prompt := ComposeInvestigatePrompt(ComposeInput{
		Topic:        in.Topic,
		AgentName:    agentName,
		Round:        round,
		Turn:         state.Turn,
		AlwaysPrompt: in.AlwaysPrompt,
		Files:        Files{Findings: in.FindingsDoc, Timeline: in.TimelineDoc},
		PriorContext: in.PriorContext,
	})
	env := AppendInvestigateEnv(os.Environ(), AppendOptions{
		AgentName:   agentName,
		RunID:       in.RunID,
		Round:       round,
		Turn:        state.Turn,
		Topic:       in.Topic,
		Prompt:      prompt,
		FindingsDoc: in.FindingsDoc,
		TimelineDoc: in.TimelineDoc,
		StartingSHA: in.StartingSHA,
	})
	cmd := spawner.BuildCmd(ctx, env, prompt)

	logPath := filepath.Join(cfg.transcriptRunDir, fmt.Sprintf("turn-%d-%s.log", state.Turn, agentName))
	logFile, err := openTurnLog(logPath)
	if err != nil {
		recordFailureStance(state, round, agentName, err, cfg.now)
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, 0, true, err, "")
		return turnOutcome{round: round, failed: true, err: err}
	}

	// Wrap the log file in a size-capped writer so a hostile or
	// misbehaving agent cannot fill the disk via runaway stdout. The TUI
	// dashboard owns the user-facing live view; the per-turn log file is
	// the canonical place for inspecting raw agent stdout/stderr.
	cappedLog := newBoundedFileWriter(logFile, maxTurnLogBytes)
	cmd.Stdout = cappedLog
	cmd.Stderr = cappedLog

	logging.Info(ctx, "investigate: turn start",
		sRun(in.RunID), sAgent(agentName), sTurn(state.Turn), sRound(round))

	turnStart := cfg.now()
	runErr := cmd.Run()
	turnDuration := cfg.now().Sub(turnStart)
	if closeErr := logFile.Close(); closeErr != nil {
		logging.Debug(ctx, "investigate: close turn log",
			sErr(closeErr), sRun(in.RunID), sAgent(agentName))
	}

	postFindings := hashFile(ctx, in.FindingsDoc)
	postTimeline := hashFile(ctx, in.TimelineDoc)
	stance, note, headingFound := ParseStanceFromTimeline(in.TimelineDoc, agentName, state.Turn)

	turn := TurnStance{
		Round:           round,
		Turn:            state.Turn,
		Agent:           agentName,
		Stance:          stance,
		PlanChanged:     preFindings != postFindings,
		TimelineChanged: preTimeline != postTimeline,
		Note:            note,
	}
	if runErr != nil {
		turn.Stance = stanceUnknown
		turn.Note = "spawn error: " + runErr.Error()
		state.Stances = append(state.Stances, turn)
		updateRoundCounter(state)
		state.UpdatedAt = cfg.now()
		logging.Warn(ctx, "investigate: turn failed",
			sRun(in.RunID), sAgent(agentName),
			sTurn(state.Turn), sRound(round),
			slogString("err", runErr.Error()))
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, turnDuration, true, runErr, "")
		return turnOutcome{round: round, failed: true, err: runErr}
	}

	state.Stances = append(state.Stances, turn)
	updateRoundCounter(state)
	state.UpdatedAt = cfg.now()
	logging.Info(ctx, "investigate: turn end",
		sRun(in.RunID), sAgent(agentName),
		sTurn(state.Turn), sRound(round),
		slogString("stance", stance),
		slogBool("plan_changed", turn.PlanChanged),
		slogBool("timeline_changed", turn.TimelineChanged))
	// Treat a missing heading as a soft failure: the agent ran cleanly but
	// produced no structured output, so it should not count toward quorum
	// and consecutive misses must trip pause-on-failure. The TurnStance is
	// still recorded for diagnostics, but the loop sees this as a failure
	// for budget-control purposes.
	if !headingFound {
		logging.Warn(ctx, "investigate: turn missing heading",
			sRun(in.RunID), sAgent(agentName),
			sTurn(state.Turn), sRound(round))
		missingHeading := errors.New("agent did not write a turn heading")
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, turnDuration, true, missingHeading, "")
		return turnOutcome{round: round, failed: true, err: missingHeading}
	}
	body, _ := findTurnBlock(readTimelineFile(in.TimelineDoc), agentName, state.Turn)
	preview := parseFindingsPreview(body)
	deps.Progress.TurnFinished(agentName, state.Turn, stance, turnDuration, false, nil, preview)
	return turnOutcome{round: round, failed: false}
}

// validateLoopInput rejects programmer errors before the loop starts. We
// treat these as bugs in the caller, not user errors, so they short-circuit
// with a plain error rather than entering the OutcomePaused/Stalled paths.
func validateLoopInput(in LoopInput) error {
	if err := validateRunID(in.RunID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	if len(in.Agents) == 0 {
		return errors.New("at least one agent is required")
	}
	if in.FindingsDoc == "" {
		return errors.New("FindingsDoc is required")
	}
	if in.TimelineDoc == "" {
		return errors.New("TimelineDoc is required")
	}
	return nil
}

// initLoopState builds the starting RunState. When in.Resume is non-nil we
// take its turn/round/idx + accumulated stances; otherwise we initialise a
// fresh state with Turn=0, NextAgentIdx=0.
func initLoopState(in LoopInput, now func() time.Time, maxTurns, quorum int) *RunState {
	if in.Resume != nil {
		st := *in.Resume
		// Always use the LoopInput's RunID/Agents/etc — Resume is a
		// snapshot, but the caller is the source of truth for run config.
		st.RunID = in.RunID
		st.Topic = in.Topic
		st.Agents = append([]string(nil), in.Agents...)
		st.MaxTurns = maxTurns
		st.Quorum = quorum
		st.FindingsDoc = in.FindingsDoc
		st.TimelineDoc = in.TimelineDoc
		st.StartingSHA = in.StartingSHA
		if st.StartedAt.IsZero() {
			st.StartedAt = now()
		}
		st.UpdatedAt = now()
		return &st
	}
	t := now()
	return &RunState{
		RunID:           in.RunID,
		Topic:           in.Topic,
		Agents:          append([]string(nil), in.Agents...),
		MaxTurns:        maxTurns,
		Quorum:          quorum,
		CompletedRounds: 0,
		Turn:            0,
		NextAgentIdx:    0,
		FindingsDoc:     in.FindingsDoc,
		TimelineDoc:     in.TimelineDoc,
		StartingSHA:     in.StartingSHA,
		StartedAt:       t,
		UpdatedAt:       t,
	}
}

// advanceAgent rolls NextAgentIdx forward modulo the agent count.
func advanceAgent(state *RunState) {
	state.NextAgentIdx = (state.NextAgentIdx + 1) % len(state.Agents)
}

// updateRoundCounter recomputes state.CompletedRounds from the current Turn.
// With N agents:
//   - Turn 1..N → round 1 in progress, completed rounds = 0
//   - Turn N+1..2N → round 2 in progress, completed rounds = 1
//
// Persisting completed-rounds keeps `entire investigate status` honest:
// the user sees how many full passes have actually happened. The
// per-stance Round (TurnStance.Round) is 1-indexed and tracks the round
// each individual turn belongs to — the two fields are not interchangeable.
func updateRoundCounter(state *RunState) {
	state.CompletedRounds = state.Turn / len(state.Agents)
}

// approveCountInRound returns how many stances in the given round are
// "approve". We scan the slice rather than looking only at the tail so
// resumed runs (whose Stances slice may include earlier rounds) compute the
// right count.
func approveCountInRound(stances []TurnStance, round int) int {
	n := 0
	for _, s := range stances {
		if s.Round == round && s.Stance == stanceApprove {
			n++
		}
	}
	return n
}

// recordFailureStance appends a TurnStance with Stance="unknown" and a Note
// describing the failure. Used when we couldn't even spawn the agent
// (no spawner, log-file open error). PlanChanged/TimelineChanged are false
// because nothing ran.
func recordFailureStance(state *RunState, round int, agent string, err error, now func() time.Time) {
	state.Stances = append(state.Stances, TurnStance{
		Round:  round,
		Turn:   state.Turn,
		Agent:  agent,
		Stance: stanceUnknown,
		Note:   "spawn error: " + err.Error(),
	})
	updateRoundCounter(state)
	state.UpdatedAt = now()
}

// openTurnLog opens (or truncates) the per-turn log file. We always
// truncate so re-runs of the same turn (e.g. after a crash and resume that
// re-uses the turn number) overwrite cleanly rather than concatenating.
//
// Concurrency note: running two `entire investigate --continue <run-id>`
// invocations against the same run from different shells is not supported.
// The state file (`.git/entire-investigations/state/<run-id>.json`) uses
// atomic temp+rename writes, but per-turn logs use O_TRUNC without file
// locking, so concurrent writers would race here. Single-shell continue is
// the supported path; concurrent runs must use distinct run IDs.
func openTurnLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, turnLogFileMode) //nolint:gosec // path is composed from validated runID + turn + agent
	if err != nil {
		return nil, fmt.Errorf("open turn log %s: %w", path, err)
	}
	return f, nil
}

// maxTurnLogBytes caps how much agent stdout/stderr we persist to a
// per-turn log file. A misbehaving or hostile agent (looping `yes`-style
// output, runaway tool calls, prompt-injected verbose echo) can otherwise
// fill the user's disk before the loop notices, since `cmd.Stdout` is
// io.MultiWriter'd into an uncapped *os.File. 16 MiB is comfortably more
// than any realistic per-turn agent transcript and small enough to stay
// well under disk-pressure thresholds even if every concurrent worktree
// hits the cap simultaneously.
const maxTurnLogBytes = 16 * 1024 * 1024

// boundedFileWriter wraps an io.Writer (the per-turn log file plus
// optional verbose tee) with a hard byte cap. Writes past the cap are
// silently discarded; on the first overflow a single "[entire: log
// truncated at N bytes]" marker is emitted so a reader can tell that
// truncation happened. Errors from the underlying writer are returned
// (caller controls behaviour); we do not return io.ErrShortWrite when
// dropping bytes because exec.Cmd treats short writes as I/O failures
// and would tear down the agent process on every stdout write past the
// cap. The contract is: report len(p) bytes consumed, drop the
// out-of-budget tail.
type boundedFileWriter struct {
	w         io.Writer
	limit     int
	written   int
	truncated bool
}

func newBoundedFileWriter(w io.Writer, limit int) *boundedFileWriter {
	return &boundedFileWriter{w: w, limit: limit}
}

func (b *boundedFileWriter) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.written
	if remaining <= 0 {
		// Cap already hit; emit marker on the first such call, then drop.
		b.emitTruncationMarker()
		return len(p), nil
	}
	if len(p) <= remaining {
		n, err := b.w.Write(p)
		b.written += n
		if err != nil {
			return n, fmt.Errorf("write turn log: %w", err)
		}
		return n, nil
	}
	// Partial write: take what fits, then emit the truncation marker once.
	n, err := b.w.Write(p[:remaining])
	b.written += n
	if err != nil {
		return n, fmt.Errorf("write turn log: %w", err)
	}
	b.emitTruncationMarker()
	// Report full p consumed so the agent process never sees a short-write
	// signal. The tail is intentionally dropped.
	return len(p), nil
}

// emitTruncationMarker appends the "[entire: log truncated at N bytes]"
// marker exactly once. Subsequent calls are no-ops, keeping the marker
// from drowning out the actual truncated content.
func (b *boundedFileWriter) emitTruncationMarker() {
	if b.truncated {
		return
	}
	b.truncated = true
	marker := fmt.Sprintf("\n[entire: log truncated at %d bytes]\n", b.limit)
	_, _ = b.w.Write([]byte(marker)) //nolint:errcheck // best-effort marker; primary write already succeeded
}

// hashFile returns the SHA-256 of the file at path as a hex string. Returns
// the empty string when the file is missing or unreadable; in that case we
// also emit a Debug log so operators can spot doc-path misconfiguration.
//
// We deliberately do not surface the error: the loop should keep running
// even if the agent has not yet created the findings file (turn 1 of a new
// run usually creates it from a template), and a missing file is detected
// downstream by comparing the empty hash before vs. the post-turn hash.
func hashFile(ctx context.Context, path string) string {
	f, err := os.Open(path) //nolint:gosec // path comes from validated LoopInput
	if err != nil {
		logging.Debug(ctx, "investigate: hash file open failed",
			slogString("path", path), sErr(err))
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		logging.Debug(ctx, "investigate: hash file read failed",
			slogString("path", path), sErr(err))
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// turnHeadingPattern matches a "## Turn <N> — <agent>" markdown heading
// where the dash separator may be an em-dash (—), an en-dash (–), a double-
// hyphen (--), or a single hyphen (-). Capture group 1 is the turn number,
// capture group 2 is the agent name.
//
// We require column 0 (no leading whitespace) on the "##" because the
// prompt explicitly asks the agent to put the heading at column 0; this
// guards against accidentally matching a "## Turn N" inside a code block
// that happens to be at the start of a line of indented prose.
var turnHeadingPattern = regexp.MustCompile(
	`(?m)^##\s+Turn\s+(\d+)\s+(?:—|–|--|-)\s+(\S.*?)\s*$`,
)

// nextHeadingPattern matches the next "## " heading at column 0. Used to
// scope a turn's body so we don't pick up a Stance line from a later turn.
var nextHeadingPattern = regexp.MustCompile(`(?m)^##\s+`)

// stanceLinePattern matches a markdown "**Stance:**" line. Case-insensitive
// on the keyword. The captured stance value (group 1) is then normalised
// against the known vocabulary.
var stanceLinePattern = regexp.MustCompile(`(?im)^\s*\*\*Stance:\*\*\s*([A-Za-z][A-Za-z0-9_-]*)`)

// ParseStanceFromTimeline reads the timeline file at path and extracts the
// stance recorded by `agent` for the overall turn `turn`. It returns:
//
//   - stance: one of "approve", "request-changes", "abstain", or "unknown".
//   - note: empty on success; "timeline missing stance" when the heading
//     was found but no Stance line was present in that block; or a similar
//     short diagnostic when the heading itself was missing.
//   - found: true iff a "## Turn N — <agent>" block was located. False does
//     not imply error — it's the normal outcome when the agent failed to
//     write its turn entry.
//
// The function is I/O-only against the given path; it has no other
// dependencies and is safe to call from tests.
func ParseStanceFromTimeline(path, agent string, turn int) (stance, note string, found bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the configured timeline doc
	if err != nil {
		return stanceUnknown, "timeline read error: " + err.Error(), false
	}
	body, ok := findTurnBlock(string(data), agent, turn)
	if !ok {
		return stanceUnknown, "timeline missing stance", false
	}
	m := stanceLinePattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return stanceUnknown, "timeline missing stance", true
	}
	return normaliseStance(m[1]), "", true
}

// findTurnBlock isolates the body between the matching "## Turn N — <agent>"
// heading and the next "## " heading (or EOF). Returns ("", false) when
// the heading is not found.
//
// Agent names occasionally pick up trailing whitespace or a stray suffix
// in agent prompts; we compare normalised forms (trimmed, lowercased) so
// "Claude-Code", "claude-code ", and "claude-code" all match.
func findTurnBlock(text, agent string, turn int) (string, bool) {
	matches := turnHeadingPattern.FindAllStringSubmatchIndex(text, -1)
	wantAgent := strings.ToLower(strings.TrimSpace(agent))
	for i, m := range matches {
		// m: [matchStart, matchEnd, turnStart, turnEnd, agentStart, agentEnd]
		gotTurn := text[m[2]:m[3]]
		gotAgent := strings.ToLower(strings.TrimSpace(text[m[4]:m[5]]))
		if gotTurn != strconv.Itoa(turn) || gotAgent != wantAgent {
			continue
		}
		// Body starts at the end of this heading line. End is either the
		// next "## " heading OR EOF.
		bodyStart := m[1]
		bodyEnd := len(text)
		if i+1 < len(matches) {
			bodyEnd = matches[i+1][0]
		} else {
			// Could still be a non-Turn "## " heading after this block.
			rel := nextHeadingPattern.FindStringIndex(text[bodyStart:])
			if rel != nil {
				bodyEnd = bodyStart + rel[0]
			}
		}
		return text[bodyStart:bodyEnd], true
	}
	return "", false
}

// normaliseStance maps the raw value captured from the timeline to the
// canonical vocabulary, returning "unknown" for anything else.
func normaliseStance(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case stanceApprove:
		return stanceApprove
	case stanceRequestChanges, "requestchanges", "request_changes":
		return stanceRequestChanges
	case stanceAbstain:
		return stanceAbstain
	default:
		return stanceUnknown
	}
}

// parseFindingsPreview returns the first non-empty non-stance line from
// the body of a turn block. Used to feed the drill-in TUI a one-line
// preview of what the agent wrote. Returns "" when the body has only
// blank lines or only the stance line. Lines longer than 200 cells are
// truncated.
func parseFindingsPreview(body string) string {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if stanceLinePattern.MatchString(line) {
			continue
		}
		if len(line) > 200 {
			return line[:200]
		}
		return line
	}
	return ""
}

// readTimelineFile returns the contents of the timeline doc or "" on
// error. Used by the success path's preview parsing — a missing file
// just means no preview, which is fine.
func readTimelineFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is the configured timeline doc
	if err != nil {
		return ""
	}
	return string(data)
}

// --- small slog helpers ---------------------------------------------------
//
// The logging package's Info/Warn/etc accept ...any so callers can pass
// slog.Attr values directly. We wrap the most common attributes in tiny
// helpers to keep call-sites readable without sprinkling slog.String
// throughout the loop body.

func slogString(k, v string) any { return slog.String(k, v) }

func slogBool(k string, v bool) any { return slog.Bool(k, v) }

func sRun(runID string) any { return slog.String("run_id", runID) }

func sAgent(agent string) any { return slog.String("agent", agent) }

func sTurn(turn int) any { return slog.Int("turn", turn) }

func sRound(round int) any { return slog.Int("round", round) }

func sErr(err error) any {
	if err == nil {
		return slog.String("err", "")
	}
	return slog.String("err", err.Error())
}
