// Package review — see env.go for package-level rationale.
//
// run.go implements the single-agent orchestrator. CU8 generalizes
// to N agents with a sibling RunMulti that re-uses the same
// AgentRun classification and Sink fan-out.
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

type reviewerRunMetadata interface {
	ActualAgentName() string
	ModelName() string
}

func reviewerActualAgentName(r reviewtypes.AgentReviewer) string {
	if meta, ok := r.(reviewerRunMetadata); ok && meta.ActualAgentName() != "" {
		return meta.ActualAgentName()
	}
	return r.Name()
}

func reviewerModelName(r reviewtypes.AgentReviewer) string {
	if meta, ok := r.(reviewerRunMetadata); ok {
		return meta.ModelName()
	}
	return ""
}

// defaultInspectorTimeout bounds a single inspector's run when the caller
// doesn't set RunConfig.InspectorTimeout. A stuck agent is cancelled (its
// process killed) and marked failed rather than hanging the review forever.
const defaultInspectorTimeout = 10 * time.Minute

// inspectorTimeout resolves the effective per-inspector timeout, distinguishing
// the three RunConfig.InspectorTimeout states the zero value alone can't:
//   - positive: use it.
//   - zero (unset): use defaultInspectorTimeout.
//   - negative: disabled — return 0, and callers treat 0 as "no timeout".
func inspectorTimeout(cfg reviewtypes.RunConfig) time.Duration {
	switch {
	case cfg.InspectorTimeout > 0:
		return cfg.InspectorTimeout
	case cfg.InspectorTimeout < 0:
		return 0
	default:
		return defaultInspectorTimeout
	}
}

func inspectorDeadlineFired(parentCtx, agentCtx context.Context, waitErr error) bool {
	if waitErr == nil {
		return false
	}
	agentDeadline, ok := agentCtx.Deadline()
	if !ok {
		return false
	}
	// Only an agent deadline that is strictly earlier than the parent deadline
	// (or no parent deadline at all) proves this was the per-inspector timeout.
	// Equal deadlines mean the child may have inherited the parent's deadline, so
	// parent cancellation/timeout must not be reported as an inspector timeout.
	if parentDeadline, parentHasDeadline := parentCtx.Deadline(); parentHasDeadline && !agentDeadline.Before(parentDeadline) {
		return false
	}
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return true
	}
	// Fallback only for adapters that formatted ctx.Err() without %w (for
	// example "agent failed: context deadline exceeded"). Do not classify an
	// unrelated non-nil Wait error as a timeout just because the inspector
	// deadline fired while/after Wait was returning.
	if !strings.Contains(waitErr.Error(), context.DeadlineExceeded.Error()) {
		return false
	}
	return errors.Is(agentCtx.Err(), context.DeadlineExceeded)
}

// timedOutError reports the per-inspector timeout as a user-facing error.
func timedOutError(agent string, timeout time.Duration) error {
	return fmt.Errorf("review agent %s timed out after %s", agent, timeout)
}

