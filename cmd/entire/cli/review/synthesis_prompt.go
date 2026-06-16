// Package review — see env.go for package-level rationale.
//
// synthesis_prompt.go builds the LLM prompt that asks a configured summary
// provider to synthesize a unified verdict across N per-agent review reports.
// It is a pure-function composer with no I/O; all logic is testable without
// network calls or TTY state.
package review

import (
	"fmt"
	"strings"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// composeSynthesisPrompt builds the LLM prompt asking the single judge to
// consolidate the N inspector reports into one verdict. Format:
//
//	You are the judge for this code review. N inspectors independently
//	reviewed the same change.
//
//	Inspector reports:
//
//	─── claude-code ───
//	<narrative from the inspector's AssistantText events, joined>
//
//	─── codex ───
//	<narrative>
//
//	...
//
//	<instructions to output only a one-line verdict plus a short bullet list
//	of actionable findings — no headings, preamble, or filler>
//
//	<perRunPrompt, if any — appended as user's per-run instructions>
//
// The instructions deliberately do not mandate a multi-section template:
// forcing fixed headers produced padded "none" filler on small/clean changes.
// The judge writes a verdict and only the findings that matter, proportional
// to the change.
//
// Inspectors with no usable narrative (empty AssistantText) are filtered out
// upstream by usableAgentRuns, so the header count and the body are both
// scoped to inspectors that produced narrative output. SynthesisSink already
// guards on len(usable) >= 2 before calling, so the empty case won't reach
// the LLM in production.
func composeSynthesisPrompt(summary reviewtypes.RunSummary, perRunPrompt string, profileName string, task string) string {
	usable := usableAgentRuns(summary)
	if len(usable) == 0 {
		return ""
	}

	var b strings.Builder

	fmt.Fprintf(&b, "You are the judge for this code review. %d inspectors independently reviewed the same change.\n", len(usable))
	if profileName != "" {
		fmt.Fprintf(&b, "Review profile: %s\n", profileName)
	}
	if strings.TrimSpace(task) != "" {
		fmt.Fprintf(&b, "Canonical task: %s\n", strings.TrimSpace(task))
	}
	b.WriteString("\nInspector reports:\n")

	for _, run := range usable {
		narrative := joinAssistantText(run.Buffer)
		if narrative == "" {
			continue
		}
		fmt.Fprintf(&b, "\n─── %s ───\n", run.Name)
		b.WriteString(narrative)
		b.WriteString("\n")
	}

	b.WriteString(`
Consolidate the inspector reports into one verdict — judge critically, don't just summarize.
  - Keep only findings backed by concrete evidence (file, function, behavior, test, or diff detail).
  - Drop unsupported or speculative claims. Merge duplicates. Resolve contradictions on the merits.

Output exactly this, nothing else:
  - One line: the verdict (approve / approve with nits / request changes) and a one-sentence reason.
  - Then a short bullet list of actionable findings, most important first, one line each with a file/symbol pointer. Omit the list entirely when nothing is actionable.

No preamble, no section headings, no restating the diff or task, no filler. Be proportional: a clean change is a single line.`)

	if perRunPrompt != "" {
		b.WriteString("\n\nPer-run user instructions:\n")
		b.WriteString(perRunPrompt)
	}

	return b.String()
}

// usableAgentRuns returns agent runs that have non-empty AssistantText
// narrative in their event buffer, in the original order from the summary.
// The filter is on narrative content alone — Status is not checked. In
// practice this drops most cancelled and errored runs (they typically don't
// produce assistant output before exiting), but a cancelled agent that
// emitted text mid-stream is still considered usable. The synthesis prompt
// uses what the agent actually said, regardless of how the run terminated.
func usableAgentRuns(summary reviewtypes.RunSummary) []reviewtypes.AgentRun {
	var result []reviewtypes.AgentRun
	for _, run := range summary.AgentRuns {
		if joinAssistantText(run.Buffer) == "" {
			continue
		}
		result = append(result, run)
	}
	return result
}
