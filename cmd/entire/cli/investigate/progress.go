// Package investigate — see env.go for package-level rationale.
//
// progress.go defines ProgressSink, the consumer-side abstraction the loop
// uses to surface turn lifecycle events to whatever UI is rendering the run.
// Two implementations live in this package: textProgressSink (headless, used
// for non-TTY stdout) and tuiProgressSink (Bubble Tea dashboard, used for
// interactive runs). Tests inject their own fakes.
//
// The sink is the only seam between the loop and the UI; the loop never
// imports bubbletea or writes formatted lines directly. This mirrors the
// review package's Sink pattern.
package investigate

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// ProgressSink consumes turn lifecycle events from RunInvestigateLoop. The
// loop invokes the methods from a single goroutine — implementations need
// not synchronize against themselves.
//
// Implementations MUST NOT block. The loop calls these synchronously around
// the per-turn agent spawn; a slow sink stalls the entire investigation.
type ProgressSink interface {
	// TurnStarted is called immediately before the agent process starts for
	// the given turn. perAgentTurn is the 1-indexed count of turns this
	// agent has taken (this one included); maxPerAgent is the configured
	// per-agent budget.
	TurnStarted(agent string, turn, perAgentTurn, maxPerAgent int)

	// TurnFinished is called once after the agent process exits AND the
	// timeline doc has been parsed for the freshly-added turn block. stance
	// is one of "approve", "request-changes", "abstain", "unknown". duration
	// is the wall-clock duration of the agent process. failed is true when
	// the turn was treated as a failure by the loop (spawn error, missing
	// heading, etc.); err is the underlying error or nil.
	TurnFinished(agent string, turn int, stance string, duration time.Duration, failed bool, err error, preview string)

	// RunFinished is called once when the loop terminates (any outcome).
	// The TUI uses this to flip rows to a terminal status and freeze the
	// dashboard; the text sink may print a final outcome line.
	RunFinished(outcome LoopOutcome)
}

// nullProgressSink is the zero-overhead default: every method is a no-op.
// Used when callers pass LoopDeps.Progress == nil (most tests).
type nullProgressSink struct{}

func (nullProgressSink) TurnStarted(string, int, int, int)                                    {}
func (nullProgressSink) TurnFinished(string, int, string, time.Duration, bool, error, string) {}
func (nullProgressSink) RunFinished(LoopOutcome)                                              {}

// textProgressSink writes today's two-line shape to a plain io.Writer:
//
//	Turn N · <agent>
//	  Stance: <stance>
//
// Used when the terminal cannot render the Bubble Tea TUI (non-TTY stdout,
// CI, agent-host invocations). The mutex guards Writer access because while
// the loop is single-threaded, the sink is also invoked from RunFinished
// after the loop returns — a tiny gap, but cheap to lock.
type textProgressSink struct {
	mu sync.Mutex
	w  io.Writer
}

func newTextProgressSink(w io.Writer) *textProgressSink {
	return &textProgressSink{w: w}
}

func (s *textProgressSink) TurnStarted(agent string, turn, _, _ int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return
	}

	_, _ = fmt.Fprintf(s.w, "Turn %d · %s\n", turn, agent)
}

func (s *textProgressSink) TurnFinished(_ string, _ int, stance string, _ time.Duration, _ bool, _ error, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return
	}

	_, _ = fmt.Fprintf(s.w, "  Stance: %s\n", stance)
}

func (s *textProgressSink) RunFinished(_ LoopOutcome) {
	// The text sink emits per-turn lines only; the post-run footer is the
	// caller's responsibility (writeInvestigateFooter in cmd.go). Nothing
	// to do here.
}

// Compile-time interface checks.
var (
	_ ProgressSink = nullProgressSink{}
	_ ProgressSink = (*textProgressSink)(nil)
)
