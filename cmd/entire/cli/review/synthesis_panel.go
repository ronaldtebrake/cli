package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// PanelSynthesisProvider runs a panel of judges over the inspectors' reports.
// Each judge independently produces a verdict from the same synthesis prompt;
// when two or more verdicts come back, the chair judge merges them into one
// final verdict and the individual verdicts are appended as a panel.
//
// It implements SynthesisProvider, so the existing SynthesisSink uses it
// unchanged — a panel is just a provider that happens to consult several
// judges. A single judge collapses to that judge's verdict (today's behavior).
type PanelSynthesisProvider struct {
	Judges   []SynthesisProvider // one per judge; index aligns with Labels
	Labels   []string            // display label per judge (e.g. "codex · gpt-5")
	ChairIdx int                 // index of the judge that merges the panel
}

// Synthesize fans out to each judge in parallel, then has the chair merge the
// verdicts. Failed judges are dropped; if only one verdict survives it is
// returned directly. If every judge fails, the error is returned so the caller
// can report "final report unavailable".
func (p PanelSynthesisProvider) Synthesize(ctx context.Context, prompt string) (string, error) {
	switch len(p.Judges) {
	case 0:
		return "", errors.New("no judges configured")
	case 1:
		return p.Judges[0].Synthesize(ctx, prompt) //nolint:wrapcheck // transparent single-judge passthrough
	}

	verdicts := make([]string, len(p.Judges))
	errs := make([]error, len(p.Judges))
	var wg sync.WaitGroup
	for i := range p.Judges {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			verdicts[i], errs[i] = p.Judges[i].Synthesize(ctx, prompt)
		}(i)
	}
	wg.Wait()

	ok := make([]int, 0, len(p.Judges))
	for i := range verdicts {
		if errs[i] == nil && strings.TrimSpace(verdicts[i]) != "" {
			ok = append(ok, i)
		}
	}
	switch len(ok) {
	case 0:
		return "", fmt.Errorf("all judges failed: %w", firstNonNilErr(errs))
	case 1:
		return verdicts[ok[0]], nil
	}

	// Pick the chair; fall back to the first successful judge if the configured
	// chair failed or is out of range. The chair participates as a full panel
	// judge (it produced its own verdict above) and then runs a second time to
	// merge the panel — these are deliberately distinct calls: an independent
	// verdict, then a reconciliation over all verdicts.
	chair := p.ChairIdx
	if chair < 0 || chair >= len(p.Judges) || errs[chair] != nil || strings.TrimSpace(verdicts[chair]) == "" {
		chair = ok[0]
	}

	final, err := p.Judges[chair].Synthesize(ctx, composeChairPrompt(verdicts, p.Labels, ok))
	if err != nil || strings.TrimSpace(final) == "" {
		// Chair merge failed: surface the panel rather than nothing.
		final = "The judges could not be merged into a single verdict; see each judge's verdict below."
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(final))
	b.WriteString("\n\n## Panel\n\n")
	for _, i := range ok {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", p.labelAt(i), strings.TrimSpace(verdicts[i]))
	}
	return b.String(), nil
}

func (p PanelSynthesisProvider) labelAt(i int) string {
	if i >= 0 && i < len(p.Labels) && strings.TrimSpace(p.Labels[i]) != "" {
		return p.Labels[i]
	}
	return fmt.Sprintf("judge %d", i+1)
}

// composeChairPrompt instructs the chair judge to reconcile the panel's
// verdicts into one final verdict, explicitly surfacing agreement and dissent.
func composeChairPrompt(verdicts, labels []string, ok []int) string {
	var b strings.Builder
	b.WriteString("You are the presiding judge on a panel reviewing a code change. " +
		"The judges' verdicts are below. Write the single final verdict, nothing else:\n" +
		"  - One line: the decision and a one-sentence reason.\n" +
		"  - Then a short bullet list of the agreed actionable findings, most important first, one line each. " +
		"Note a disagreement only when it changes the decision, and resolve it (which judge is right and why).\n\n" +
		"Merge duplicates, drop unsupported claims, don't concatenate or repeat the verdicts. " +
		"No preamble or headings; be proportional to the change.\n\n")
	for _, i := range ok {
		label := fmt.Sprintf("judge %d", i+1)
		if i < len(labels) && strings.TrimSpace(labels[i]) != "" {
			label = labels[i]
		}
		fmt.Fprintf(&b, "## Verdict from %s\n\n%s\n\n", label, strings.TrimSpace(verdicts[i]))
	}
	return b.String()
}

func firstNonNilErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return errors.New("unknown synthesis error")
}

// Compile-time check.
var _ SynthesisProvider = PanelSynthesisProvider{}
