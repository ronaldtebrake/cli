// show.go implements `entire investigate show [run-id]`: prints the
// header summary and findings body for a saved investigation manifest.
//
// This is the user-facing way to read a run's findings after the
// per-run directory has been auto-cleaned on Quorum/Stalled (see R3 in
// cmd.go:writeRunManifest). For paused/cancelled runs the manifest's
// FindingsContent is empty, so we fall back to reading FindingsDoc
// from disk — the per-run dir is still there for those resumable runs.
package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ShowInput drives RunShow.
type ShowInput struct {
	// RunID is the run id (or run-id prefix) to display. Empty means
	// "show the only manifest, or list options if more than one exists".
	RunID string
	// Out is the destination writer for the rendered summary + findings.
	Out io.Writer
	// ErrOut is the destination writer for user-facing error/help messages.
	ErrOut io.Writer
}

// ShowDeps collects what RunShow needs that's test-injectable.
type ShowDeps struct {
	ManifestStore *LocalManifestStore
}

// RunShow prints the saved investigation summary + findings for the
// requested run id. Resolution rules:
//   - empty RunID + exactly one manifest → use that manifest
//   - empty RunID + multiple manifests   → list candidates, return error
//   - non-empty RunID: exact match wins; otherwise unique-prefix match;
//     otherwise return an "ambiguous" or "not found" error
//
// Findings come from manifest.FindingsContent when present (terminal
// outcomes), or by reading manifest.FindingsDoc from disk (paused /
// cancelled runs whose per-run dir still exists). Both paths missing
// is a soft state — the header is printed with an explanatory line.
func RunShow(ctx context.Context, in ShowInput, deps ShowDeps) error {
	if deps.ManifestStore == nil {
		return errors.New("show: manifest store not wired")
	}
	manifests, err := deps.ManifestStore.List(ctx)
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	if len(manifests) == 0 {
		fmt.Fprintln(in.Out, "No local investigations found.")
		return nil
	}

	m, resolveErr := resolveShowTarget(manifests, in.RunID)
	if resolveErr != nil {
		return resolveErr
	}
	printShowSummary(in.Out, m)
	printShowFindings(in.Out, m)
	return nil
}

// resolveShowTarget picks the manifest matching runID using exact match
// first, then unique-prefix match. Errors are user-facing strings so
// they render usefully via wrapSilent at the cobra layer.
func resolveShowTarget(manifests []LocalManifest, runID string) (LocalManifest, error) {
	if strings.TrimSpace(runID) == "" {
		if len(manifests) == 1 {
			return manifests[0], nil
		}
		return LocalManifest{}, ambiguousShowError(manifests, "")
	}
	// Exact match first.
	for _, m := range manifests {
		if m.RunID == runID {
			return m, nil
		}
	}
	// Prefix match.
	var matches []LocalManifest
	for _, m := range manifests {
		if strings.HasPrefix(m.RunID, runID) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return LocalManifest{}, fmt.Errorf("no investigation found with run id or prefix %q", runID)
	}
	return LocalManifest{}, ambiguousShowError(matches, runID)
}

// ambiguousShowError formats a list of candidate run ids for the user
// to choose from when their input matched 0 or 2+ manifests.
func ambiguousShowError(candidates []LocalManifest, runID string) error {
	sorted := append([]LocalManifest(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].StartedAt.After(sorted[j].StartedAt)
	})
	var b strings.Builder
	if runID == "" {
		b.WriteString("multiple investigations available — pass a run id (or unique prefix):\n")
	} else {
		fmt.Fprintf(&b, "ambiguous run id prefix %q matches multiple investigations:\n", runID)
	}
	for _, m := range sorted {
		fmt.Fprintf(&b, "  %s  %s\n", m.RunID, m.Topic)
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// printShowSummary writes the header block (topic, agents, outcome,
// timestamps, stances per agent) to w. Keeps the format compact and
// stable so users can grep its output.
func printShowSummary(w io.Writer, m LocalManifest) {
	fmt.Fprintf(w, "Investigation %s\n", m.RunID)
	if m.Topic != "" {
		fmt.Fprintf(w, "Topic:    %s\n", m.Topic)
	}
	if len(m.Agents) > 0 {
		fmt.Fprintf(w, "Agents:   %s\n", strings.Join(m.Agents, ", "))
	}
	if m.Outcome != "" {
		fmt.Fprintf(w, "Outcome:  %s\n", m.Outcome)
	}
	if !m.StartedAt.IsZero() {
		fmt.Fprintf(w, "Started:  %s\n", m.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if !m.EndedAt.IsZero() {
		fmt.Fprintf(w, "Ended:    %s\n", m.EndedAt.UTC().Format("2006-01-02 15:04:05Z"))
	}
	if len(m.StancesByAgent) > 0 {
		keys := make([]string, 0, len(m.StancesByAgent))
		for k := range m.StancesByAgent {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Last stance per agent:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s: %s\n", k, m.StancesByAgent[k])
		}
	}
	fmt.Fprintln(w)
}

// printShowFindings writes the findings content to w. Prefers the
// manifest's embedded content (set on terminal outcomes); falls back
// to reading the on-disk findings file (still present for paused or
// cancelled runs). Prints a soft "no content" notice when neither is
// available.
func printShowFindings(w io.Writer, m LocalManifest) {
	if m.FindingsContent != "" {
		fmt.Fprintln(w, "--- Findings ---")
		fmt.Fprintln(w)
		fmt.Fprint(w, m.FindingsContent)
		if !strings.HasSuffix(m.FindingsContent, "\n") {
			fmt.Fprintln(w)
		}
		return
	}
	if m.FindingsDoc != "" {
		data, err := os.ReadFile(m.FindingsDoc)
		if err == nil {
			fmt.Fprintln(w, "--- Findings ---")
			fmt.Fprintln(w)
			fmt.Fprint(w, string(data))
			if !strings.HasSuffix(string(data), "\n") {
				fmt.Fprintln(w)
			}
			return
		}
	}
	fmt.Fprintf(w, "No findings content available for run %s.\n", m.RunID)
}
