package agentimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi/pijsonl"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// piImporter imports Pi transcripts (~/.pi/agent/sessions/<repo>/<ts>_<uuid>.jsonl).
// Pi records token usage and the model on every assistant message, so imported
// turns carry both via the agent's own CalculateTokenUsage / ExtractModel.
type piImporter struct{}

func (piImporter) Name() string { return string(agent.AgentNamePi) }

func (piImporter) AgentType() types.AgentType { return agent.AgentTypePi }

// Discover returns Pi transcript files for the repo modified within the lookback
// window. The session ID is the <uuid> suffix of the <timestamp>_<uuid> file
// stem (Pi timestamps use dashes, so the first underscore is the separator).
func (piImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir := overridePath
	if dir == "" {
		ag := &pi.PiAgent{}
		d, err := ag.GetSessionDir(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve pi session dir: %w", err)
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pi session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".jsonl")
		sessionID := piSessionID(stem)
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, sessionID) {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, SessionFile{Path: filepath.Join(dir, e.Name()), SessionID: sessionID})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// piSessionID extracts the <uuid> portion of a "<timestamp>_<uuid>" file stem.
// Falls back to the whole stem when there is no underscore separator.
func piSessionID(stem string) string {
	if i := strings.Index(stem, "_"); i >= 0 {
		return stem[i+1:]
	}
	return stem
}

// SplitTurns produces one Turn per user-prompt message line, bounded by the
// next. Token usage and model are delegated to the Pi agent so import reuses the
// same accounting (branch-aware) the live path uses.
func (piImporter) SplitTurns(_ SessionFile, full []byte) ([]Turn, error) {
	rawLines := splitRawLines(full)
	var starts []int
	for i, raw := range rawLines {
		if _, ok := piPromptText(raw); ok {
			starts = append(starts, i)
		}
	}

	ag := &pi.PiAgent{}
	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}

		// Bound token usage to [start, end): truncate to the first `end` lines,
		// then let the agent helper slice from `start`.
		truncated := joinLines(rawLines[:end])
		tokens, err := ag.CalculateTokenUsage(truncated, start)
		if err != nil {
			return nil, fmt.Errorf("token usage for turn %d: %w", k, err)
		}
		model, mErr := ag.ExtractModel(joinLines(rawLines[start:end]))
		if mErr != nil {
			model = ""
		}

		var entry pijsonl.Entry
		if err := json.Unmarshal(rawLines[start], &entry); err != nil {
			continue
		}
		ts, parseErr := time.Parse(time.RFC3339, entry.Timestamp)
		if parseErr != nil {
			ts = time.Time{}
		}
		prompt, _ := piPromptText(rawLines[start])

		turns = append(turns, Turn{
			LineStart: start,
			LineEnd:   end,
			UUID:      entry.ID,
			Prompt:    prompt,
			Model:     model,
			CreatedAt: ts,
			Tokens:    tokens,
		})
	}
	return turns, nil
}

// piPromptText reports whether a raw Pi JSONL line is a user-prompt message and
// returns its text. Pi user content may be a plain string or an array of typed
// blocks; toolResult/assistant messages and empty content return false.
func piPromptText(raw []byte) (string, bool) {
	var entry pijsonl.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false
	}
	if entry.Type != pijsonl.EntryTypeMessage || entry.Message.Role != pijsonl.RoleUser {
		return "", false
	}
	if s := pijsonl.DecodeStringContent(entry.Message.Content); s != "" {
		return s, true
	}
	var items []pijsonl.ContentItem
	if err := json.Unmarshal(entry.Message.Content, &items); err != nil {
		return "", false
	}
	var texts []string
	for _, it := range items {
		if it.Type == pijsonl.ContentTypeText && it.Text != "" {
			texts = append(texts, it.Text)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n\n"), true
}
