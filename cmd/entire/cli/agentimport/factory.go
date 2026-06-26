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
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// factoryImporter imports Factory AI Droid transcripts
// (~/.factory/sessions/<repo>/<id>.jsonl). Droid wraps each message in an
// envelope ({"type":"message","id":..,"message":{...}}); token usage is
// subagent-aware and the model lives in an adjacent <id>.settings.json.
type factoryImporter struct{}

func (factoryImporter) Name() string { return string(agent.AgentNameFactoryAIDroid) }

func (factoryImporter) AgentType() types.AgentType { return agent.AgentTypeFactoryAIDroid }

// Discover returns Factory transcript files for the repo modified within the
// lookback window.
func (factoryImporter) Discover(repoRoot, overridePath string, now time.Time, sessionFilter []string) ([]SessionFile, error) {
	dir := overridePath
	if dir == "" {
		ag := &factoryaidroid.FactoryAIDroidAgent{}
		d, err := ag.GetSessionDir(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve factory session dir: %w", err)
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read factory session dir: %w", err)
	}
	cutoff := now.AddDate(0, 0, -LookbackDays)
	var out []SessionFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".jsonl")
		if len(sessionFilter) > 0 && !slices.Contains(sessionFilter, stem) {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil || info.ModTime().Before(cutoff) {
			continue
		}
		out = append(out, SessionFile{Path: filepath.Join(dir, e.Name()), SessionID: stem})
	}
	slices.SortFunc(out, func(a, b SessionFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// SplitTurns produces one Turn per user-prompt envelope, bounded by the next.
// Token usage (including spawned subagents) is delegated to the Factory agent;
// the model is read once from the session's adjacent settings file.
func (factoryImporter) SplitTurns(sf SessionFile, full []byte) ([]Turn, error) {
	rawLines := splitRawLines(full)
	var starts []int
	for i, raw := range rawLines {
		if _, ok := factoryPromptText(raw); ok {
			starts = append(starts, i)
		}
	}

	subagentsDir := filepath.Join(filepath.Dir(sf.Path), sf.SessionID, "subagents")
	model := factoryaidroid.ExtractModelFromTranscript(sf.Path)
	ag := &factoryaidroid.FactoryAIDroidAgent{}
	turns := make([]Turn, 0, len(starts))
	for k, start := range starts {
		end := len(rawLines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}

		truncated := joinLines(rawLines[:end])
		tokens, err := ag.CalculateTotalTokenUsage(truncated, start, subagentsDir)
		if err != nil {
			return nil, fmt.Errorf("token usage for turn %d: %w", k, err)
		}

		var env struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rawLines[start], &env); err != nil {
			continue
		}
		prompt, _ := factoryPromptText(rawLines[start])

		turns = append(turns, Turn{
			LineStart: start,
			LineEnd:   end,
			UUID:      env.ID,
			Prompt:    prompt,
			Model:     model,
			Tokens:    tokens,
		})
	}
	return turns, nil
}

// factoryPromptText reports whether a raw Droid JSONL line is a user-prompt
// message and returns its text. Droid tags the role inside the inner message;
// tool_result user messages carry no extractable text and return false.
func factoryPromptText(raw []byte) (string, bool) {
	var env struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != "message" {
		return "", false
	}
	var role struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(env.Message, &role); err != nil || role.Role != transcript.TypeUser {
		return "", false
	}
	text := transcript.ExtractUserContent(env.Message)
	if text == "" {
		return "", false
	}
	return text, true
}