// Run executes a single-agent review. Events from the agent are forwarded
// to all sinks via AgentEvent as they arrive; on completion, RunFinished
// is called on each sink with the populated RunSummary.
//
// Returns the summary plus the agent's terminal error (nil on clean exit,
// ctx.Err() on cancellation, an error wrapping *exec.ExitError on non-zero
// exit, or an agent-level error when the event stream reports failure).
// Callers can inspect both: the summary is always populated even on error.
func Run(
	ctx context.Context,
	reviewer reviewtypes.AgentReviewer,
	cfg reviewtypes.RunConfig,
	sinks []reviewtypes.Sink,
) (reviewtypes.RunSummary, error) {
	started := time.Now()
	displayName := reviewer.Name()
	agentName := reviewerActualAgentName(reviewer)
	modelName := reviewerModelName(reviewer)
	if modelName == "" {
		modelName = cfg.Model
	}

	// Bound the inspector so a stuck agent can't hang the review forever (unless
	// the timeout is disabled). The deadline applies only to this agent;
	// cancellation kills its process. defer runs at function return (after
	// proc.Wait below), so agentCtx stays live for the whole run; deferring here
	// — not after Start — also releases the timer on the Start-error path.
	timeout := inspectorTimeout(cfg)
	agentCtx := ctx
	var cancelAgent context.CancelFunc = func() {}
	if timeout > 0 {
		agentCtx, cancelAgent = context.WithTimeout(ctx, timeout)
	}
	defer cancelAgent()

	proc, err := reviewer.Start(agentCtx, cfg)
	if err != nil {
		// Construction failed — classify (cancellation vs failure), fan out, return.
		// No event-stream signals available since Start failed before producing any.
		finished := time.Now()
		status := classifyStatus(ctx, err, eventOutcome{})
		summary := reviewtypes.RunSummary{
			StartedAt:  started,
			FinishedAt: finished,
			Cancelled:  status == reviewtypes.AgentStatusCancelled,
			AgentRuns: []reviewtypes.AgentRun{{
				Name:      displayName,
				AgentName: agentName,
				Model:     modelName,
				Status:    status,
				Err:       err,
				StartedAt: started,
				Duration:  finished.Sub(started),
			}},
		}
		for _, sink := range sinks {
			sink.RunFinished(summary)
		}
		return summary, err //nolint:wrapcheck // interface-boundary passthrough; wrapping breaks classifyStatus's ctx.Err() identity check for cancelled-during-Start scenarios
	}

	var (
		buffer       []reviewtypes.Event
		tokens       reviewtypes.Tokens
		finishedSeen bool // saw Finished{...} event from the parser
		finishedOk   bool // its Success field
		sawRunError  bool // saw at least one RunError event
		firstRunErr  error
	)
	for ev := range proc.Events() {
		buffer = append(buffer, ev)
		switch e := ev.(type) {
		case reviewtypes.Tokens:
			tokens = e // Tokens is cumulative per CU2 contract — overwrite.
		case reviewtypes.Finished:
			finishedSeen = true
			finishedOk = e.Success
		case reviewtypes.RunError:
			sawRunError = true
			if firstRunErr == nil {
				firstRunErr = e.Err
			}
		}
		for _, sink := range sinks {
			sink.AgentEvent(displayName, ev)
		}
	}

	waitErr := proc.Wait()
	finished := time.Now()
	// Classify from waitErr, the termination cause captured when Wait returned:
	// the Process contract returns DeadlineExceeded when the process was killed
	// by this inspector's deadline and Canceled on a parent cancellation (user
	// Ctrl+C). If an implementation formats ctx.Err() without preserving the
	// sentinel, fall back to the per-agent context only when Wait returned an
	// error; this avoids a deadline firing after a natural completion (waitErr ==
	// nil) and producing a false timeout.
	timedOut := inspectorDeadlineFired(ctx, agentCtx, waitErr)
	if shouldEmitSyntheticRunError(ctx, waitErr) {
		synthEvent := reviewtypes.RunError{Err: waitErr}
		buffer = append(buffer, synthEvent)
		sawRunError = true
		if firstRunErr == nil {
			firstRunErr = waitErr
		}
		for _, sink := range sinks {
			sink.AgentEvent(displayName, synthEvent)
		}
	}
	status := classifyStatus(ctx, waitErr, eventOutcome{finishedSeen: finishedSeen, finishedOk: finishedOk, sawRunError: sawRunError})
	runErr := waitErr
	switch {
	case timedOut:
		status = reviewtypes.AgentStatusFailed
		runErr = timedOutError(displayName, timeout)
	case runErr == nil && status == reviewtypes.AgentStatusFailed:
		runErr = agentRunFailureError(displayName, firstRunErr)
	}

	summary := reviewtypes.RunSummary{
		StartedAt:  started,
		FinishedAt: finished,
		Cancelled:  status == reviewtypes.AgentStatusCancelled,
		AgentRuns: []reviewtypes.AgentRun{{
			Name:      displayName,
			AgentName: agentName,
			Model:     modelName,
			Status:    status,
			Tokens:    tokens,
			Buffer:    buffer,
			StartedAt: started,
			Duration:  finished.Sub(started),
			Err:       runErr,
		}},
	}
	summary = enrichRunSummary(ctx, cfg, summary)
	for _, sink := range sinks {
		sink.RunFinished(summary)
	}
	return summary, runErr
}

func enrichRunSummary(ctx context.Context, cfg reviewtypes.RunConfig, summary reviewtypes.RunSummary) reviewtypes.RunSummary {
	if cfg.EnrichSummary == nil {
		return summary
	}
	return cfg.EnrichSummary(ctx, summary)
}

func shouldEmitSyntheticRunError(ctx context.Context, waitErr error) bool {
	if waitErr == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
		return false
	}
	return true
}

func agentRunFailureError(agent string, cause error) error {
	if cause != nil {
		return fmt.Errorf("review agent %s reported failure: %w", agent, cause)
	}
	return fmt.Errorf("review agent %s reported failure", agent)
}

// eventOutcome summarises agent-level signals observed in the event stream.
// Used by classifyStatus to downgrade a process-level "exit 0" run that the
// parser flagged as torn (RunError + Finished{Success: false}) — a clean
// process exit doesn't imply a clean review result.
type eventOutcome struct {
	finishedSeen bool // a Finished event was emitted (parser ran to completion)
	finishedOk   bool // Finished.Success
	sawRunError  bool // at least one RunError event was emitted
}

// classifyStatus maps a process Wait error AND the event-stream outcome to
// a terminal AgentStatus.
//
// Order matters: ctx-cancelled + signal-killed delivers an *exec.ExitError
// with exit code -1, which would otherwise be misclassified as Failed.
// Check ctx.Err() FIRST so cancelled runs are reported as Cancelled.
// (Reference: PR #1018 commit 8e82e407a.)
//
// Event-level signals override process-level success: if the parser saw a
// RunError or emitted Finished{Success: false}, the run is Failed even when
// the process exits 0. This catches torn stdout streams the parser surfaces
// via RunError + Finished{Success: false} (CU3 fix-loop guarantee).
func classifyStatus(ctx context.Context, waitErr error, outcome eventOutcome) reviewtypes.AgentStatus {
	if ctx.Err() != nil {
		return reviewtypes.AgentStatusCancelled
	}
	if waitErr != nil {
		return reviewtypes.AgentStatusFailed
	}
	// Process exited cleanly (exit 0). Honor agent-level signals.
	if outcome.sawRunError {
		return reviewtypes.AgentStatusFailed
	}
	if outcome.finishedSeen && !outcome.finishedOk {
		return reviewtypes.AgentStatusFailed
	}
	return reviewtypes.AgentStatusSucceeded
}
