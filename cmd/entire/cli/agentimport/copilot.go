package agentimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// copilotImporter imports Copilot CLI transcripts. Copilot stores sessions
// flat (~/.copilot/session-state/<id>/events.jsonl), not per-repo, so Discover
// reads each session's session.start event and keeps only those whose
// cwd/gitRoot is the repo root or a descendant.
type copilotImporter struct{}

func (copilotImporter) Name() string { return string(agent.AgentNameCopilotCLI) }

func (copilotImporter) AgentType() types.AgentType { return agent.AgentTypeCopilotCLI }

// Discover returns Copilot session transcripts belonging to this repo (by the
// session.start context) modified within the lookback window. The session ID is
// the session-state subdirectory name.
func (copilotImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir := overridePath
	if dir == "" {
		ag := &copilotcli.CopilotCLIAgent{}
		d, err := ag.GetSessionDir(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve copilot session dir: %w", err)
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read copilot session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessionID := e.Name()
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, sessionID) {
			continue
		}
		path := filepath.Join(dir, sessionID, "events.jsonl")
		info, statErr := os.Stat(path)
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		if !copilotSessionInRepo(path, repoRoot) {
			continue
		}
		out = append(out, SessionFile{Path: path, SessionID: sessionID})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// copilotSessionInRepo reports whether the session's session.start event places
// it in this repo (gitRoot or cwd is the repo root or a descendant). Sessions
// whose location can't be determined are treated as not belonging to the repo.
func copilotSessionInRepo(path, repoRoot string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path discovered under the configured session dir
	if err != nil {
		return false
	}
	for _, raw := range splitRawLines(data) {
		var evt struct {
			Type string `json:"type"`
			Data struct {
				Context struct {
					Cwd     string `json:"cwd"`
					GitRoot string `json:"gitRoot"`
				} `json:"context"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &evt); err != nil || evt.Type != "session.start" {
			continue
		}
		return repoMatches(evt.Data.Context.GitRoot, repoRoot) || repoMatches(evt.Data.Context.Cwd, repoRoot)
	}
	return false
}

// SplitTurns produces one Turn per user.message event, bounded by the next.
// Token usage is delegated to the Copilot agent (per-turn slices sum
// assistant.message outputTokens); the model is read once from the transcript.
func (copilotImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	rawLines := splitRawLines(full)
	var starts []int
	for i, raw := range rawLines {
		if _, ok := copilotPromptText(raw); ok {
			starts = append(starts, i)
		}
	}

	ag := &copilotcli.CopilotCLIAgent{}
	model := copilotcli.ExtractModelFromTranscript(context.Background(), sf.Path)
	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}

		truncated := joinLines(rawLines[:end])
		tokens, err := ag.CalculateTokenUsage(truncated, start)
		if err != nil {
			return nil, fmt.Errorf("token usage for turn %d: %w", k, err)
		}

		var evt struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(rawLines[start], &evt); err != nil {
			continue
		}
		ts, parseErr := time.Parse(time.RFC3339, evt.Timestamp)
		if parseErr != nil {
			ts = time.Time{}
		}
		prompt, _ := copilotPromptText(rawLines[start])

		turns = append(turns, Turn{
			LineStart: start,
			LineEnd:   end,
			UUID:      evt.ID,
			Prompt:    prompt,
			Model:     model,
			CreatedAt: ts,
			Tokens:    tokens,
		})
	}
	return turns, nil
}

// copilotPromptText reports whether a raw events.jsonl line is a user.message
// and returns its content. Other event types return false.
func copilotPromptText(raw []byte) (string, bool) {
	var evt struct {
		Type string `json:"type"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil || evt.Type != "user.message" {
		return "", false
	}
	if evt.Data.Content == "" {
		return "", false
	}
	return evt.Data.Content, true
}
